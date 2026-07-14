package tools

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/osrs-ge/ge-mcp/internal/envelope"
)

// QUERIES #2 + the always-applied liquidity join (building block #2, R7) and
// the SPEC deltas: per-leg ages, members filter, filled_profit. The sort
// column is switched in Go from a closed set — never interpolated from input.
const topFlipsSQL = `
WITH latest AS (
  SELECT DISTINCT ON (item_id) item_id, ts, high, high_time, low, low_time, margin
  FROM prices_1m ORDER BY item_id, ts DESC
),
liq AS (
  SELECT DISTINCT ON (item_id) item_id,
         coalesce(high_volume,0)+coalesce(low_volume,0) AS vol5m
  FROM prices_5m WHERE ts > now() - interval '15 min'
  ORDER BY item_id, ts DESC
)
SELECT l.item_id, i.name, l.ts,
       l.low  AS buy_at,
       l.high AS sell_at,
       l.margin,
       round(l.margin::numeric / nullif(l.low,0) * 100, 2)::float8 AS roi_pct,
       i.buy_limit,
       l.margin * i.buy_limit AS profit_per_limit,
       l.margin * least(i.buy_limit, coalesce(liq.vol5m,0)) AS filled_profit,
       extract(epoch from now() - l.high_time)::int AS high_age_s,
       extract(epoch from now() - l.low_time)::int  AS low_age_s,
       coalesce(liq.vol5m,0) AS vol5m
FROM latest l JOIN items i USING (item_id) LEFT JOIN liq USING (item_id)
WHERE l.margin > 0
  AND l.high_time > $1 AND l.low_time > $1
  AND coalesce(liq.vol5m,0) >= $2
  AND ($3::boolean IS NULL OR i.members = $3)
ORDER BY %s DESC NULLS LAST
LIMIT $4`

var topFlipsSorts = map[string]string{
	"margin":           "l.margin",
	"roi_pct":          "roi_pct",
	"profit_per_limit": "profit_per_limit",
	"filled_profit":    "filled_profit",
}

type flipRow struct {
	ItemID         int       `json:"item_id"`
	Name           string    `json:"name"`
	Ts             time.Time `json:"ts"`
	BuyAt          *int64    `json:"buy_at"`
	SellAt         *int64    `json:"sell_at"`
	Margin         int64     `json:"margin"`
	RoiPct         *float64  `json:"roi_pct"`
	BuyLimit       *int64    `json:"buy_limit"`
	ProfitPerLimit *int64    `json:"profit_per_limit"`
	FilledProfit   *int64    `json:"filled_profit"`
	HighAgeS       int       `json:"high_age_s"`
	LowAgeS        int       `json:"low_age_s"`
	Vol5m          int64     `json:"vol5m"`
}

func NewTopFlipsTool() mcp.Tool {
	return mcp.NewTool("top_flips",
		mcp.WithDescription("The fresh, liquid flip watchlist: latest post-tax margin with both legs fresh, always liquidity-gated (min_volume=0 to loosen). profit_per_limit is the 4h ceiling (margin x buy_limit); filled_profit is the conservative fill-aware variant (margin x least(buy_limit, vol5m))."),
		mcp.WithString("max_age", mcp.Description("Both-leg freshness gate, e.g. 30min / 5m (default 30min)")),
		mcp.WithNumber("min_volume", mcp.Description("Latest-5m-volume liquidity gate (default 50; 0 = freshness-only)")),
		mcp.WithBoolean("members", mcp.Description("Filter to members (true) or F2P (false) items; omit for all")),
		mcp.WithString("sort_by", mcp.Description("Ranking (default profit_per_limit)"), mcp.Enum("margin", "roi_pct", "profit_per_limit", "filled_profit")),
		mcp.WithNumber("limit", mcp.Description("Max rows (1-100, default 25)")),
	)
}

func TopFlipsHandler(pool *pgxpool.Pool) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		maxAge, badParam := durationParam(req, "max_age", "30min")
		if badParam != nil {
			return badParam, nil
		}
		sortBy := req.GetString("sort_by", "profit_per_limit")
		sortCol, ok := topFlipsSorts[sortBy]
		if !ok {
			return mcp.NewToolResultError(envelope.ErrorJSON("bad_param", "sort_by must be one of margin | roi_pct | profit_per_limit | filled_profit")), nil
		}
		minVolume := req.GetInt("min_volume", 50)
		if minVolume < 0 {
			minVolume = 0
		}
		limit := clampLimit(req.GetInt("limit", 25))
		// Tri-state: nil pointer binds as SQL NULL (no members filter).
		var members *bool
		if v, present := req.GetArguments()["members"]; present {
			b, ok := v.(bool)
			if !ok {
				return mcp.NewToolResultError(envelope.ErrorJSON("bad_param", "members must be a boolean")), nil
			}
			members = &b
		}

		cutoff := time.Now().UTC().Add(-maxAge)
		rows, err := pool.Query(ctx, sprintfSQL(topFlipsSQL, sortCol), cutoff, minVolume, members, limit)
		if err != nil {
			return nil, err
		}
		defer rows.Close()

		out := []flipRow{}
		for rows.Next() {
			var r flipRow
			if err := rows.Scan(&r.ItemID, &r.Name, &r.Ts, &r.BuyAt, &r.SellAt, &r.Margin,
				&r.RoiPct, &r.BuyLimit, &r.ProfitPerLimit, &r.FilledProfit,
				&r.HighAgeS, &r.LowAgeS, &r.Vol5m); err != nil {
				return nil, err
			}
			out = append(out, r)
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}

		env := envelope.New(out, len(out))
		env.Meta = map[string]any{"sort_by": sortBy, "min_volume": minVolume}
		// Latest-row tool: data_window = ts bounds of the returned rows.
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
			env.Note = "no flips pass the freshness + liquidity gates"
		}
		return mcp.NewToolResultText(env.JSON()), nil
	}
}
