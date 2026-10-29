package tools

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/osrs-ge/ge-mcp/internal/envelope"
)

// Processing recipes (ge-data init/04-06): conversions that cost player time,
// ranked by gp/hr. Every margin and rate comes from ge-data's
// process_quotes_now / process_legs_now views and the ingester's hourly
// process_snapshots; these tools only filter, sort and aggregate. Re-deriving
// a margin here would let the agent and the dashboard disagree.

// processUnavailable converts a missing process table/view (42P01) into a
// typed tool error: the schema ships as a manual DDL apply separate from this
// binary, and a run must be able to note it and continue.
func processUnavailable(err error) *mcp.CallToolResult {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "42P01" {
		return mcp.NewToolResultError(envelope.ErrorJSON("process_unavailable",
			"the process_recipes schema (ge-data init/04-06) is not present in this database: processing tools are unavailable; note it and continue"))
	}
	return nil
}

// resolveRecipe takes a numeric recipe_id or a name: exact match first, then
// the shortest name containing it.
func resolveRecipe(ctx context.Context, pool *pgxpool.Pool, nameOrID string) (int, *mcp.CallToolResult, error) {
	nameOrID = strings.TrimSpace(nameOrID)
	if nameOrID == "" {
		return 0, mcp.NewToolResultError(envelope.ErrorJSON("bad_param", "recipe must be a recipe_id or a name")), nil
	}
	var id int
	var err error
	if n, convErr := strconv.Atoi(nameOrID); convErr == nil {
		err = pool.QueryRow(ctx, `SELECT recipe_id FROM process_recipes WHERE recipe_id = $1`, n).Scan(&id)
	} else {
		err = pool.QueryRow(ctx, `
			SELECT recipe_id FROM process_recipes
			WHERE lower(name) LIKE '%' || lower($1) || '%' ESCAPE '\'
			ORDER BY (lower(name) = lower($2)) DESC, length(name), name
			LIMIT 1`, escapeLike(nameOrID), nameOrID).Scan(&id)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, mcp.NewToolResultError(envelope.ErrorJSON("recipe_not_found", "no processing recipe matches "+nameOrID+"; find names with process_screen")), nil
	}
	if err != nil {
		if r := processUnavailable(err); r != nil {
			return 0, r, nil
		}
		return 0, nil, err
	}
	return id, nil, nil
}

// processQuoteRow mirrors process_quotes_now. Numerics are *float64: NULL is
// "unpriceable right now" (a leg had no traded side) and must reach the agent
// as null, never 0.
type processQuoteRow struct {
	RecipeID                int      `json:"recipe_id"`
	Name                    string   `json:"name"`
	Category                string   `json:"category"`
	Throughput              string   `json:"throughput"`
	MarginSlow              *float64 `json:"margin_slow"`
	MarginInsta             *float64 `json:"margin_insta"`
	GpHr                    *float64 `json:"gp_hr"`
	GpHrInsta               *float64 `json:"gp_hr_insta"`
	GpHrTheoretical         *float64 `json:"gp_hr_theoretical"`
	ActionsPerHour          *float64 `json:"actions_per_hour"`
	EffectiveActionsPerHour *float64 `json:"effective_actions_per_hour"`
	LimitActionsPerHour     *float64 `json:"limit_actions_per_hour"`
	MarketActionsPerHour    *float64 `json:"market_actions_per_hour"`
	FeeGp                   float64  `json:"fee_gp"`
	MissingLegs             int64    `json:"missing_legs"`
	MaxLegAgeS              *int64   `json:"max_leg_age_s"`
}

const processQuoteCols = `recipe_id, name, category, throughput, margin_slow, margin_insta, gp_hr,
  gp_hr_insta, gp_hr_theoretical, actions_per_hour, effective_actions_per_hour,
  limit_actions_per_hour, market_actions_per_hour, fee_gp, missing_legs, max_leg_age_s`

func scanProcessQuote(row pgx.Row, extra ...any) (processQuoteRow, error) {
	var r processQuoteRow
	dest := append([]any{&r.RecipeID, &r.Name, &r.Category, &r.Throughput, &r.MarginSlow, &r.MarginInsta,
		&r.GpHr, &r.GpHrInsta, &r.GpHrTheoretical, &r.ActionsPerHour, &r.EffectiveActionsPerHour,
		&r.LimitActionsPerHour, &r.MarketActionsPerHour, &r.FeeGp, &r.MissingLegs, &r.MaxLegAgeS}, extra...)
	return r, row.Scan(dest...)
}

// ---------------------------------------------------------------------------
// process_screen
// ---------------------------------------------------------------------------

// The %s is a sort column from processSortCols (closed Go-side set).
const processScreenSQL = `
SELECT ` + processQuoteCols + `, count(*) OVER () AS screened
FROM process_quotes_now
WHERE ($1::text IS NULL OR category ILIKE '%%' || $1 || '%%')
  AND ($2::text IS NULL OR throughput = $2)
ORDER BY %s DESC NULLS LAST, recipe_id
LIMIT $3`

var processSortCols = map[string]string{
	"gp_hr":             "gp_hr",
	"gp_hr_insta":       "gp_hr_insta",
	"gp_hr_theoretical": "gp_hr_theoretical",
	"margin_slow":       "margin_slow",
}

func NewProcessScreenTool() mcp.Tool {
	return mcp.NewTool("process_screen",
		mcp.WithDescription("Rank ALL processing recipes (player-time money-makers: planks, keels, hull parts, potions, fish crates, GWD breakdowns...) in one call by live gp/hr. margin_slow = buy inputs at low, sell outputs at high minus GE tax, minus NPC fee, per action; margin_insta = the reverse prices. gp_hr = margin_slow x effective_actions_per_hour, where effective = LEAST(player actions/hr, buy-limit rate, 24h market-volume rate); gp_hr_theoretical ignores the market (player speed only) and overstates thin items. A null margin with missing_legs > 0 means a leg has no traded side: unpriceable now, not zero. Skill/quest requirements are NOT curated; surface that caveat. Deep-dive one recipe with process_quote; trends with process_history / process_top."),
		mcp.WithString("category", mcp.Description("Optional substring filter on category, e.g. 'Hull', 'Herblore', 'Plank'")),
		mcp.WithString("throughput", mcp.Enum("fixed", "volume_capped", "volume_bound", "unknown"), mcp.Description("Optional throughput-kind filter")),
		mcp.WithString("sort", mcp.Enum("gp_hr", "gp_hr_insta", "gp_hr_theoretical", "margin_slow"), mcp.Description("Ranking column (default gp_hr)")),
		mcp.WithNumber("limit", mcp.Description("Rows returned (default 25, max 100); meta.screened says how many matched")),
	)
}

func ProcessScreenHandler(pool *pgxpool.Pool) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var category, throughput *string
		if c := strings.TrimSpace(req.GetString("category", "")); c != "" {
			c = escapeLike(c)
			category = &c
		}
		if t := req.GetString("throughput", ""); t != "" {
			switch t {
			case "fixed", "volume_capped", "volume_bound", "unknown":
				throughput = &t
			default:
				return mcp.NewToolResultError(envelope.ErrorJSON("bad_param", "throughput must be fixed, volume_capped, volume_bound or unknown")), nil
			}
		}
		sortCol, ok := processSortCols[req.GetString("sort", "gp_hr")]
		if !ok {
			return mcp.NewToolResultError(envelope.ErrorJSON("bad_param", "sort must be gp_hr, gp_hr_insta, gp_hr_theoretical or margin_slow")), nil
		}
		limit := min(max(req.GetInt("limit", 25), 1), 100)

		rows, err := pool.Query(ctx, sprintfSQL(processScreenSQL, sortCol), category, throughput, limit)
		if err != nil {
			if r := processUnavailable(err); r != nil {
				return r, nil
			}
			return nil, err
		}
		defer rows.Close()
		out := []processQuoteRow{}
		var screened int64
		for rows.Next() {
			r, err := scanProcessQuote(rows, &screened)
			if err != nil {
				return nil, err
			}
			out = append(out, r)
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}

		env := envelope.New(out, len(out))
		env.Meta = map[string]any{"screened": screened, "returned": len(out), "sort": sortCol}
		if len(out) == 0 {
			env.Note = "no processing recipes match the filters"
		}
		return mcp.NewToolResultText(env.JSON()), nil
	}
}

// ---------------------------------------------------------------------------
// process_quote
// ---------------------------------------------------------------------------

const processRecipeSQL = `
SELECT recipe_id, source, coalesce(relation_name, ''), coalesce(relation_direction, ''), coalesce(notes, '')
FROM process_recipes WHERE recipe_id = $1`

const processLegsSQL = `
SELECT side, item_id, name, qty, buy_limit,
       CASE side WHEN 'buy' THEN low ELSE high END AS price_slow,
       CASE side WHEN 'buy' THEN high ELSE low END AS price_insta,
       extract(epoch FROM now() - CASE side WHEN 'buy' THEN low_time ELSE high_time END)::bigint AS age_s,
       CASE side WHEN 'buy' THEN low_vol_24h ELSE high_vol_24h END AS fill_volume_24h
FROM process_legs_now WHERE recipe_id = $1
ORDER BY side, item_id`

const processQuoteSQL = `SELECT ` + processQuoteCols + ` FROM process_quotes_now WHERE recipe_id = $1`

type processLeg struct {
	Side          string   `json:"side"`
	ItemID        int      `json:"item_id"`
	Name          *string  `json:"name"`
	Qty           float64  `json:"qty"`
	BuyLimit      *int64   `json:"buy_limit"`
	PriceSlow     *int64   `json:"price_slow"`
	PriceInsta    *int64   `json:"price_insta"`
	AgeS          *int64   `json:"age_s"`
	FillVolume24h *float64 `json:"fill_volume_24h"`
}

func NewProcessQuoteTool() mcp.Tool {
	return mcp.NewTool("process_quote",
		mcp.WithDescription("One processing recipe in detail: every leg (side buy/sell, qty per action (can be fractional: expected yields), buy_limit, price_slow/price_insta, age_s of the price used, fill_volume_24h = trades on the side a patient offer fills against) plus the same summary process_screen returns, and the recipe's source, notes and linked item_relations name if any. Use it to check which leg limits a recipe (a null buy_limit means the limit is unknown, so the limit rate ignores that leg and is optimistic)."),
		mcp.WithString("recipe", mcp.Required(), mcp.Description("recipe_id from process_screen, or a recipe name")),
	)
}

func ProcessQuoteHandler(pool *pgxpool.Pool) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		id, errResult, err := resolveRecipe(ctx, pool, req.GetString("recipe", ""))
		if err != nil || errResult != nil {
			return errResult, err
		}

		var source, relName, relDir, notes string
		if err := pool.QueryRow(ctx, processRecipeSQL, id).Scan(&id, &source, &relName, &relDir, &notes); err != nil {
			return nil, err
		}
		summary, err := scanProcessQuote(pool.QueryRow(ctx, processQuoteSQL, id))
		if err != nil {
			return nil, err
		}

		rows, err := pool.Query(ctx, processLegsSQL, id)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		legs := []processLeg{}
		for rows.Next() {
			var l processLeg
			if err := rows.Scan(&l.Side, &l.ItemID, &l.Name, &l.Qty, &l.BuyLimit, &l.PriceSlow, &l.PriceInsta, &l.AgeS, &l.FillVolume24h); err != nil {
				return nil, err
			}
			legs = append(legs, l)
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}

		env := envelope.New(legs, len(legs))
		env.Meta = map[string]any{"summary": summary, "source": source}
		if relName != "" {
			env.Meta["relation_name"] = relName
			env.Meta["relation_direction"] = relDir
		}
		if notes != "" {
			env.Meta["notes"] = notes
		}
		return mcp.NewToolResultText(env.JSON()), nil
	}
}

// ---------------------------------------------------------------------------
// process_history
// ---------------------------------------------------------------------------

// Bucket width comes from processBuckets (closed set) and is bound as an
// interval parameter, never spliced.
const processHistorySQL = `
SELECT time_bucket($3::interval, ts) AS bucket,
       round(avg(margin_slow))  AS margin_slow,
       round(avg(margin_insta)) AS margin_insta,
       round(avg(gp_hr))        AS gp_hr,
       round(avg(gp_hr_insta))  AS gp_hr_insta,
       round(avg(effective_actions_per_hour), 2) AS effective_actions_per_hour,
       count(*)           AS hours,
       count(margin_slow) AS priced_hours
FROM process_snapshots
WHERE recipe_id = $1 AND ts >= $2
GROUP BY 1 ORDER BY 1`

var processBuckets = map[string]string{"1h": "1 hour", "6h": "6 hours", "1d": "1 day"}

type processHistoryRow struct {
	Bucket                  time.Time `json:"bucket"`
	MarginSlow              *float64  `json:"margin_slow"`
	MarginInsta             *float64  `json:"margin_insta"`
	GpHr                    *float64  `json:"gp_hr"`
	GpHrInsta               *float64  `json:"gp_hr_insta"`
	EffectiveActionsPerHour *float64  `json:"effective_actions_per_hour"`
	Hours                   int64     `json:"hours"`
	PricedHours             int64     `json:"priced_hours"`
}

func NewProcessHistoryTool() mcp.Tool {
	return mcp.NewTool("process_history",
		mcp.WithDescription("Trend for ONE processing recipe from the hourly snapshots: per bucket the average margin_slow, margin_insta, gp_hr, gp_hr_insta and effective actions/hr, plus hours (snapshots in the bucket) and priced_hours (hours with a non-null margin). priced_hours < hours means the recipe was unpriceable for part of the bucket; averages cover priced hours only. History starts when the snapshot job first ran, so short windows may be all there is."),
		mcp.WithString("recipe", mcp.Required(), mcp.Description("recipe_id from process_screen, or a recipe name")),
		mcp.WithString("window", mcp.Description("How far back, <n><h|d> (default 7d, max 90d)")),
		mcp.WithString("bucket", mcp.Enum("1h", "6h", "1d"), mcp.Description("Bucket width (default 6h)")),
	)
}

func ProcessHistoryHandler(pool *pgxpool.Pool) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		id, errResult, err := resolveRecipe(ctx, pool, req.GetString("recipe", ""))
		if err != nil || errResult != nil {
			return errResult, err
		}
		window, errResult := durationParam(req, "window", "7d")
		if errResult != nil {
			return errResult, nil
		}
		if window > 90*24*time.Hour {
			return mcp.NewToolResultError(envelope.ErrorJSON("bad_param", "window: max 90d")), nil
		}
		bucket, ok := processBuckets[req.GetString("bucket", "6h")]
		if !ok {
			return mcp.NewToolResultError(envelope.ErrorJSON("bad_param", "bucket must be 1h, 6h or 1d")), nil
		}
		to := time.Now().UTC()
		from := to.Add(-window)

		rows, err := pool.Query(ctx, processHistorySQL, id, from, bucket)
		if err != nil {
			if r := processUnavailable(err); r != nil {
				return r, nil
			}
			return nil, err
		}
		defer rows.Close()
		out := []processHistoryRow{}
		for rows.Next() {
			var r processHistoryRow
			if err := rows.Scan(&r.Bucket, &r.MarginSlow, &r.MarginInsta, &r.GpHr, &r.GpHrInsta, &r.EffectiveActionsPerHour, &r.Hours, &r.PricedHours); err != nil {
				return nil, err
			}
			out = append(out, r)
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}

		env := envelope.New(out, len(out))
		env.DataWindow = &envelope.Window{From: from, To: to}
		env.Meta = map[string]any{"recipe_id": id}
		if len(out) == 0 {
			env.Note = "no snapshots in this window: the ingester writes one per recipe per hour, so history may not have started yet"
		}
		return mcp.NewToolResultText(env.JSON()), nil
	}
}

// ---------------------------------------------------------------------------
// process_top
// ---------------------------------------------------------------------------

// %s is a metric column from processTopMetrics (closed Go-side set). The
// per-hour rank runs across ALL recipes before the category filter, so
// hours_in_top10 means top 10 of the whole universe.
const processTopSQL = `
WITH s AS (
  SELECT recipe_id, ts, %s AS v,
         rank() OVER (PARTITION BY ts ORDER BY %[1]s DESC NULLS LAST) AS rk
  FROM process_snapshots WHERE ts >= $1
)
SELECT s.recipe_id, p.name, p.category,
       count(*)   AS hours,
       count(s.v) AS priced_hours,
       round(avg(s.v)) AS avg,
       round(percentile_cont(0.5)  WITHIN GROUP (ORDER BY s.v)::numeric) AS median,
       round(percentile_cont(0.25) WITHIN GROUP (ORDER BY s.v)::numeric) AS p25,
       round(100.0 * count(*) FILTER (WHERE s.v > 0) / count(*), 1) AS pct_hours_positive,
       count(*) FILTER (WHERE s.rk <= 10 AND s.v IS NOT NULL) AS hours_in_top10,
       round(avg(s.v) FILTER (WHERE s.ts >= now() - interval '24 hours')) AS avg_last_24h
FROM s JOIN process_recipes p USING (recipe_id)
WHERE ($2::text IS NULL OR p.category ILIKE '%%' || $2 || '%%')
GROUP BY s.recipe_id, p.name, p.category
ORDER BY median DESC NULLS LAST, s.recipe_id
LIMIT $3`

var processTopMetrics = map[string]string{
	"gp_hr":       "gp_hr",
	"gp_hr_insta": "gp_hr_insta",
	"margin_slow": "margin_slow",
}

type processTopRow struct {
	RecipeID         int      `json:"recipe_id"`
	Name             string   `json:"name"`
	Category         string   `json:"category"`
	Hours            int64    `json:"hours"`
	PricedHours      int64    `json:"priced_hours"`
	Avg              *float64 `json:"avg"`
	Median           *float64 `json:"median"`
	P25              *float64 `json:"p25"`
	PctHoursPositive *float64 `json:"pct_hours_positive"`
	HoursInTop10     int64    `json:"hours_in_top10"`
	AvgLast24h       *float64 `json:"avg_last_24h"`
}

func NewProcessTopTool() mcp.Tool {
	return mcp.NewTool("process_top",
		mcp.WithDescription("Top processing recipes over a window, from the hourly snapshots, ranked by MEDIAN of the metric (robust to one-hour spikes). Per recipe: avg, median, p25 (a bad-hour floor), pct_hours_positive, hours_in_top10 (hours it ranked top 10 of ALL recipes), and avg_last_24h vs avg to spot a recipe that's rising or fading. Prefer recipes with a high p25 and pct_hours_positive near 100 (unpriced hours count as not positive): consistently good beats occasionally great. priced_hours < hours means some hours had an unpriceable leg."),
		mcp.WithString("window", mcp.Description("How far back, <n><h|d> (default 7d, max 90d)")),
		mcp.WithString("metric", mcp.Enum("gp_hr", "gp_hr_insta", "margin_slow"), mcp.Description("Metric to rank on (default gp_hr)")),
		mcp.WithString("category", mcp.Description("Optional substring filter on category")),
		mcp.WithNumber("limit", mcp.Description("Rows (default 20, max 100)")),
	)
}

func ProcessTopHandler(pool *pgxpool.Pool) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		window, errResult := durationParam(req, "window", "7d")
		if errResult != nil {
			return errResult, nil
		}
		if window > 90*24*time.Hour {
			return mcp.NewToolResultError(envelope.ErrorJSON("bad_param", "window: max 90d")), nil
		}
		metric, ok := processTopMetrics[req.GetString("metric", "gp_hr")]
		if !ok {
			return mcp.NewToolResultError(envelope.ErrorJSON("bad_param", "metric must be gp_hr, gp_hr_insta or margin_slow")), nil
		}
		var category *string
		if c := strings.TrimSpace(req.GetString("category", "")); c != "" {
			c = escapeLike(c)
			category = &c
		}
		limit := min(max(req.GetInt("limit", 20), 1), 100)
		to := time.Now().UTC()
		from := to.Add(-window)

		rows, err := pool.Query(ctx, sprintfSQL(processTopSQL, metric), from, category, limit)
		if err != nil {
			if r := processUnavailable(err); r != nil {
				return r, nil
			}
			return nil, err
		}
		defer rows.Close()
		out := []processTopRow{}
		for rows.Next() {
			var r processTopRow
			if err := rows.Scan(&r.RecipeID, &r.Name, &r.Category, &r.Hours, &r.PricedHours, &r.Avg, &r.Median, &r.P25, &r.PctHoursPositive, &r.HoursInTop10, &r.AvgLast24h); err != nil {
				return nil, err
			}
			out = append(out, r)
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}

		env := envelope.New(out, len(out))
		env.DataWindow = &envelope.Window{From: from, To: to}
		env.Meta = map[string]any{"metric": metric}
		if len(out) == 0 {
			env.Note = "no snapshots in this window: the ingester writes one per recipe per hour, so history may not have started yet"
		}
		return mcp.NewToolResultText(env.JSON()), nil
	}
}
