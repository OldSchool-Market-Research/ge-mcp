package tools

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/osrs-ge/ge-mcp/internal/envelope"
)

// QUERIES #3 + n (SPEC delta). coalesce(high,low) is a price proxy that mixes
// bid and ask — tagged in meta (R4).
const moversSQL = `
WITH w AS (
  SELECT item_id,
         first(coalesce(high,low), ts) AS p_start,
         last (coalesce(high,low), ts) AS p_end,
         count(*) AS n
  FROM prices_1m
  WHERE ts > $1
  GROUP BY item_id
),
liq AS (
  SELECT DISTINCT ON (item_id) item_id,
         coalesce(high_volume,0)+coalesce(low_volume,0) AS vol
  FROM prices_5m WHERE ts > now() - interval '15 min'
  ORDER BY item_id, ts DESC
)
SELECT w.item_id, i.name, w.p_start, w.p_end,
       round((w.p_end - w.p_start)::numeric / nullif(w.p_start,0) * 100, 2)::float8 AS pct_chg,
       liq.vol AS vol5m,
       w.n
FROM w JOIN items i USING (item_id) JOIN liq USING (item_id)
WHERE w.p_start > $2 AND liq.vol > $3
ORDER BY abs((w.p_end - w.p_start)::numeric / nullif(w.p_start,0)) DESC NULLS LAST
LIMIT $4`

type moverRow struct {
	ItemID int      `json:"item_id"`
	Name   string   `json:"name"`
	PStart *int64   `json:"p_start"`
	PEnd   *int64   `json:"p_end"`
	PctChg *float64 `json:"pct_chg"`
	Vol5m  int64    `json:"vol5m"`
	N      int64    `json:"n"`
}

func NewMoversTool() mcp.Tool {
	return mcp.NewTool("movers",
		mcp.WithDescription("Biggest % price moves over a window (event/news detection), liquidity-gated. Price is the coalesce(high,low) proxy — includes spread bounce, not a clean mid (tagged in meta)."),
		mcp.WithString("window", mcp.Description("Move window, e.g. 2h / 30min (default 2h)")),
		mcp.WithNumber("min_price", mcp.Description("Price floor in gp (default 50) — kills penny-item % noise")),
		mcp.WithNumber("min_volume", mcp.Description("Latest-5m-volume gate (default 100)")),
		mcp.WithNumber("limit", mcp.Description("Max rows (1-100, default 25)")),
	)
}

func MoversHandler(pool *pgxpool.Pool) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		window, badParam := durationParam(req, "window", "2h")
		if badParam != nil {
			return badParam, nil
		}
		minPrice := req.GetInt("min_price", 50)
		minVolume := req.GetInt("min_volume", 100)
		limit := clampLimit(req.GetInt("limit", 25))

		now := time.Now().UTC()
		from := now.Add(-window)

		rows, err := pool.Query(ctx, moversSQL, from, minPrice, minVolume, limit)
		if err != nil {
			return nil, err
		}
		defer rows.Close()

		out := []moverRow{}
		for rows.Next() {
			var r moverRow
			if err := rows.Scan(&r.ItemID, &r.Name, &r.PStart, &r.PEnd, &r.PctChg, &r.Vol5m, &r.N); err != nil {
				return nil, err
			}
			out = append(out, r)
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}

		env := envelope.New(out, len(out))
		env.DataWindow = &envelope.Window{From: from, To: now}
		env.Meta = map[string]any{
			"window":      req.GetString("window", "2h"),
			"price_proxy": "coalesce(high,low) — includes spread bounce, not a clean mid",
		}
		if len(out) == 0 {
			env.Note = "no movers pass the price + liquidity gates in window"
		}
		return mcp.NewToolResultText(env.JSON()), nil
	}
}
