package tools

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/osrs-ge/ge-mcp/internal/envelope"
)

// QUERIES #21 (validated 2026-07-14). Prices one item_relations row
// end-to-end at the latest quotes: buy legs fill at `low` (your buy offer
// fills against insta-sells), sell legs at `high` minus the GE tax.
//
// Tax note: LEAST(high/50, 5000000) with integer division is the ingest
// formula for prices_1m.margin applied to a SELL LEG — this is not a
// recomputation of the stored single-item margin (which never applies to
// multi-item conversions); the per-leg tax composes into combo_margin.
//
// Nulls are signal: a leg with no traded side has price NULL, and the
// combined margin is then NULL (meta.summary.note says which leg) — never
// zero-filled.
const comboQuoteSQL = `
WITH rel AS (SELECT * FROM item_relations WHERE relation_id = $1),
legs AS (
  SELECT (l->>'item_id')::int AS item_id, (l->>'qty')::bigint AS qty, 'buy' AS side
  FROM rel, jsonb_array_elements(CASE WHEN $2 THEN rel.inputs ELSE rel.outputs END) l
  UNION ALL
  SELECT (l->>'item_id')::int, (l->>'qty')::bigint, 'sell'
  FROM rel, jsonb_array_elements(CASE WHEN $2 THEN rel.outputs ELSE rel.inputs END) l
),
latest1 AS (
  SELECT DISTINCT ON (item_id) item_id, high, high_time, low, low_time
  FROM prices_1m WHERE item_id IN (SELECT item_id FROM legs)
  ORDER BY item_id, ts DESC
),
latest5 AS (
  SELECT DISTINCT ON (item_id) item_id,
         coalesce(high_volume,0)+coalesce(low_volume,0) AS vol5m
  FROM prices_5m WHERE item_id IN (SELECT item_id FROM legs)
  ORDER BY item_id, ts DESC
)
SELECT leg.side, leg.item_id, i.name, leg.qty, i.buy_limit,
       CASE WHEN leg.side='buy' THEN l1.low ELSE l1.high END AS price,
       CASE WHEN leg.side='sell' AND l1.high IS NOT NULL
            THEN LEAST(l1.high/50, 5000000) ELSE 0 END AS tax,
       extract(epoch from now() - CASE WHEN leg.side='buy' THEN l1.low_time ELSE l1.high_time END)::bigint AS age_s,
       l5.vol5m
FROM legs leg
JOIN items i USING (item_id)
LEFT JOIN latest1 l1 USING (item_id)
LEFT JOIN latest5 l5 USING (item_id)
ORDER BY leg.side, leg.item_id`

type comboLegRow struct {
	Side     string `json:"side"`
	ItemID   int    `json:"item_id"`
	Name     string `json:"name"`
	Qty      int64  `json:"qty"`
	BuyLimit *int64 `json:"buy_limit"`
	Price    *int64 `json:"price"`
	Tax      int64  `json:"tax"`
	AgeS     *int64 `json:"age_s"`
	Vol5m    *int64 `json:"vol5m"`
}

func NewComboQuoteTool() mcp.Tool {
	return mcp.NewTool("combo_quote",
		mcp.WithDescription("Price one relation end-to-end at the latest quotes (archetype C's falsification primitive): buy legs at `low`, sell legs at `high` minus GE tax (2%, capped 5M, per unit). meta.summary carries input_cost, output_revenue_post_tax, combo_margin (per conversion), roi_pct, max_leg_age_s (worst-leg freshness — a stale leg voids the quote), min-leg volume, and units_bound = the conversions/4h the tightest buy-limit leg allows. A leg with no traded side has price null and combo_margin is null — that leg is untradeable right now, not free. Get relation_ids from list_relations; direction=reverse only for reversible relations."),
		mcp.WithNumber("relation_id", mcp.Required(), mcp.Description("From list_relations")),
		mcp.WithString("direction", mcp.Enum("forward", "reverse"), mcp.Description("forward = buy inputs, sell outputs (default); reverse only if the relation is reversible")),
	)
}

func ComboQuoteHandler(pool *pgxpool.Pool) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		relationID := req.GetInt("relation_id", 0)
		if relationID <= 0 {
			return mcp.NewToolResultError(envelope.ErrorJSON("bad_param", "relation_id is required and must be positive")), nil
		}
		direction := req.GetString("direction", "forward")
		if direction != "forward" && direction != "reverse" {
			return mcp.NewToolResultError(envelope.ErrorJSON("bad_param", "direction must be forward or reverse")), nil
		}

		var relName, relKind, relNotes string
		var reversible bool
		err := pool.QueryRow(ctx,
			`SELECT name, kind, coalesce(notes,''), reversible FROM item_relations WHERE relation_id = $1`,
			relationID).Scan(&relName, &relKind, &relNotes, &reversible)
		if err == pgx.ErrNoRows {
			return mcp.NewToolResultError(envelope.ErrorJSON("relation_not_found", fmt.Sprintf("no relation with id %d (see list_relations)", relationID))), nil
		}
		if err != nil {
			if errResult := relationsUnavailable(err); errResult != nil {
				return errResult, nil
			}
			return nil, err
		}
		if direction == "reverse" && !reversible {
			return mcp.NewToolResultError(envelope.ErrorJSON("not_reversible", relName+" is one-way (see its notes)")), nil
		}

		rows, err := pool.Query(ctx, comboQuoteSQL, relationID, direction == "forward")
		if err != nil {
			return nil, err
		}
		defer rows.Close()

		out := []comboLegRow{}
		for rows.Next() {
			var r comboLegRow
			if err := rows.Scan(&r.Side, &r.ItemID, &r.Name, &r.Qty, &r.BuyLimit,
				&r.Price, &r.Tax, &r.AgeS, &r.Vol5m); err != nil {
				return nil, err
			}
			out = append(out, r)
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}

		// Summary math in Go: transparent, and NULL legs stay signal.
		var inputCost, outputRevenue int64
		var missingLeg string
		var maxAge *int64
		var minVol *int64
		var unitsBound *int64
		for _, l := range out {
			if l.Price == nil {
				missingLeg = fmt.Sprintf("%s leg %q has no traded price", l.Side, l.Name)
				continue
			}
			if l.Side == "buy" {
				inputCost += *l.Price * l.Qty
				if l.BuyLimit != nil && *l.BuyLimit > 0 {
					b := *l.BuyLimit / l.Qty
					if unitsBound == nil || b < *unitsBound {
						unitsBound = &b
					}
				}
			} else {
				outputRevenue += (*l.Price - l.Tax) * l.Qty
			}
			if l.AgeS != nil && (maxAge == nil || *l.AgeS > *maxAge) {
				maxAge = l.AgeS
			}
			if l.Vol5m != nil && (minVol == nil || *l.Vol5m < *minVol) {
				minVol = l.Vol5m
			}
		}

		summary := map[string]any{
			"relation": relName, "kind": relKind, "direction": direction,
			"notes":         relNotes,
			"max_leg_age_s": maxAge, "min_leg_vol5m": minVol,
			"units_bound_per_4h": unitsBound,
		}
		if missingLeg != "" {
			summary["combo_margin"] = nil
			summary["note"] = missingLeg + " — conversion is unpriceable right now (null is signal, not zero)"
		} else {
			summary["input_cost"] = inputCost
			summary["output_revenue_post_tax"] = outputRevenue
			summary["combo_margin"] = outputRevenue - inputCost
			if inputCost > 0 {
				summary["roi_pct"] = float64(outputRevenue-inputCost) / float64(inputCost) * 100
			}
		}

		env := envelope.New(out, len(out))
		env.Meta = map[string]any{"summary": summary}
		return mcp.NewToolResultText(env.JSON()), nil
	}
}
