package tools

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/osrs-ge/ge-mcp/internal/envelope"
)

// QUERIES #19 (validated live 2026-07-14, ~11s same_how scan). The
// archetype-V primitive: is an item's volume RIGHT NOW abnormal vs its own
// baseline? Two baselines:
//   - same_how: mean/sd of this hour-of-week's hourly volumes across all
//     history — respects the weekly cycle but is thin (~4 samples at 4 weeks
//     of data; n_baseline keeps it honest).
//   - trailing: mean/sd of ALL hourly volumes over the past 7 days — robust
//     n (~168) but ignores the weekly cycle (a normal Saturday peak looks
//     anomalous vs a Tuesday-heavy baseline).
//
// buys/sells split the current window's volume by side (high_volume =
// insta-buys, low_volume = insta-sells): a one-sided spike is hoarding or a
// dump, a two-sided one is an event repricing. price_move_pct over the same
// window answers "did volume move before price?" — the V edge exists only
// while it hasn't.
//
// The orchestrator's trigger evaluation runs this same computation; keep the
// two in sync (ge-orchestrator internal/eval/source.go).
const volumeZscoreSameHowSQL = `
WITH cur AS (
  SELECT item_id,
         sum(coalesce(high_volume,0)+coalesce(low_volume,0)) AS cur_vol,
         sum(coalesce(high_volume,0)) AS buys,
         sum(coalesce(low_volume,0)) AS sells
  FROM prices_5m WHERE ts >= $1
  GROUP BY 1
),
hist AS (
  SELECT item_id, date_trunc('hour', ts) AS hb,
         sum(coalesce(high_volume,0)+coalesce(low_volume,0)) AS vol
  FROM prices_5m
  WHERE ts < date_trunc('hour', now())
    AND (extract(dow from ts AT TIME ZONE 'utc')*24 + extract(hour from ts AT TIME ZONE 'utc'))::int
      = (extract(dow from now() AT TIME ZONE 'utc')*24 + extract(hour from now() AT TIME ZONE 'utc'))::int
    AND ($4::int IS NULL OR item_id = $4)
  GROUP BY 1, 2
),
base AS (
  SELECT item_id, avg(vol) AS baseline_mean, stddev_samp(vol) AS baseline_sd,
         count(*) AS n_baseline, min(hb) AS from_ts
  FROM hist GROUP BY 1
),
px AS (
  SELECT item_id,
         (array_agg((coalesce(avg_high_price,avg_low_price)+coalesce(avg_low_price,avg_high_price))/2.0 ORDER BY ts ASC))[1]  AS p_start,
         (array_agg((coalesce(avg_high_price,avg_low_price)+coalesce(avg_low_price,avg_high_price))/2.0 ORDER BY ts DESC))[1] AS p_end
  FROM prices_5m
  WHERE ts >= $1 AND (avg_high_price IS NOT NULL OR avg_low_price IS NOT NULL)
  GROUP BY 1
)
SELECT c.item_id, i.name, c.cur_vol,
       round(b.baseline_mean)::bigint AS baseline_mean,
       round(b.baseline_sd)::bigint AS baseline_sd,
       round(((c.cur_vol - b.baseline_mean) / b.baseline_sd)::numeric, 2)::float8 AS z_score,
       b.n_baseline, c.buys, c.sells,
       round(p.p_end)::bigint AS cur_price,
       round((100*(p.p_end - p.p_start)/nullif(p.p_start,0))::numeric, 2)::float8 AS price_move_pct,
       b.from_ts, now() AS to_ts
FROM cur c
JOIN base b USING (item_id)
JOIN items i USING (item_id)
LEFT JOIN px p USING (item_id)
WHERE b.baseline_sd > 0 AND b.n_baseline >= $2 AND c.cur_vol >= $3
  AND ($4::int IS NULL OR c.item_id = $4)
ORDER BY abs((c.cur_vol - b.baseline_mean) / b.baseline_sd) DESC
LIMIT $5`

// trailing: identical shape, baseline = all hourly volumes in the past 7d.
const volumeZscoreTrailingSQL = `
WITH cur AS (
  SELECT item_id,
         sum(coalesce(high_volume,0)+coalesce(low_volume,0)) AS cur_vol,
         sum(coalesce(high_volume,0)) AS buys,
         sum(coalesce(low_volume,0)) AS sells
  FROM prices_5m WHERE ts >= $1
  GROUP BY 1
),
hist AS (
  SELECT item_id, date_trunc('hour', ts) AS hb,
         sum(coalesce(high_volume,0)+coalesce(low_volume,0)) AS vol
  FROM prices_5m
  WHERE ts < date_trunc('hour', now()) AND ts >= now() - interval '7 days'
    AND ($4::int IS NULL OR item_id = $4)
  GROUP BY 1, 2
),
base AS (
  SELECT item_id, avg(vol) AS baseline_mean, stddev_samp(vol) AS baseline_sd,
         count(*) AS n_baseline, min(hb) AS from_ts
  FROM hist GROUP BY 1
),
px AS (
  SELECT item_id,
         (array_agg((coalesce(avg_high_price,avg_low_price)+coalesce(avg_low_price,avg_high_price))/2.0 ORDER BY ts ASC))[1]  AS p_start,
         (array_agg((coalesce(avg_high_price,avg_low_price)+coalesce(avg_low_price,avg_high_price))/2.0 ORDER BY ts DESC))[1] AS p_end
  FROM prices_5m
  WHERE ts >= $1 AND (avg_high_price IS NOT NULL OR avg_low_price IS NOT NULL)
  GROUP BY 1
)
SELECT c.item_id, i.name, c.cur_vol,
       round(b.baseline_mean)::bigint AS baseline_mean,
       round(b.baseline_sd)::bigint AS baseline_sd,
       round(((c.cur_vol - b.baseline_mean) / b.baseline_sd)::numeric, 2)::float8 AS z_score,
       b.n_baseline, c.buys, c.sells,
       round(p.p_end)::bigint AS cur_price,
       round((100*(p.p_end - p.p_start)/nullif(p.p_start,0))::numeric, 2)::float8 AS price_move_pct,
       b.from_ts, now() AS to_ts
FROM cur c
JOIN base b USING (item_id)
JOIN items i USING (item_id)
LEFT JOIN px p USING (item_id)
WHERE b.baseline_sd > 0 AND b.n_baseline >= $2 AND c.cur_vol >= $3
  AND ($4::int IS NULL OR c.item_id = $4)
ORDER BY abs((c.cur_vol - b.baseline_mean) / b.baseline_sd) DESC
LIMIT $5`

type volumeZscoreRow struct {
	ItemID       int      `json:"item_id"`
	Name         string   `json:"name"`
	CurVol       int64    `json:"cur_vol"`
	BaselineMean int64    `json:"baseline_mean"`
	BaselineSd   int64    `json:"baseline_sd"`
	ZScore       float64  `json:"z_score"`
	NBaseline    int64    `json:"n_baseline"`
	Buys         int64    `json:"buys"`
	Sells        int64    `json:"sells"`
	CurPrice     *int64   `json:"cur_price"`
	PriceMovePct *float64 `json:"price_move_pct"`
}

func NewVolumeZscoreTool() mcp.Tool {
	return mcp.NewTool("volume_zscore",
		mcp.WithDescription("Volume anomaly detection (archetype V): current traded volume vs the item's own baseline, ranked by |z|. baseline=same_how compares against this hour-of-week's history (cycle-aware but thin — check n_baseline); baseline=trailing uses all hours of the past 7d (robust n, ignores the weekly cycle). buys/sells split the current volume by side — one-sided spikes are hoarding/dumps, two-sided are event repricing. price_move_pct over the same window: the V edge is volume moving BEFORE price. Omit name_or_id for a ranked scan, set it for one item. Scan takes ~10s for same_how."),
		mcp.WithString("name_or_id", mcp.Description("Optional single-item filter; ranked scan when omitted")),
		mcp.WithString("window", mcp.Description("Current-volume window (default 1h; grammar <n><s|m|min|h|d>)")),
		mcp.WithString("baseline", mcp.Enum("same_how", "trailing"), mcp.Description("Baseline population (default same_how)")),
		mcp.WithNumber("min_baseline_obs", mcp.Description("Min baseline sample count (default 3)")),
		mcp.WithNumber("min_volume", mcp.Description("Min current-window volume to report (default 100)")),
		mcp.WithNumber("limit", mcp.Description("Max rows (default 25)")),
	)
}

func VolumeZscoreHandler(pool *pgxpool.Pool) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		window, badParam := durationParam(req, "window", "1h")
		if badParam != nil {
			return badParam, nil
		}
		baseline := req.GetString("baseline", "same_how")
		var query string
		switch baseline {
		case "same_how":
			query = volumeZscoreSameHowSQL
		case "trailing":
			query = volumeZscoreTrailingSQL
		default:
			return mcp.NewToolResultError(envelope.ErrorJSON("bad_param", "baseline must be same_how or trailing")), nil
		}
		minObs := req.GetInt("min_baseline_obs", 3)
		minVolume := req.GetInt("min_volume", 100)
		limit := clampLimit(req.GetInt("limit", 25))

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

		cutoff := time.Now().UTC().Add(-window)

		// Full-history scan (same_how) / week scan: run uncapped.
		tx, err := pool.Begin(ctx)
		if err != nil {
			return nil, err
		}
		defer tx.Rollback(ctx)
		if _, err := tx.Exec(ctx, "SET LOCAL statement_timeout = 0"); err != nil {
			return nil, err
		}
		rows, err := tx.Query(ctx, query, cutoff, minObs, minVolume, itemID, limit)
		if err != nil {
			return nil, err
		}

		out := []volumeZscoreRow{}
		var scanFrom, scanTo *time.Time
		for rows.Next() {
			var r volumeZscoreRow
			if err := rows.Scan(&r.ItemID, &r.Name, &r.CurVol, &r.BaselineMean, &r.BaselineSd,
				&r.ZScore, &r.NBaseline, &r.Buys, &r.Sells, &r.CurPrice, &r.PriceMovePct,
				&scanFrom, &scanTo); err != nil {
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
			"window":   req.GetString("window", "1h"),
			"baseline": baseline,
			"note":     "z vs the item's OWN baseline; same_how n is thin at ~4 weeks of data (n_baseline). Post-shock volume overstates what a strategy can fill once the shock passes.",
		}
		if scanFrom != nil && scanTo != nil {
			env.DataWindow = &envelope.Window{From: *scanFrom, To: *scanTo}
		}
		if len(out) == 0 {
			env.Note = "no anomalies pass the gates in this window"
		}
		return mcp.NewToolResultText(env.JSON()), nil
	}
}
