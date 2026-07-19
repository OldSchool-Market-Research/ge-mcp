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
// the SPEC deltas: per-leg ages, members filter, filled_profit, and the
// 24h-volume gates behind the two flip lanes (min_vol24h, min_price).
// gp_day is the absolute-capacity metric: post-tax margin x what a day can
// actually fill — min(buy_limit x 6 four-hour cycles, 15% of 24h volume).
// The sort column is switched in Go from a closed set — never interpolated
// from input.
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
),
vol24 AS (
  SELECT item_id,
         coalesce(sum(high_volume),0)+coalesce(sum(low_volume),0) AS vol24h
  FROM prices_5m WHERE ts > now() - interval '24 hours'
  GROUP BY item_id
)
SELECT l.item_id, i.name, l.ts,
       l.low  AS buy_at,
       l.high AS sell_at,
       l.margin,
       round(l.margin::numeric / nullif(l.low,0) * 100, 2)::float8 AS roi_pct,
       i.buy_limit,
       l.margin * i.buy_limit AS profit_per_limit,
       l.margin * least(i.buy_limit, coalesce(liq.vol5m,0)) AS filled_profit,
       l.margin * least(i.buy_limit * 6, floor(coalesce(v.vol24h,0) * 0.15)::bigint) AS gp_day,
       extract(epoch from now() - l.high_time)::int AS high_age_s,
       extract(epoch from now() - l.low_time)::int  AS low_age_s,
       coalesce(liq.vol5m,0) AS vol5m,
       coalesce(v.vol24h,0) AS vol24h
FROM latest l JOIN items i USING (item_id)
     LEFT JOIN liq USING (item_id)
     LEFT JOIN vol24 v USING (item_id)
WHERE l.margin > 0
  AND l.high_time > $1 AND l.low_time > $1
  AND coalesce(liq.vol5m,0) >= $2
  AND coalesce(v.vol24h,0) >= $3
  AND l.low >= $4
  AND ($5::boolean IS NULL OR i.members = $5)
ORDER BY %s DESC NULLS LAST
LIMIT $6`

var topFlipsSorts = map[string]string{
	"margin":           "l.margin",
	"roi_pct":          "roi_pct",
	"profit_per_limit": "profit_per_limit",
	"filled_profit":    "filled_profit",
	"gp_day":           "gp_day",
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
	GpDay          *int64    `json:"gp_day"`
	HighAgeS       int       `json:"high_age_s"`
	LowAgeS        int       `json:"low_age_s"`
	Vol5m          int64     `json:"vol5m"`
	Vol24h         int64     `json:"vol24h"`
}

func NewTopFlipsTool() mcp.Tool {
	return mcp.NewTool("top_flips",
		mcp.WithDescription("The fresh, liquid flip watchlist: latest post-tax margin with both legs fresh, always liquidity-gated (min_volume=0 to loosen). profit_per_limit is the 4h ceiling (margin x buy_limit); filled_profit is the conservative instant variant (margin x least(buy_limit, vol5m)); gp_day is the absolute daily capacity (margin x min(buy_limit x 6, 15% of vol24h)). Lane screens: volume flips = min_vol24h=100000; high-value flips = min_price=10000000, min_vol24h=200, sort_by=margin."),
		mcp.WithString("max_age", mcp.Description("Both-leg freshness gate, e.g. 30min / 5m (default 30min)")),
		mcp.WithNumber("min_volume", mcp.Description("Latest-5m-volume liquidity gate (default 50; 0 = freshness-only)")),
		mcp.WithNumber("min_vol24h", mcp.Description("24h summed volume gate in units (default 0 = off)")),
		mcp.WithNumber("min_price", mcp.Description("Minimum buy-leg price in gp (default 0 = off); the high-value lane uses 10000000")),
		mcp.WithBoolean("members", mcp.Description("Filter to members (true) or F2P (false) items; omit for all")),
		mcp.WithString("sort_by", mcp.Description("Ranking (default profit_per_limit)"), mcp.Enum("margin", "roi_pct", "profit_per_limit", "filled_profit", "gp_day")),
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
			return mcp.NewToolResultError(envelope.ErrorJSON("bad_param", "sort_by must be one of margin | roi_pct | profit_per_limit | filled_profit | gp_day")), nil
		}
		minVolume := req.GetInt("min_volume", 50)
		if minVolume < 0 {
			minVolume = 0
		}
		minVol24h := req.GetInt("min_vol24h", 0)
		if minVol24h < 0 {
			minVol24h = 0
		}
		minPrice := req.GetInt("min_price", 0)
		if minPrice < 0 {
			minPrice = 0
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
		rows, err := pool.Query(ctx, sprintfSQL(topFlipsSQL, sortCol), cutoff, minVolume, minVol24h, minPrice, members, limit)
		if err != nil {
			return nil, err
		}
		defer rows.Close()

		out := []flipRow{}
		for rows.Next() {
			var r flipRow
			if err := rows.Scan(&r.ItemID, &r.Name, &r.Ts, &r.BuyAt, &r.SellAt, &r.Margin,
				&r.RoiPct, &r.BuyLimit, &r.ProfitPerLimit, &r.FilledProfit, &r.GpDay,
				&r.HighAgeS, &r.LowAgeS, &r.Vol5m, &r.Vol24h); err != nil {
				return nil, err
			}
			out = append(out, r)
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}

		env := envelope.New(out, len(out))
		env.Meta = map[string]any{
			"sort_by": sortBy, "min_volume": minVolume,
			"min_vol24h": minVol24h, "min_price": minPrice,
			"gp_day_basis": "margin x min(buy_limit x 6 cycles/day, 15% participation of vol24h); a ceiling, not a promise — self-impact and fill risk are yours to model",
		}
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
