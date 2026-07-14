package tools

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/osrs-ge/ge-mcp/internal/envelope"
)

// QUERIES #10/#11 (hour/dow, unlocked 2026-07-13) + #17 (hour-of-week,
// 2026-07 re-architecture). Scans all history; slow is fine (SPEC §6).
//
// Buckets use `ts AT TIME ZONE 'utc'` so a server timezone change can never
// silently shift them (prod runs GMT today; this makes UTC explicit). The
// hour-of-week bucket is dow*24+hour, dow 0 = Sunday — the same convention
// as Go's time.Weekday, asserted by TestHourOfWeekConvention.
//
// Two modes:
//   - per-item: margin structure from prices_1m PLUS price_index (bucket mean
//     mid-price ÷ the item's whole-window mean, from prices_5m), volume and
//     vol_share. `smooth` (how only, default true) pools hour±1 within the
//     same day — at ~4 weeks of history a raw how bucket is thin, pooling
//     triples its obs.
//   - global (no name_or_id): margin + summed volume only. No price_index:
//     averaging raw prices across items is meaningless (expensive items
//     dominate); the per-item-normalized version of that question is the
//     seasonal_scan tool.

// bucketExpr returns the SQL bucket expression for a dimension. Closed set —
// the only values ever spliced (sprintfSQL discipline).
func bucketExpr(dimension string) string {
	switch dimension {
	case "hour":
		return "extract(hour from ts AT TIME ZONE 'utc')::int"
	case "dow":
		return "extract(dow from ts AT TIME ZONE 'utc')::int"
	case "how":
		return "(extract(dow from ts AT TIME ZONE 'utc')*24 + extract(hour from ts AT TIME ZONE 'utc'))::int"
	}
	return ""
}

// Global: margin structure + total traded volume per bucket. %s = bucketExpr.
const seasonalityGlobalSQL = `
WITH m AS (
  SELECT %[1]s AS b, round(avg(margin))::bigint AS avg_margin, count(*) AS obs,
         min(ts) AS from_ts, max(ts) AS to_ts
  FROM prices_1m WHERE margin IS NOT NULL
  GROUP BY 1
),
v AS (
  SELECT %[1]s AS b, sum(coalesce(high_volume,0)+coalesce(low_volume,0)) AS vol
  FROM prices_5m
  GROUP BY 1
),
t AS (SELECT sum(vol) AS total_vol FROM v)
SELECT m.b, m.avg_margin, m.obs, m.obs,
       v.vol, round(v.vol::numeric / nullif(t.total_vol,0), 4),
       NULL::numeric,
       m.from_ts, m.to_ts
FROM m LEFT JOIN v ON v.b = m.b CROSS JOIN t
ORDER BY m.b`

// Per-item, unsmoothed (hour / dow / how with smooth=false). %s = bucketExpr.
const seasonalityItemSQL = `
WITH raw5 AS (
  SELECT %[1]s AS b,
         sum((coalesce(avg_high_price, avg_low_price) + coalesce(avg_low_price, avg_high_price)) / 2.0) AS sum_mid,
         count(*) AS n_mid,
         sum(coalesce(high_volume,0) + coalesce(low_volume,0)) AS vol,
         min(ts) AS from_ts, max(ts) AS to_ts
  FROM prices_5m
  WHERE item_id = $1 AND (avg_high_price IS NOT NULL OR avg_low_price IS NOT NULL)
  GROUP BY 1
),
raw1 AS (
  SELECT %[1]s AS b, round(avg(margin))::bigint AS avg_margin
  FROM prices_1m WHERE item_id = $1 AND margin IS NOT NULL
  GROUP BY 1
),
total AS (
  SELECT sum(sum_mid)/sum(n_mid) AS mean_mid, sum(vol) AS total_vol,
         min(from_ts) AS scan_from, max(to_ts) AS scan_to
  FROM raw5
)
SELECT r.b, m.avg_margin, r.n_mid, r.n_mid,
       r.vol, round(r.vol::numeric / nullif(t.total_vol,0), 4),
       round((r.sum_mid / r.n_mid / t.mean_mid)::numeric, 4),
       t.scan_from, t.scan_to
FROM raw5 r CROSS JOIN total t LEFT JOIN raw1 m USING (b)
ORDER BY r.b`

// Per-item, hour-of-week with hour±1 pooling within the same day (QUERIES #17,
// validated live 2026-07-14). obs = pooled sample count; raw_obs = this
// bucket's own rows.
const seasonalityItemHowSmoothSQL = `
WITH raw5 AS (
  SELECT (extract(dow from ts AT TIME ZONE 'utc')*24 + extract(hour from ts AT TIME ZONE 'utc'))::int AS b,
         extract(dow from ts AT TIME ZONE 'utc')::int AS d,
         extract(hour from ts AT TIME ZONE 'utc')::int AS h,
         sum((coalesce(avg_high_price, avg_low_price) + coalesce(avg_low_price, avg_high_price)) / 2.0) AS sum_mid,
         count(*) AS n_mid,
         sum(coalesce(high_volume,0) + coalesce(low_volume,0)) AS vol,
         min(ts) AS from_ts, max(ts) AS to_ts
  FROM prices_5m
  WHERE item_id = $1 AND (avg_high_price IS NOT NULL OR avg_low_price IS NOT NULL)
  GROUP BY 1, 2, 3
),
raw1 AS (
  SELECT (extract(dow from ts AT TIME ZONE 'utc')*24 + extract(hour from ts AT TIME ZONE 'utc'))::int AS b,
         round(avg(margin))::bigint AS avg_margin
  FROM prices_1m WHERE item_id = $1 AND margin IS NOT NULL
  GROUP BY 1
),
total AS (
  SELECT sum(sum_mid)/sum(n_mid) AS mean_mid, sum(vol) AS total_vol,
         min(from_ts) AS scan_from, max(to_ts) AS scan_to
  FROM raw5
),
pooled AS (
  SELECT r.b, sum(p.sum_mid)/nullif(sum(p.n_mid),0) AS mid, sum(p.n_mid) AS obs,
         r.n_mid AS raw_obs, r.vol AS vol
  FROM raw5 r
  JOIN raw5 p ON p.d = r.d AND (p.h = r.h OR p.h = (r.h+1)%24 OR p.h = (r.h+23)%24)
  GROUP BY r.b, r.n_mid, r.vol
)
SELECT p.b, m.avg_margin, p.obs, p.raw_obs,
       p.vol, round(p.vol::numeric / nullif(t.total_vol,0), 4),
       round((p.mid / t.mean_mid)::numeric, 4),
       t.scan_from, t.scan_to
FROM pooled p CROSS JOIN total t LEFT JOIN raw1 m USING (b)
ORDER BY p.b`

type seasonalityRow struct {
	Bucket     int      `json:"bucket"`
	AvgMargin  *int64   `json:"avg_margin"`
	Obs        int64    `json:"obs"`
	RawObs     int64    `json:"raw_obs"`
	Volume     *int64   `json:"volume"`
	VolShare   *float64 `json:"vol_share"`
	PriceIndex *float64 `json:"price_index"`
}

func NewSeasonalityTool() mcp.Tool {
	return mcp.NewTool("seasonality",
		mcp.WithDescription("Time-of-cycle structure: hour-of-day (bucket 0-23 UTC), day-of-week (0-6, 0=Sunday), or hour-of-week ('how', 0-167 = dow*24+hour UTC). Per item (name_or_id): post-tax margin structure plus price_index (bucket mean price / item's overall mean; 1.00 = average — the 'cheap at hour A, dear at hour B' signal) and volume share. Global: margin + volume only (cross-item price averaging is meaningless — use seasonal_scan for normalized discovery). smooth (how only, default true) pools hour±1 within the same day; obs is the pooled count, raw_obs this bucket's own. ~4 weeks of history = ~4 day-samples per how bucket even pooled: check obs, and falsify against the item's multi-week trend (a trending item fakes hour-of-week structure at this depth). Scans all history — slow."),
		mcp.WithString("dimension", mcp.Required(), mcp.Enum("hour", "dow", "how")),
		mcp.WithString("name_or_id", mcp.Description("Optional single-item filter (fuzzy name or numeric id); global when omitted")),
		mcp.WithBoolean("smooth", mcp.Description("hour±1 pooling for dimension=how (default true; ignored for hour/dow)")),
	)
}

func SeasonalityHandler(pool *pgxpool.Pool) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		dimension, err := req.RequireString("dimension")
		if err != nil {
			return mcp.NewToolResultError(envelope.ErrorJSON("bad_param", "dimension is required")), nil
		}
		expr := bucketExpr(dimension)
		if expr == "" {
			return mcp.NewToolResultError(envelope.ErrorJSON("bad_param", "dimension must be hour, dow or how")), nil
		}
		smooth := req.GetBool("smooth", true)

		var itemID *int
		var resolved *envelope.Resolved
		if nameOrID := req.GetString("name_or_id", ""); nameOrID != "" {
			res, errResult, err := resolveItem(ctx, pool, nameOrID)
			if err != nil || errResult != nil {
				return errResult, err
			}
			resolved = res
			itemID = &res.ItemID
		}

		var query string
		var args []any
		switch {
		case itemID == nil:
			query = sprintfSQL(seasonalityGlobalSQL, expr)
		case dimension == "how" && smooth:
			query = seasonalityItemHowSmoothSQL
			args = append(args, *itemID)
		default:
			query = sprintfSQL(seasonalityItemSQL, expr)
			args = append(args, *itemID)
		}

		// Full-history scan: run uncapped.
		tx, err := pool.Begin(ctx)
		if err != nil {
			return nil, err
		}
		defer tx.Rollback(ctx)
		if _, err := tx.Exec(ctx, "SET LOCAL statement_timeout = 0"); err != nil {
			return nil, err
		}
		rows, err := tx.Query(ctx, query, args...)
		if err != nil {
			return nil, err
		}

		out := []seasonalityRow{}
		var scanFrom, scanTo *time.Time
		for rows.Next() {
			var r seasonalityRow
			if err := rows.Scan(&r.Bucket, &r.AvgMargin, &r.Obs, &r.RawObs,
				&r.Volume, &r.VolShare, &r.PriceIndex, &scanFrom, &scanTo); err != nil {
				rows.Close()
				return nil, err
			}
			out = append(out, r)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
		if err := tx.Commit(ctx); err != nil {
			return nil, err
		}

		env := envelope.New(out, len(out))
		env.Resolved = resolved
		env.Meta = map[string]any{
			"dimension": dimension,
			"note":      "price_index: bucket mean mid-price / item overall mean (per-item mode only; 1.00 = average). Global mode has no price_index — use seasonal_scan.",
		}
		if dimension == "how" {
			env.Meta["smooth"] = smooth && itemID != nil
			env.Meta["bucket_convention"] = "dow*24+hour UTC, dow 0=Sunday"
		}
		if scanFrom != nil && scanTo != nil {
			env.DataWindow = &envelope.Window{From: *scanFrom, To: *scanTo}
		}
		if len(out) == 0 {
			env.Note = "no observations for this scope"
		}
		return mcp.NewToolResultText(env.JSON()), nil
	}
}
