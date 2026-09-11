package tools

import (
	"context"
	"fmt"
	"sort"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/osrs-ge/ge-mcp/internal/envelope"
)

const maxComboScreenIDs = 50

// QUERIES #21 batched across relations: the same leg-pricing CTEs as
// combo_quote, with relation_id carried through so one pass prices the whole
// C universe. Aggregation stays in Go (shared shape with combo_quote's
// summary math) so NULL legs stay signal per relation instead of poisoning
// the batch.
//
// latest1/latest5 deliberately diverge from combo_quote's DISTINCT ON shape:
// with the whole universe's legs (~300 items) in the IN-list the planner
// abandons per-item index descents and scans entire hypertable chunks (~82s
// on prod, past the 30s statement timeout). LATERAL LIMIT 1 pins one index
// descent per item (~180ms). An item with no rows yields no row here, same
// as DISTINCT ON — the outer LEFT JOIN keeps null legs signal.
const comboScreenSQL = `
WITH rel AS (
  SELECT relation_id, kind, name, reversible FROM item_relations
  WHERE ($1::text IS NULL OR kind = $1)
    AND ($2::int[] IS NULL OR relation_id = ANY($2))
),
legs AS (
  SELECT rel.relation_id, (l->>'item_id')::int AS item_id, (l->>'qty')::bigint AS qty, 'buy' AS side
  FROM rel JOIN item_relations r USING (relation_id), jsonb_array_elements(r.inputs) l
  UNION ALL
  SELECT rel.relation_id, (l->>'item_id')::int, (l->>'qty')::bigint, 'sell'
  FROM rel JOIN item_relations r USING (relation_id), jsonb_array_elements(r.outputs) l
),
latest1 AS (
  SELECT ids.item_id, p.high, p.high_time, p.low, p.low_time
  FROM (SELECT DISTINCT item_id FROM legs) ids
  CROSS JOIN LATERAL (
    SELECT high, high_time, low, low_time
    FROM prices_1m WHERE item_id = ids.item_id
    ORDER BY ts DESC LIMIT 1
  ) p
),
latest5 AS (
  SELECT ids.item_id, coalesce(p.high_volume,0)+coalesce(p.low_volume,0) AS vol5m
  FROM (SELECT DISTINCT item_id FROM legs) ids
  CROSS JOIN LATERAL (
    SELECT high_volume, low_volume
    FROM prices_5m WHERE item_id = ids.item_id
    ORDER BY ts DESC LIMIT 1
  ) p
),
act AS (
  SELECT item_id,
         count(*) FILTER (WHERE coalesce(low_volume,0)  > 0) AS low_ticks_24h,
         count(*) FILTER (WHERE coalesce(high_volume,0) > 0) AS high_ticks_24h
  FROM prices_5m WHERE item_id IN (SELECT item_id FROM legs)
    AND ts > now() - interval '24 hours'
  GROUP BY item_id
)
SELECT leg.relation_id, rel.kind, rel.name, rel.reversible,
       leg.side, leg.qty, i.buy_limit,
       CASE WHEN leg.side='buy' THEN l1.low ELSE l1.high END AS price,
       CASE WHEN leg.side='sell' AND l1.high IS NOT NULL
            THEN LEAST(l1.high/50, 5000000) ELSE 0 END AS tax,
       extract(epoch from now() - CASE WHEN leg.side='buy' THEN l1.low_time ELSE l1.high_time END)::bigint AS age_s,
       l5.vol5m,
       CASE WHEN leg.side='buy' THEN a.low_ticks_24h ELSE a.high_ticks_24h END AS trades_24h
FROM legs leg
JOIN rel USING (relation_id)
JOIN items i USING (item_id)
LEFT JOIN latest1 l1 USING (item_id)
LEFT JOIN latest5 l5 USING (item_id)
LEFT JOIN act a USING (item_id)
ORDER BY leg.relation_id, leg.side`

type comboScreenRow struct {
	RelationID int    `json:"relation_id"`
	Kind       string `json:"kind"`
	Name       string `json:"name"`
	Reversible bool   `json:"reversible"`

	// Forward-direction summary, same math as combo_quote's meta.summary.
	// combo_margin is NULL (with missing_legs > 0) when any leg has no traded
	// price — that relation is unpriceable right now, not margin-zero.
	InputCost            *int64   `json:"input_cost"`
	OutputRevenuePostTax *int64   `json:"output_revenue_post_tax"`
	ComboMargin          *int64   `json:"combo_margin"`
	RoiPct               *float64 `json:"roi_pct"`
	MinLegVol5m          *int64   `json:"min_leg_vol5m"`
	WorstLegAgeRatio     *float64 `json:"worst_leg_age_ratio"`
	UnitsBoundPer4h      *int64   `json:"units_bound_per_4h"`
	MissingLegs          int      `json:"missing_legs"`
}

func NewComboScreenTool() mcp.Tool {
	return mcp.NewTool("combo_screen",
		mcp.WithDescription("Rank the ENTIRE archetype-C universe in ONE call: prices every item_relations row forward (buy inputs at `low`, sell outputs at `high` minus GE tax) and returns one summary row per relation, ranked by combo_margin (nulls last). Row shape matches combo_quote's meta.summary. Use this FIRST for C research — it replaces calling combo_quote once per relation — then deep-dive only the top few with combo_quote (per-leg detail, reverse direction, cadence-relative freshness). combo_margin null + missing_legs > 0 means a leg had no traded side: unpriceable right now, not zero. worst_leg_age_ratio <= ~3 is that leg's normal cadence, not staleness."),
		mcp.WithString("kind", mcp.Enum("decant", "set", "combine"), mcp.Description("Only relations of this kind")),
		mcp.WithArray("relation_ids", mcp.Description("Only these relations (max 50); omit to screen all"), mcp.WithNumberItems()),
		mcp.WithNumber("limit", mcp.Description("Rows returned after ranking (default 25, max 50); meta.screened says how many were priced")),
	)
}

func ComboScreenHandler(pool *pgxpool.Pool) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var kind *string
		if k := req.GetString("kind", ""); k != "" {
			if k != "decant" && k != "set" && k != "combine" {
				return mcp.NewToolResultError(envelope.ErrorJSON("bad_param", "kind must be decant, set or combine")), nil
			}
			kind = &k
		}

		var relationIDs []int
		if raw, present := req.GetArguments()["relation_ids"]; present {
			list, ok := raw.([]any)
			if !ok || len(list) == 0 {
				return mcp.NewToolResultError(envelope.ErrorJSON("bad_param", "relation_ids must be a non-empty array when given")), nil
			}
			if len(list) > maxComboScreenIDs {
				return mcp.NewToolResultError(envelope.ErrorJSON("bad_param", fmt.Sprintf("relation_ids exceeds the batch limit of %d", maxComboScreenIDs))), nil
			}
			for _, v := range list {
				f, ok := v.(float64)
				if !ok || f <= 0 {
					return mcp.NewToolResultError(envelope.ErrorJSON("bad_param", "relation_ids entries must be positive numbers")), nil
				}
				relationIDs = append(relationIDs, int(f))
			}
		}

		limit := req.GetInt("limit", 25)
		if limit < 1 || limit > maxComboScreenIDs {
			return mcp.NewToolResultError(envelope.ErrorJSON("bad_param", fmt.Sprintf("limit must be 1-%d", maxComboScreenIDs))), nil
		}

		rows, err := pool.Query(ctx, comboScreenSQL, kind, relationIDs)
		if err != nil {
			if errResult := relationsUnavailable(err); errResult != nil {
				return errResult, nil
			}
			return nil, err
		}
		defer rows.Close()

		byRelation := map[int]*comboScreenRow{}
		var order []int
		for rows.Next() {
			var relationID int
			var kind, name, side string
			var reversible bool
			var qty, tax int64
			var buyLimit, price, ageS, vol5m, trades24h *int64
			if err := rows.Scan(&relationID, &kind, &name, &reversible,
				&side, &qty, &buyLimit, &price, &tax, &ageS, &vol5m, &trades24h); err != nil {
				return nil, err
			}
			r := byRelation[relationID]
			if r == nil {
				r = &comboScreenRow{RelationID: relationID, Kind: kind, Name: name, Reversible: reversible}
				byRelation[relationID] = r
				order = append(order, relationID)
			}
			if price == nil {
				r.MissingLegs++
				continue
			}
			if side == "buy" {
				cost := *price * qty
				r.InputCost = addInt(r.InputCost, cost)
				if buyLimit != nil && *buyLimit > 0 {
					b := *buyLimit / qty
					if r.UnitsBoundPer4h == nil || b < *r.UnitsBoundPer4h {
						r.UnitsBoundPer4h = &b
					}
				}
			} else {
				rev := (*price - tax) * qty
				r.OutputRevenuePostTax = addInt(r.OutputRevenuePostTax, rev)
			}
			if vol5m != nil && (r.MinLegVol5m == nil || *vol5m < *r.MinLegVol5m) {
				r.MinLegVol5m = vol5m
			}
			if ageS != nil && trades24h != nil && *trades24h > 0 {
				ratio := float64(*ageS) / float64(int64(86400) / *trades24h)
				if r.WorstLegAgeRatio == nil || ratio > *r.WorstLegAgeRatio {
					r.WorstLegAgeRatio = &ratio
				}
			}
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}

		out := make([]comboScreenRow, 0, len(order))
		for _, id := range order {
			r := byRelation[id]
			if r.MissingLegs == 0 && r.InputCost != nil && r.OutputRevenuePostTax != nil {
				m := *r.OutputRevenuePostTax - *r.InputCost
				r.ComboMargin = &m
				if *r.InputCost > 0 {
					roi := float64(m) / float64(*r.InputCost) * 100
					r.RoiPct = &roi
				}
			}
			out = append(out, *r)
		}
		sort.SliceStable(out, func(i, j int) bool {
			a, b := out[i].ComboMargin, out[j].ComboMargin
			switch {
			case a == nil:
				return false
			case b == nil:
				return true
			default:
				return *a > *b
			}
		})

		screened := len(out)
		if len(out) > limit {
			out = out[:limit]
		}
		env := envelope.New(out, len(out))
		env.Meta = map[string]any{"screened": screened, "returned": len(out)}
		if screened > len(out) {
			env.Meta["note"] = fmt.Sprintf("%d lower-margin relations dropped by limit — raise limit or filter by kind to see them", screened-len(out))
		}
		if screened == 0 {
			env.Note = "no relations matched the filters"
		}
		return mcp.NewToolResultText(env.JSON()), nil
	}
}

func addInt(p *int64, v int64) *int64 {
	if p == nil {
		return &v
	}
	s := *p + v
	return &s
}
