package tools

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/osrs-ge/ge-mcp/internal/envelope"
)

// QUERIES #18 (validated live 2026-07-14, ~12s full-market scan). The
// archetype-S discovery primitive: rank every item by the amplitude of its
// hour-of-week price structure so candidates surface in one call instead of
// one seasonality call per item.
//
// Buckets are hour±1 pooled within the same day (as seasonality smooth=true),
// so each reported cheap/dear bucket is effectively a 3-hour window centred
// on it. Items must cover all 168 buckets at min_obs — partial coverage fakes
// amplitude. Cheap/thin items still fake it via single bad prints, hence the
// min_price / min_avg_vol5m gates; and at ~4 weeks of history a secular trend
// masquerades as hour-of-week structure (a given how bucket falls on only ~4
// calendar days) — falsification against the item's own trend is mandatory
// before shipping an S strategy on a scan hit.
const seasonalScanSQL = `
WITH raw5 AS (
  SELECT item_id,
         extract(dow from ts AT TIME ZONE 'utc')::int AS d,
         extract(hour from ts AT TIME ZONE 'utc')::int AS h,
         sum((coalesce(avg_high_price, avg_low_price) + coalesce(avg_low_price, avg_high_price)) / 2.0) AS sum_mid,
         count(*) AS n_mid,
         sum(coalesce(high_volume,0) + coalesce(low_volume,0)) AS vol,
         min(ts) AS from_ts, max(ts) AS to_ts
  FROM prices_5m
  WHERE avg_high_price IS NOT NULL OR avg_low_price IS NOT NULL
  GROUP BY 1, 2, 3
),
item_stats AS (
  SELECT item_id, sum(sum_mid)/sum(n_mid) AS mean_mid, sum(vol) AS tot_vol,
         sum(n_mid) AS tot_n, min(from_ts) AS from_ts, max(to_ts) AS to_ts
  FROM raw5 GROUP BY 1
),
pooled AS (
  SELECT r.item_id, (r.d*24 + r.h)::int AS b,
         sum(p.sum_mid)/nullif(sum(p.n_mid),0) AS mid, sum(p.n_mid) AS obs
  FROM raw5 r
  JOIN raw5 p ON p.item_id = r.item_id AND p.d = r.d
             AND (p.h = r.h OR p.h = (r.h+1)%24 OR p.h = (r.h+23)%24)
  GROUP BY 1, 2, r.d, r.h
),
gated AS (
  SELECT p.item_id, p.b, p.mid / s.mean_mid AS idx, p.obs
  FROM pooled p JOIN item_stats s USING (item_id)
  WHERE s.tot_vol::numeric / s.tot_n >= $1  -- min avg volume per 5m block
    AND s.mean_mid >= $2                    -- min mean price
    AND p.obs >= $3                         -- min pooled obs per bucket
),
agg AS (
  SELECT item_id,
         min(idx) AS cheap_idx, max(idx) AS dear_idx,
         (array_agg(b ORDER BY idx ASC))[1]  AS cheap_bucket,
         (array_agg(b ORDER BY idx DESC))[1] AS dear_bucket,
         min(obs) AS min_bucket_obs
  FROM gated GROUP BY item_id
  HAVING count(*) = 168
)
SELECT a.item_id, i.name, i.buy_limit, i.members,
       round((a.dear_idx - a.cheap_idx)*100, 2)::float8 AS amplitude_pct,
       a.cheap_bucket, round(a.cheap_idx::numeric, 4)::float8 AS cheap_idx,
       a.dear_bucket, round(a.dear_idx::numeric, 4)::float8 AS dear_idx,
       a.min_bucket_obs,
       round(s.tot_vol::numeric / s.tot_n)::bigint AS avg_vol5m,
       round(s.mean_mid::numeric)::bigint AS mean_price,
       round(((a.dear_idx - a.cheap_idx) * s.mean_mid
              * least(i.buy_limit, floor((s.tot_vol::numeric / s.tot_n) * 36 * 0.15)))::numeric)::bigint AS gp_cycle,
       s.from_ts, s.to_ts
FROM agg a
JOIN item_stats s USING (item_id)
JOIN items i USING (item_id)
WHERE ($4::boolean IS NULL OR i.members = $4)
ORDER BY amplitude_pct DESC
LIMIT $5`

type seasonalScanRow struct {
	ItemID       int     `json:"item_id"`
	Name         string  `json:"name"`
	BuyLimit     *int64  `json:"buy_limit"`
	Members      *bool   `json:"members"`
	AmplitudePct float64 `json:"amplitude_pct"`
	CheapBucket  int     `json:"cheap_bucket"`
	CheapIdx     float64 `json:"cheap_idx"`
	DearBucket   int     `json:"dear_bucket"`
	DearIdx      float64 `json:"dear_idx"`
	MinBucketObs int64   `json:"min_bucket_obs"`
	AvgVol5m     int64   `json:"avg_vol5m"`
	MeanPrice    int64   `json:"mean_price"`
	GpCycle      *int64  `json:"gp_cycle"`
}

func NewSeasonalScanTool() mcp.Tool {
	return mcp.NewTool("seasonal_scan",
		mcp.WithDescription("Archetype-S discovery: rank ALL items by hour-of-week price amplitude (max pooled price_index − min). Buckets are hour±1 pooled (a reported bucket ≈ a 3h window centred on it; convention dow*24+hour UTC, dow 0=Sunday). amplitude_pct must clear the 2% GE sell tax plus spread friction to be tradeable — treat < ~3% as noise. CAUTION at ~4 weeks of data: a secular trend fakes hour-of-week structure (each bucket samples only ~4 calendar days) — always falsify a hit against the item's own multi-week trend (item_history) and its per-item seasonality before shipping. Full-history scan, takes ~10-15s."),
		mcp.WithNumber("min_avg_vol5m", mcp.Description("Min average traded volume per 5m block over the whole window (default 500)")),
		mcp.WithNumber("min_price", mcp.Description("Min whole-window mean price in gp (default 250) — cheap items are print-noise amplitude")),
		mcp.WithNumber("min_obs", mcp.Description("Min pooled obs per how bucket; buckets below are excluded and items must still cover all 168 (default 9)")),
		mcp.WithBoolean("members", mcp.Description("Filter to members (true) or F2P (false) items; omit for all")),
		mcp.WithNumber("limit", mcp.Description("Max rows (default 25)")),
	)
}

func SeasonalScanHandler(pool *pgxpool.Pool) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		minVol := req.GetFloat("min_avg_vol5m", 500)
		minPrice := req.GetFloat("min_price", 250)
		minObs := req.GetInt("min_obs", 9)
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

		// Full-history scan: run uncapped.
		tx, err := pool.Begin(ctx)
		if err != nil {
			return nil, err
		}
		defer tx.Rollback(ctx)
		if _, err := tx.Exec(ctx, "SET LOCAL statement_timeout = 0"); err != nil {
			return nil, err
		}
		rows, err := tx.Query(ctx, seasonalScanSQL, minVol, minPrice, minObs, members, limit)
		if err != nil {
			return nil, err
		}

		out := []seasonalScanRow{}
		var scanFrom, scanTo *time.Time
		for rows.Next() {
			var r seasonalScanRow
			if err := rows.Scan(&r.ItemID, &r.Name, &r.BuyLimit, &r.Members,
				&r.AmplitudePct, &r.CheapBucket, &r.CheapIdx, &r.DearBucket, &r.DearIdx,
				&r.MinBucketObs, &r.AvgVol5m, &r.MeanPrice, &r.GpCycle, &scanFrom, &scanTo); err != nil {
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
		env.Meta = map[string]any{
			"bucket_convention": "dow*24+hour UTC, dow 0=Sunday; each bucket hour±1 pooled (≈3h window)",
			"note":              "amplitude below ~3% rarely survives the 2% sell tax + spread crossing; trend-vs-seasonality falsification is mandatory (see tool description)",
			"gp_cycle_basis":    "pre-tax ceiling per cheap->dear cycle: amplitude in gp x min(buy_limit, 15% of the ~3h bucket window volume). A big amplitude_pct with a tiny gp_cycle is a trap — judge candidates by gp, not ratio",
		}
		if scanFrom != nil && scanTo != nil {
			env.DataWindow = &envelope.Window{From: *scanFrom, To: *scanTo}
		}
		if len(out) == 0 {
			env.Note = "no items pass the gates (lower min_avg_vol5m / min_price / min_obs?)"
		}
		return mcp.NewToolResultText(env.JSON()), nil
	}
}
