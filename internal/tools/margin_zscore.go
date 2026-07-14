package tools

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/osrs-ge/ge-mcp/internal/envelope"
)

// QUERIES #4 + the SPEC deltas (sd and per-leg ages in the row).
const marginZscoreSQL = `
WITH stats AS (
  SELECT item_id, avg(margin) AS mu, stddev_samp(margin) AS sd, count(*) AS n
  FROM prices_1m
  WHERE ts > $1 AND margin IS NOT NULL
  GROUP BY item_id
),
latest AS (
  SELECT DISTINCT ON (item_id) item_id, margin, low, high_time, low_time
  FROM prices_1m ORDER BY item_id, ts DESC
)
SELECT l.item_id, i.name,
       l.margin                                              AS cur_margin,
       round(s.mu)::bigint                                   AS avg_margin,
       round(s.sd)::bigint                                   AS sd,
       round((l.margin - s.mu)/nullif(s.sd,0), 2)::float8    AS z_score,
       round(l.margin::numeric/nullif(l.low,0)*100, 2)::float8 AS roi_pct,
       s.n                                                   AS samples,
       extract(epoch from now() - l.high_time)::int          AS high_age_s,
       extract(epoch from now() - l.low_time)::int           AS low_age_s
FROM latest l JOIN stats s USING (item_id) JOIN items i USING (item_id)
WHERE s.n >= $2 AND s.sd > 0 AND l.margin > 0
  AND l.high_time > $3 AND l.low_time > $3
ORDER BY z_score DESC
LIMIT $4`

type zscoreRow struct {
	ItemID    int      `json:"item_id"`
	Name      string   `json:"name"`
	CurMargin int64    `json:"cur_margin"`
	AvgMargin int64    `json:"avg_margin"`
	Sd        int64    `json:"sd"`
	ZScore    *float64 `json:"z_score"`
	RoiPct    *float64 `json:"roi_pct"`
	Samples   int64    `json:"samples"`
	HighAgeS  int      `json:"high_age_s"`
	LowAgeS   int      `json:"low_age_s"`
}

func NewMarginZscoreTool() mcp.Tool {
	return mcp.NewTool("margin_zscore",
		mcp.WithDescription("Items whose current post-tax margin is abnormally wide vs their OWN recent baseline (mean reversion / transient spread). samples is the baseline n — the directive's confidence rules key off it."),
		mcp.WithString("baseline_window", mcp.Description("Baseline stats window, e.g. 2h (default 2h)")),
		mcp.WithNumber("min_samples", mcp.Description("Minimum baseline observations (default 10) — thin items give garbage z-scores")),
		mcp.WithString("max_age", mcp.Description("Both-leg freshness gate (default 20min)")),
		mcp.WithNumber("limit", mcp.Description("Max rows (1-100, default 25)")),
	)
}

func MarginZscoreHandler(pool *pgxpool.Pool) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		baselineWindow, badParam := durationParam(req, "baseline_window", "2h")
		if badParam != nil {
			return badParam, nil
		}
		maxAge, badParam := durationParam(req, "max_age", "20min")
		if badParam != nil {
			return badParam, nil
		}
		minSamples := req.GetInt("min_samples", 10)
		if minSamples < 2 {
			minSamples = 2 // stddev_samp needs >= 2
		}
		limit := clampLimit(req.GetInt("limit", 25))

		now := time.Now().UTC()
		baselineFrom := now.Add(-baselineWindow)
		freshCutoff := now.Add(-maxAge)

		rows, err := pool.Query(ctx, marginZscoreSQL, baselineFrom, minSamples, freshCutoff, limit)
		if err != nil {
			return nil, err
		}
		defer rows.Close()

		out := []zscoreRow{}
		for rows.Next() {
			var r zscoreRow
			if err := rows.Scan(&r.ItemID, &r.Name, &r.CurMargin, &r.AvgMargin, &r.Sd,
				&r.ZScore, &r.RoiPct, &r.Samples, &r.HighAgeS, &r.LowAgeS); err != nil {
				return nil, err
			}
			out = append(out, r)
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}

		env := envelope.New(out, len(out))
		// The baseline window is what was scanned.
		env.DataWindow = &envelope.Window{From: baselineFrom, To: now}
		env.Meta = map[string]any{"baseline_window": req.GetString("baseline_window", "2h"), "min_samples": minSamples}
		if len(out) == 0 {
			env.Note = "no items pass the baseline + freshness gates"
		}
		return mcp.NewToolResultText(env.JSON()), nil
	}
}
