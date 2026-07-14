package tools

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/osrs-ge/ge-mcp/internal/envelope"
)

// The seven ranking lenses (QUERIES #6–#9, #13–#15), one fixed SQL each,
// chosen by a closed-set switch. All computed columns are cast to float8 /
// bigint in SQL so the generic row collector produces clean JSON types.
// Common binds: $1 window cutoff, $2 min_obs, $3 limit (+ metric extras).

const screenVolatilitySQL = `
SELECT p.item_id, i.name,
       round(stddev_samp(coalesce(p.high,p.low)) /
             nullif(avg(coalesce(p.high,p.low)),0), 4)::float8 AS cv,
       count(*) AS obs
FROM prices_1m p JOIN items i USING (item_id)
WHERE p.ts > $1
GROUP BY p.item_id, i.name HAVING count(*) >= $2
ORDER BY cv DESC NULLS LAST LIMIT $3`

const screenSurgeSQL = `
WITH v AS (
  SELECT item_id, ts, coalesce(high_volume,0)+coalesce(low_volume,0) AS vol
  FROM prices_5m WHERE ts > $1
)
SELECT v.item_id, i.name,
       last(v.vol, v.ts)::bigint AS cur_vol,
       round(avg(v.vol))::bigint AS baseline_vol,
       round(last(v.vol, v.ts)/nullif(avg(v.vol),0), 2)::float8 AS surge_ratio,
       count(*) AS obs
FROM v JOIN items i USING (item_id)
GROUP BY v.item_id, i.name HAVING avg(v.vol) > 0 AND count(*) >= $2
ORDER BY surge_ratio DESC NULLS LAST LIMIT $3`

const screenPersistenceSQL = `
SELECT p.item_id, i.name,
       round(count(*) FILTER (WHERE p.margin > 0)::numeric / count(*), 2)::float8 AS pct_flippable,
       count(*) AS obs
FROM prices_1m p JOIN items i USING (item_id)
WHERE p.ts > $1 AND p.margin IS NOT NULL
GROUP BY p.item_id, i.name HAVING count(*) >= $2
ORDER BY pct_flippable DESC LIMIT $3`

const screenMomentumSQL = `
SELECT p.item_id, i.name,
       regr_slope(coalesce(p.high,p.low), extract(epoch from p.ts))::float8 AS slope_gp_per_sec,
       count(*) AS obs
FROM prices_1m p JOIN items i USING (item_id)
WHERE p.ts > $1
GROUP BY p.item_id, i.name HAVING count(*) >= $2
ORDER BY slope_gp_per_sec DESC NULLS LAST LIMIT $3`

// $4 = min_volume. min_obs gate is load-bearing: single-bucket items pin to ±1.0.
const screenImbalanceSQL = `
SELECT p.item_id, i.name,
       coalesce(sum(high_volume),0)::bigint AS buys,
       coalesce(sum(low_volume),0)::bigint  AS sells,
       round((coalesce(sum(high_volume),0)-coalesce(sum(low_volume),0))::numeric
             / nullif(coalesce(sum(high_volume),0)+coalesce(sum(low_volume),0),0), 3)::float8 AS imbalance,
       count(*) AS obs
FROM prices_5m p JOIN items i USING (item_id)
WHERE p.ts > $1
GROUP BY p.item_id, i.name
HAVING coalesce(sum(high_volume),0)+coalesce(sum(low_volume),0) >= $4 AND count(*) >= $2
ORDER BY abs((coalesce(sum(high_volume),0)-coalesce(sum(low_volume),0))::numeric
             / nullif(coalesce(sum(high_volume),0)+coalesce(sum(low_volume),0),0)) DESC
LIMIT $3`

// $4 = min_price (penny items pin to range_pos 0.000). Sorted ascending:
// candidates near their range floor. Band-width gate keeps the range tradeable.
const screenRangePositionSQL = `
WITH r AS (
  SELECT item_id,
         min(coalesce(low,high))  AS range_low,
         max(coalesce(high,low))  AS range_high,
         count(*) AS obs
  FROM prices_1m
  WHERE ts > $1
  GROUP BY item_id
  HAVING count(*) >= $2
),
latest AS (
  SELECT DISTINCT ON (item_id) item_id, coalesce(high,low) AS px, ts
  FROM prices_1m ORDER BY item_id, ts DESC
),
liq AS (
  SELECT DISTINCT ON (item_id) item_id,
         coalesce(high_volume,0)+coalesce(low_volume,0) AS vol5m
  FROM prices_5m WHERE ts > now() - interval '15 min'
  ORDER BY item_id, ts DESC
)
SELECT l.item_id, i.name, l.px AS cur_price, r.range_low, r.range_high,
       round((l.px - r.range_low)::numeric / nullif(r.range_high - r.range_low, 0), 3)::float8 AS range_pos,
       round((r.range_high - r.range_low)::numeric / nullif(r.range_low,0) * 100, 1)::float8 AS range_width_pct,
       r.obs, coalesce(liq.vol5m,0) AS vol5m
FROM latest l JOIN r USING (item_id) JOIN items i USING (item_id) JOIN liq USING (item_id)
WHERE l.ts > now() - interval '30 min'
  AND l.px > $4
  AND coalesce(liq.vol5m,0) >= $5
  AND (r.range_high - r.range_low)::numeric / nullif(r.range_low,0) > 0.05
ORDER BY range_pos ASC
LIMIT $3`

// cur_margin is post-tax, realized_spread pre-tax (5m has no margin column and
// we never recompute tax) — ratio slightly understated; tagged in meta.
const screenSpreadGapSQL = `
WITH rs AS (
  SELECT item_id, avg(avg_high_price - avg_low_price)::bigint AS realized_spread, count(*) AS obs
  FROM prices_5m
  WHERE ts > $1
    AND avg_high_price IS NOT NULL AND avg_low_price IS NOT NULL
  GROUP BY item_id HAVING count(*) >= $2
),
latest AS (
  SELECT DISTINCT ON (item_id) item_id, margin, high_time, low_time
  FROM prices_1m ORDER BY item_id, ts DESC
)
SELECT l.item_id, i.name, l.margin AS cur_margin, rs.realized_spread,
       round(l.margin::numeric / nullif(rs.realized_spread,0), 2)::float8 AS spread_ratio, rs.obs
FROM latest l JOIN rs USING (item_id) JOIN items i USING (item_id)
WHERE l.margin > 0 AND rs.realized_spread > 0
  AND l.high_time > now() - interval '30 min' AND l.low_time > now() - interval '30 min'
ORDER BY spread_ratio DESC LIMIT $3`

type screenMetric struct {
	sql           string
	defaultWindow string
	// extraArgs appends the metric-specific binds ($4…), if any.
	extraArgs func(req mcp.CallToolRequest) []any
	metaNote  map[string]any
}

var screenMetrics = map[string]screenMetric{
	"volatility": {sql: screenVolatilitySQL, defaultWindow: "2h",
		metaNote: map[string]any{"price_proxy": "coalesce(high,low) — cv includes spread bounce"}},
	"surge":       {sql: screenSurgeSQL, defaultWindow: "3h"},
	"persistence": {sql: screenPersistenceSQL, defaultWindow: "2h"},
	"momentum": {sql: screenMomentumSQL, defaultWindow: "1h",
		metaNote: map[string]any{"price_proxy": "coalesce(high,low) — slope includes spread bounce"}},
	"imbalance": {sql: screenImbalanceSQL, defaultWindow: "2h",
		extraArgs: func(req mcp.CallToolRequest) []any { return []any{req.GetInt("min_volume", 100)} },
		metaNote:  map[string]any{"imbalance": "(buys-sells)/(buys+sells): +1 pure insta-buying, -1 pure insta-selling"}},
	"range_position": {sql: screenRangePositionSQL, defaultWindow: "7d",
		extraArgs: func(req mcp.CallToolRequest) []any {
			return []any{req.GetInt("min_price", 50), req.GetInt("min_volume", 100)}
		},
		metaNote: map[string]any{"range_pos": "0 = at range low (sorted ascending), 1 = at range high; negative = below the band; band-width gate > 5%"}},
	"spread_gap": {sql: screenSpreadGapSQL, defaultWindow: "2h",
		metaNote: map[string]any{"basis": "cur_margin post-tax vs realized_spread pre-tax — ratio slightly understated"}},
}

func NewScreenTool() mcp.Tool {
	return mcp.NewTool("screen",
		mcp.WithDescription("One metric-tagged ranking tool, seven lenses: volatility (cv), surge (volume spike), persistence (pct of obs flippable), momentum (trend slope), imbalance (insta-buy vs insta-sell flow), range_position (where price sits in its N-day band, sorted nearest-floor first), spread_gap (quoted margin vs realized spread — stale-print traps). Every row carries obs."),
		mcp.WithString("metric", mcp.Required(), mcp.Enum("volatility", "surge", "persistence", "momentum", "imbalance", "range_position", "spread_gap")),
		mcp.WithString("window", mcp.Description("Scan window (defaults per metric: 2h; surge 3h; momentum 1h; range_position 7d)")),
		mcp.WithNumber("min_obs", mcp.Description("Minimum observations per item (default 10; range_position 50)")),
		mcp.WithNumber("min_volume", mcp.Description("imbalance only: minimum total volume in window (default 100)")),
		mcp.WithNumber("min_price", mcp.Description("range_position only: price floor in gp (default 50)")),
		mcp.WithNumber("limit", mcp.Description("Max rows (1-100, default 25)")),
	)
}

func ScreenHandler(pool *pgxpool.Pool) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		metricName, err := req.RequireString("metric")
		if err != nil {
			return mcp.NewToolResultError(envelope.ErrorJSON("bad_param", "metric is required")), nil
		}
		m, ok := screenMetrics[metricName]
		if !ok {
			return mcp.NewToolResultError(envelope.ErrorJSON("bad_param", "unknown metric "+metricName)), nil
		}
		window, badParam := durationParam(req, "window", m.defaultWindow)
		if badParam != nil {
			return badParam, nil
		}
		defaultObs := 10
		if metricName == "range_position" {
			defaultObs = 50
		}
		minObs := req.GetInt("min_obs", defaultObs)
		if minObs < 1 {
			minObs = 1
		}
		limit := clampLimit(req.GetInt("limit", 25))

		now := time.Now().UTC()
		from := now.Add(-window)
		args := []any{from, minObs, limit}
		if m.extraArgs != nil {
			args = append(args, m.extraArgs(req)...)
		}

		// Broad research scans (e.g. range_position over weeks) run uncapped.
		tx, err := pool.Begin(ctx)
		if err != nil {
			return nil, err
		}
		defer tx.Rollback(ctx)
		if _, err := tx.Exec(ctx, "SET LOCAL statement_timeout = 0"); err != nil {
			return nil, err
		}
		rows, err := tx.Query(ctx, m.sql, args...)
		if err != nil {
			return nil, err
		}
		// Metric-specific columns: collect generically, keyed by SQL aliases.
		out, err := pgx.CollectRows(rows, pgx.RowToMap)
		if err != nil {
			return nil, err
		}
		if err := tx.Commit(ctx); err != nil {
			return nil, err
		}

		env := envelope.New(out, len(out))
		env.DataWindow = &envelope.Window{From: from, To: now}
		env.Meta = map[string]any{"metric": metricName, "window": req.GetString("window", m.defaultWindow)}
		for k, v := range m.metaNote {
			env.Meta[k] = v
		}
		if len(out) == 0 {
			env.Note = "no items pass the gates in window"
		}
		return mcp.NewToolResultText(env.JSON()), nil
	}
}
