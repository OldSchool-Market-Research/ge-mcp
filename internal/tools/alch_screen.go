package tools

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/osrs-ge/ge-mcp/internal/envelope"
)

const natureRuneID = 561

// QUERIES #12. Cost basis is the high leg (insta-buy) — the conservative fill
// assumption for a bulk alch buy.
const alchScreenSQL = `
WITH nat AS (
  SELECT high AS cost FROM prices_1m
  WHERE item_id = $4 AND high IS NOT NULL ORDER BY ts DESC LIMIT 1
),
latest AS (
  SELECT DISTINCT ON (item_id) item_id, ts, high, high_time FROM prices_1m ORDER BY item_id, ts DESC
),
liq AS (
  SELECT DISTINCT ON (item_id) item_id,
         coalesce(high_volume,0)+coalesce(low_volume,0) AS vol5m
  FROM prices_5m WHERE ts > now() - interval '15 min'
  ORDER BY item_id, ts DESC
)
SELECT l.item_id, i.name, l.ts, l.high AS buy_at, i.highalch, nat.cost AS nat_cost,
       i.highalch - l.high - nat.cost AS alch_margin,
       i.buy_limit, (i.highalch - l.high - nat.cost) * i.buy_limit AS profit_per_limit,
       extract(epoch from now() - l.high_time)::int AS buy_age_s,
       coalesce(liq.vol5m,0) AS vol5m
FROM latest l JOIN items i USING (item_id) LEFT JOIN liq USING (item_id), nat
WHERE i.highalch > 0 AND l.high IS NOT NULL
  AND l.high_time > $1
  AND i.highalch - l.high - nat.cost > 0
  AND coalesce(liq.vol5m,0) >= $2
ORDER BY alch_margin DESC LIMIT $3`

type alchRow struct {
	ItemID         int       `json:"item_id"`
	Name           string    `json:"name"`
	Ts             time.Time `json:"ts"`
	BuyAt          int64     `json:"buy_at"`
	Highalch       int64     `json:"highalch"`
	NatCost        int64     `json:"nat_cost"`
	AlchMargin     int64     `json:"alch_margin"`
	BuyLimit       *int64    `json:"buy_limit"`
	ProfitPerLimit *int64    `json:"profit_per_limit"`
	BuyAgeS        int       `json:"buy_age_s"`
	Vol5m          int64     `json:"vol5m"`
}

func NewAlchScreenTool() mcp.Tool {
	return mcp.NewTool("alch_screen",
		mcp.WithDescription("High-alch arbitrage: items whose insta-buy cost + a nature rune is under their highalch value. Throughput is capped by ~1,200 casts/hr AND buy_limit per 4h — profit_per_limit is the 4h ceiling, not gp/hr. Alching consumes the item (no GE tax, no resale). Requires 55 Magic."),
		mcp.WithNumber("min_volume", mcp.Description("Latest-5m-volume gate (default 50)")),
		mcp.WithString("max_age", mcp.Description("Buy-leg freshness gate (default 30min)")),
		mcp.WithNumber("limit", mcp.Description("Max rows (1-100, default 25)")),
	)
}

func AlchScreenHandler(pool *pgxpool.Pool) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		maxAge, badParam := durationParam(req, "max_age", "30min")
		if badParam != nil {
			return badParam, nil
		}
		minVolume := req.GetInt("min_volume", 50)
		limit := clampLimit(req.GetInt("limit", 25))
		cutoff := time.Now().UTC().Add(-maxAge)

		rows, err := pool.Query(ctx, alchScreenSQL, cutoff, minVolume, limit, natureRuneID)
		if err != nil {
			return nil, err
		}
		defer rows.Close()

		out := []alchRow{}
		for rows.Next() {
			var r alchRow
			if err := rows.Scan(&r.ItemID, &r.Name, &r.Ts, &r.BuyAt, &r.Highalch, &r.NatCost,
				&r.AlchMargin, &r.BuyLimit, &r.ProfitPerLimit, &r.BuyAgeS, &r.Vol5m); err != nil {
				return nil, err
			}
			out = append(out, r)
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}

		env := envelope.New(out, len(out))
		env.Meta = map[string]any{"cost_basis": "insta-buy (high leg) + nature rune", "nature_rune_item_id": natureRuneID}
		if len(out) > 0 {
			w := envelope.Window{From: out[0].Ts, To: out[0].Ts}
			for _, r := range out {
				if r.Ts.Before(w.From) {
					w.From = r.Ts
				}
				if r.Ts.After(w.To) {
					w.To = r.Ts
				}
			}
			env.DataWindow = &w
		} else {
			env.Note = "no alch-profitable items pass the gates (or no fresh nature rune quote)"
		}
		return mcp.NewToolResultText(env.JSON()), nil
	}
}
