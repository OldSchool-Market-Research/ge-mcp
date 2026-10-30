package tools

import (
	"context"
	"errors"
	"fmt"
	"math"
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
//
// Every DB error goes through processFail: ge-agent fails the whole run on a
// raw handler error, and a half-applied schema (04-05 without 06) or a
// missing grant is a deploy state, not a reason to lose a run.

// processUnavailable converts a missing relation (42P01) or a missing grant
// (42501, e.g. DDL applied by a role other than ge-data so the default
// privileges didn't attach) into a typed tool error.
func processUnavailable(err error) *mcp.CallToolResult {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && (pgErr.Code == "42P01" || pgErr.Code == "42501") {
		return mcp.NewToolResultError(envelope.ErrorJSON("process_unavailable",
			"the processing schema (ge-data init/04-06) is not present or not readable in this database: processing tools are unavailable; note it and continue"))
	}
	return nil
}

// processFail is the one exit for a DB error: typed when it's a deploy state,
// raw otherwise.
func processFail(err error) (*mcp.CallToolResult, error) {
	if r := processUnavailable(err); r != nil {
		return r, nil
	}
	return nil, err
}

// recipeArg reads `recipe` as a string or a JSON number: process_screen
// returns recipe_id as an integer, so agents send it back as one.
func recipeArg(req mcp.CallToolRequest) string {
	switch v := req.GetArguments()["recipe"].(type) {
	case string:
		return v
	case float64:
		return strconv.FormatInt(int64(v), 10)
	}
	return ""
}

type resolvedRecipe struct {
	RecipeID int
	Name     string
}

// resolveRecipe takes a numeric recipe_id or a name: exact match first, then
// the shortest name containing it.
func resolveRecipe(ctx context.Context, pool *pgxpool.Pool, nameOrID string) (*resolvedRecipe, *mcp.CallToolResult, error) {
	nameOrID = strings.TrimSpace(nameOrID)
	if nameOrID == "" {
		return nil, mcp.NewToolResultError(envelope.ErrorJSON("bad_param", "recipe must be a recipe_id (e.g. 150) or a recipe name")), nil
	}
	var r resolvedRecipe
	var err error
	if n, convErr := strconv.ParseInt(nameOrID, 10, 32); convErr == nil {
		err = pool.QueryRow(ctx, `SELECT recipe_id, name FROM process_recipes WHERE recipe_id = $1`, n).Scan(&r.RecipeID, &r.Name)
	} else if errors.Is(convErr, strconv.ErrRange) {
		err = pgx.ErrNoRows // numeric but outside int4: no such id
	} else {
		err = pool.QueryRow(ctx, `
			SELECT recipe_id, name FROM process_recipes
			WHERE lower(name) LIKE '%' || lower($1) || '%' ESCAPE '\'
			ORDER BY (lower(name) = lower($2)) DESC, length(name), name
			LIMIT 1`, escapeLike(nameOrID), nameOrID).Scan(&r.RecipeID, &r.Name)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, mcp.NewToolResultError(envelope.ErrorJSON("recipe_not_found", "no processing recipe matches "+nameOrID+"; find names and ids with process_screen")), nil
	}
	if err != nil {
		res, err := processFail(err)
		return nil, res, err
	}
	return &r, nil, nil
}

// processQuoteRow mirrors process_quotes_now. Numerics are *float64: NULL is
// "unpriceable right now" (a leg had no traded side) or, for gp_hr on
// throughput=unknown recipes, "no rate"; it must reach the agent as null.
type processQuoteRow struct {
	RecipeID                     int      `json:"recipe_id"`
	Name                         string   `json:"name"`
	Category                     string   `json:"category"`
	Throughput                   string   `json:"throughput"`
	MarginSlow                   *float64 `json:"margin_slow"`
	MarginInsta                  *float64 `json:"margin_insta"`
	GpHr                         *float64 `json:"gp_hr"`
	GpHrInsta                    *float64 `json:"gp_hr_insta"`
	GpHrTheoretical              *float64 `json:"gp_hr_theoretical"`
	ActionsPerHour               *float64 `json:"actions_per_hour"`
	EffectiveActionsPerHour      *float64 `json:"effective_actions_per_hour"`
	LimitActionsPerHour          *float64 `json:"limit_actions_per_hour"`
	MarketActionsPerHour         *float64 `json:"market_actions_per_hour"`
	EffectiveActionsPerHourInsta *float64 `json:"effective_actions_per_hour_insta"`
	MarketActionsPerHourInsta    *float64 `json:"market_actions_per_hour_insta"`
	FeeGp                        float64  `json:"fee_gp"`
	MissingLegs                  int64    `json:"missing_legs"`
	MissingLegsInsta             int64    `json:"missing_legs_insta"`
	UnknownLimitLegs             int64    `json:"unknown_limit_legs"`
	MaxLegAgeS                   *int64   `json:"max_leg_age_s"`
}

const processQuoteCols = `recipe_id, name, category, throughput, margin_slow, margin_insta, gp_hr,
  gp_hr_insta, gp_hr_theoretical, actions_per_hour, effective_actions_per_hour,
  limit_actions_per_hour, market_actions_per_hour, effective_actions_per_hour_insta,
  market_actions_per_hour_insta, fee_gp, missing_legs, missing_legs_insta,
  unknown_limit_legs, max_leg_age_s`

func scanProcessQuote(row pgx.Row, extra ...any) (processQuoteRow, error) {
	var r processQuoteRow
	dest := append([]any{&r.RecipeID, &r.Name, &r.Category, &r.Throughput, &r.MarginSlow, &r.MarginInsta,
		&r.GpHr, &r.GpHrInsta, &r.GpHrTheoretical, &r.ActionsPerHour, &r.EffectiveActionsPerHour,
		&r.LimitActionsPerHour, &r.MarketActionsPerHour, &r.EffectiveActionsPerHourInsta,
		&r.MarketActionsPerHourInsta, &r.FeeGp, &r.MissingLegs, &r.MissingLegsInsta,
		&r.UnknownLimitLegs, &r.MaxLegAgeS}, extra...)
	return r, row.Scan(dest...)
}

// windowParam parses `window` (default 7d, max 90d).
func windowParam(req mcp.CallToolRequest) (time.Duration, *mcp.CallToolResult) {
	window, errResult := durationParam(req, "window", "7d")
	if errResult != nil {
		return 0, errResult
	}
	if window > 90*24*time.Hour {
		return 0, mcp.NewToolResultError(envelope.ErrorJSON("bad_param", "window: max 90d"))
	}
	return window, nil
}

// categoryParam reads the optional category substring filter, LIKE-escaped.
func categoryParam(req mcp.CallToolRequest) *string {
	c := strings.TrimSpace(req.GetString("category", ""))
	if c == "" {
		return nil
	}
	c = escapeLike(c)
	return &c
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
		mcp.WithDescription("Rank ALL processing recipes (player-time money-makers: planks, keels, hull parts, potions, fish crates, GWD breakdowns...) in one call by live gp/hr. margin_slow = buy inputs at low, sell outputs at high minus GE tax, minus NPC fee, per action; margin_insta = the reverse prices. gp_hr = margin_slow x effective_actions_per_hour = LEAST(player actions/hr, buy-limit rate, 24h market-volume rate); gp_hr_insta uses the insta fill side's volume. gp_hr_theoretical is player speed only and overstates thin items. Null margin with missing_legs (slow) / missing_legs_insta > 0: a leg has no traded side (unpriceable now, not zero). throughput=unknown has no rate, so gp_hr is null: rank those with sort=margin_slow. unknown_limit_legs > 0: the limit cap is optimistic. Skill/quest requirements are NOT curated; read notes via process_quote and surface the caveat. History: process_history / process_top."),
		mcp.WithString("category", mcp.Description("Optional substring filter on category, e.g. 'Hull', 'Herblore', 'Plank'")),
		mcp.WithString("throughput", mcp.Enum("fixed", "volume_capped", "volume_bound", "unknown"), mcp.Description("Optional throughput-kind filter")),
		mcp.WithString("sort", mcp.Enum("gp_hr", "gp_hr_insta", "gp_hr_theoretical", "margin_slow"), mcp.Description("Ranking column (default gp_hr)")),
		mcp.WithNumber("limit", mcp.Description("Rows returned (default 25, max 100); meta.screened says how many matched")),
	)
}

func ProcessScreenHandler(pool *pgxpool.Pool) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var throughput *string
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
		limit := clampLimit(req.GetInt("limit", 25))

		rows, err := pool.Query(ctx, sprintfSQL(processScreenSQL, sortCol), categoryParam(req), throughput, limit)
		if err != nil {
			return processFail(err)
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
			return processFail(err)
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
SELECT source, coalesce(relation_name, ''), coalesce(relation_direction, ''), coalesce(notes, '')
FROM process_recipes WHERE recipe_id = $1`

const processLegsSQL = `
SELECT side, item_id, name, qty, buy_limit,
       CASE side WHEN 'buy' THEN low ELSE high END AS price_slow,
       CASE side WHEN 'buy' THEN high ELSE low END AS price_insta,
       extract(epoch FROM now() - CASE side WHEN 'buy' THEN low_time ELSE high_time END)::bigint AS slow_age_s,
       CASE side WHEN 'buy' THEN low_vol_24h ELSE high_vol_24h END AS fill_volume_24h
FROM process_legs_now WHERE recipe_id = $1
ORDER BY side, item_id`

const processQuoteSQL = `SELECT ` + processQuoteCols + ` FROM process_quotes_now WHERE recipe_id = $1`

type processLeg struct {
	Side string `json:"side"`
	// pointers: init/06 treats a leg with a missing item_id or qty as an
	// unpriceable recipe, not an error, so it must scan rather than fail the run
	ItemID        *int     `json:"item_id"`
	Name          *string  `json:"name"`
	Qty           *float64 `json:"qty"`
	BuyLimit      *int64   `json:"buy_limit"`
	PriceSlow     *int64   `json:"price_slow"`
	PriceInsta    *int64   `json:"price_insta"`
	SlowAgeS      *int64   `json:"slow_age_s"`
	FillVolume24h *float64 `json:"fill_volume_24h"`
}

func NewProcessQuoteTool() mcp.Tool {
	return mcp.NewTool("process_quote",
		mcp.WithDescription("One processing recipe in detail: every leg (side buy/sell, qty per action (can be fractional: expected yields), buy_limit, price_slow/price_insta, slow_age_s = age of the price_slow quote, fill_volume_24h = 24h trades on the side a patient offer fills against) plus the same summary process_screen returns, and the recipe's source, notes (corrections, caveats) and linked item_relations name if any. meta.harness is the orchestrator's own arithmetic for a lane-P pitch: entry_per_action and exit_per_action (your entry_price and exit_price), gp_hr after slippage and capped at 15% of market volume, units_per_4h (your units_used), and shippable with the reason if not. Call it last before shipping a P strategy. Use it to see which leg limits a recipe; a null buy_limit means the limit is unknown, so the limit rate ignores that leg and is optimistic."),
		mcp.WithString("recipe", mcp.Required(), mcp.Description("recipe_id from process_screen, or a recipe name")),
	)
}

func ProcessQuoteHandler(pool *pgxpool.Pool) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		rec, errResult, err := resolveRecipe(ctx, pool, recipeArg(req))
		if err != nil || errResult != nil {
			return errResult, err
		}

		// One read-only snapshot (and one now()) so the leg prices reproduce
		// the summary even if a price row lands between the statements.
		tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
		if err != nil {
			return nil, err
		}
		defer tx.Rollback(ctx)

		var source, relName, relDir, notes string
		if err := tx.QueryRow(ctx, processRecipeSQL, rec.RecipeID).Scan(&source, &relName, &relDir, &notes); err != nil {
			return processFail(err)
		}
		summary, err := scanProcessQuote(tx.QueryRow(ctx, processQuoteSQL, rec.RecipeID))
		if err != nil {
			return processFail(err)
		}
		rows, err := tx.Query(ctx, processLegsSQL, rec.RecipeID)
		if err != nil {
			return processFail(err)
		}
		defer rows.Close()
		legs := []processLeg{}
		for rows.Next() {
			var l processLeg
			if err := rows.Scan(&l.Side, &l.ItemID, &l.Name, &l.Qty, &l.BuyLimit, &l.PriceSlow, &l.PriceInsta, &l.SlowAgeS, &l.FillVolume24h); err != nil {
				return nil, err
			}
			legs = append(legs, l)
		}
		if err := rows.Err(); err != nil {
			return processFail(err)
		}

		var persist processPersistence
		if err := tx.QueryRow(ctx, processPersistenceSQL, rec.RecipeID, processFloorGpHr).Scan(&persist.Hours, &persist.AtFloor); err != nil {
			return processFail(err)
		}

		env := envelope.New(legs, len(legs))
		env.Meta = map[string]any{"summary": summary, "source": source, "harness": harnessQuote(summary, legs, relName != "", persist)}
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

// Lane P's ship-time arithmetic, deliberate duplicates of the orchestrator's
// (separate Go modules; the same pattern as flip_quote):
//
//	participation 0.15, slippage 0.005 -> ge-orchestrator internal/eval/eval.go
//	floor 250k gp/hr, budget 50M       -> internal/eval/process.go, brief.Defaults
//	12 hours, 80% at the floor         -> internal/runner/vet.go
//	6h leg age                         -> internal/eval/combo.go
//
// Change any of those there and this must move in the same PR train.
const (
	processParticipation = 0.15
	processSlippage      = 0.005
	processFloorGpHr     = 250_000
	processBudgetGp      = 50_000_000
	processMinHours      = 12
	processMinAtFloor    = 0.8
	processMaxLegAgeS    = 6 * 3600
)

// processPersistenceSQL counts the trailing 24h's hourly snapshots: how many exist,
// and how many paid the floor. A spike one hour doesn't make a recipe worth shipping.
const processPersistenceSQL = `
SELECT count(*), count(*) FILTER (WHERE gp_hr >= $2)
FROM process_snapshots WHERE recipe_id = $1 AND ts > now() - interval '24 hours'`

type processPersistence struct{ Hours, AtFloor int }

// processHarness is what the orchestrator will compute for a lane-P pitch. The
// view's gp_hr assumes 100% of market volume and exact prices, which the harness
// doesn't: Making Anguish Uncut reads 3.2M there and about 159k here. Nulls are
// signal: an unpriceable recipe has null numbers, never zero. The numbers a strategy
// copies are integers: submit_report's schema takes nothing else, and the client
// rejects the whole call on a miss.
type processHarness struct {
	EntryPerAction  *int64   `json:"entry_per_action"`   // input cost + fee; the strategy's entry_price
	ExitPerAction   *int64   `json:"exit_per_action"`    // post-tax output; the strategy's exit_price
	MarginHaircut   *float64 `json:"margin_haircut"`     // margin_slow minus slippage both sides
	ActionsPerHour  *float64 `json:"actions_per_hour"`   // units_per_4h / 4
	GpHr            *int64   `json:"gp_hr"`              // what realized pace is judged against
	UnitsPer4h      *int64   `json:"units_per_4h"`       // min(player, buy limit, 15% of market, budget) x 4; the strategy's size.units_used
	OutputItem      *int64   `json:"output_item"`        // the recipe's main output; the strategy's items[0]
	Hours24h        int      `json:"hours_24h"`          // hourly snapshots in the last 24h
	HoursAtFloor24h int      `json:"hours_at_floor_24h"` // how many of those read at least the floor
	MaxLegAgeS      *int64   `json:"max_leg_age_s"`      // oldest leg's last trade
	ClearsFloor     bool     `json:"clears_p_floor"`
	Shippable       bool     `json:"shippable"`
	Why             string   `json:"why,omitempty"` // why not shippable
}

// mainOutput is the sell leg the recipe makes most of, lowest id on a tie: the same
// pick as the orchestrator's sweep, so a strategy and its signal name one item.
func mainOutput(legs []processLeg) *int64 {
	var best *processLeg
	for i := range legs {
		l := &legs[i]
		if l.Side != "sell" || l.Qty == nil || l.ItemID == nil {
			continue
		}
		if best == nil || *l.Qty > *best.Qty || (*l.Qty == *best.Qty && *l.ItemID < *best.ItemID) {
			best = l
		}
	}
	if best == nil {
		return nil
	}
	id := int64(*best.ItemID)
	return &id
}

func harnessQuote(q processQuoteRow, legs []processLeg, linked bool, p processPersistence) processHarness {
	h := processHarness{OutputItem: mainOutput(legs), Hours24h: p.Hours, HoursAtFloor24h: p.AtFloor, MaxLegAgeS: q.MaxLegAgeS}
	switch {
	case linked:
		h.Why = "this recipe is a C relation: ship it through lane C with combo_quote"
	case q.Throughput == "unknown":
		h.Why = "no known rate: the harness can't size it"
	case q.UnknownLimitLegs > 0:
		h.Why = "a leg has no known buy limit"
	}
	var in, out float64
	for _, l := range legs {
		if l.Qty == nil || l.PriceSlow == nil {
			if h.Why == "" {
				h.Why = "a leg has no price right now"
			}
			return h
		}
		if l.Side == "buy" {
			in += *l.Qty * float64(*l.PriceSlow)
		} else {
			out += *l.Qty * float64(*l.PriceSlow)
		}
	}
	if q.MarginSlow == nil || q.MarketActionsPerHour == nil || in <= 0 {
		if h.Why == "" {
			h.Why = "a leg has no price right now"
		}
		return h
	}
	rawEntry := in + q.FeeGp
	entry := int64(math.Round(rawEntry))
	exit := int64(math.Round(rawEntry + *q.MarginSlow))
	margin := math.Round((*q.MarginSlow-processSlippage*(in+out))*100) / 100
	rate := processParticipation * *q.MarketActionsPerHour
	if budget := processBudgetGp / in / 4; budget < rate {
		rate = budget
	}
	for _, bound := range []*float64{q.ActionsPerHour, q.LimitActionsPerHour} {
		if bound != nil && *bound < rate {
			rate = *bound
		}
	}
	// Whole actions per 4h, as the orchestrator sizes it from size.units_used.
	units := int64(rate * 4)
	perHour := float64(units) / 4
	gp := int64((*q.MarginSlow - processSlippage*(in+out)) * perHour)
	h.EntryPerAction, h.ExitPerAction, h.MarginHaircut = &entry, &exit, &margin
	h.ActionsPerHour, h.GpHr, h.UnitsPer4h = &perHour, &gp, &units
	h.ClearsFloor = gp >= processFloorGpHr
	if h.Why == "" {
		switch {
		case !h.ClearsFloor:
			h.Why = "under the 250k gp/hr floor at honest size"
		case q.MaxLegAgeS != nil && *q.MaxLegAgeS > processMaxLegAgeS:
			h.Why = "a leg hasn't traded in over 6h, so its price can't be trusted"
		case p.Hours < processMinHours:
			h.Why = fmt.Sprintf("only %d hourly snapshots in the last 24h, under the %d the gate needs", p.Hours, processMinHours)
		case float64(p.AtFloor) < processMinAtFloor*float64(p.Hours):
			h.Why = fmt.Sprintf("read at the floor in only %d of the last %d hourly snapshots, under the gate of 80%%", p.AtFloor, p.Hours)
		}
	}
	h.Shippable = h.Why == ""
	return h
}

// ---------------------------------------------------------------------------
// process_history
// ---------------------------------------------------------------------------

// Bucket width comes from processBuckets (closed set) and is bound as an
// interval parameter, never spliced. priced_hours = hours with a margin,
// the same definition process_top uses.
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

var processBuckets = map[string]struct {
	interval string
	width    time.Duration
}{
	"1h": {"1 hour", time.Hour},
	"6h": {"6 hours", 6 * time.Hour},
	"1d": {"1 day", 24 * time.Hour},
}

// maxHistoryBuckets keeps one tool result to a few hundred rows.
const maxHistoryBuckets = 400

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
		mcp.WithDescription("Trend for ONE processing recipe from the hourly snapshots: per bucket the average margin_slow, margin_insta, gp_hr, gp_hr_insta and effective actions/hr, plus hours (snapshots in the bucket) and priced_hours (hours with a margin). Averages cover hours with a value; priced_hours < hours means the recipe was unpriceable part of the bucket. gp_hr stays null for throughput=unknown recipes (no rate): read margin_slow. History starts when the snapshot job first ran. At most 400 buckets per call (1h up to 16d, 6h up to 90d)."),
		mcp.WithString("recipe", mcp.Required(), mcp.Description("recipe_id from process_screen, or a recipe name")),
		mcp.WithString("window", mcp.Description("How far back, <n><m|h|d> (default 7d, max 90d)")),
		mcp.WithString("bucket", mcp.Enum("1h", "6h", "1d"), mcp.Description("Bucket width (default 6h)")),
	)
}

func ProcessHistoryHandler(pool *pgxpool.Pool) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		rec, errResult, err := resolveRecipe(ctx, pool, recipeArg(req))
		if err != nil || errResult != nil {
			return errResult, err
		}
		window, errResult := windowParam(req)
		if errResult != nil {
			return errResult, nil
		}
		bucketKey := req.GetString("bucket", "6h")
		bucket, ok := processBuckets[bucketKey]
		if !ok {
			return mcp.NewToolResultError(envelope.ErrorJSON("bad_param", "bucket must be 1h, 6h or 1d")), nil
		}
		if window/bucket.width > maxHistoryBuckets {
			return mcp.NewToolResultError(envelope.ErrorJSON("bad_param",
				"window/bucket exceeds 400 buckets: use bucket=1h up to 16d, bucket=6h up to 90d")), nil
		}
		to := time.Now().UTC()
		from := to.Add(-window)

		rows, err := pool.Query(ctx, processHistorySQL, rec.RecipeID, from, bucket.interval)
		if err != nil {
			return processFail(err)
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
			return processFail(err)
		}

		env := envelope.New(out, len(out))
		env.DataWindow = &envelope.Window{From: from, To: to}
		env.Meta = map[string]any{"recipe_id": rec.RecipeID, "name": rec.Name, "bucket": bucketKey}
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
// hours_in_top10 means top 10 of the whole universe. avg/median/p25 cover
// hours with a metric value; $4 is the coverage floor (metric_hours / hours)
// that keeps a mostly-unpriceable recipe from ranking on a few good hours.
const processTopSQL = `
WITH s AS (
  SELECT recipe_id, ts, margin_slow, %s AS v,
         rank() OVER (PARTITION BY ts ORDER BY %[1]s DESC NULLS LAST) AS rk
  FROM process_snapshots WHERE ts >= $1
)
SELECT s.recipe_id, p.name, p.category,
       count(*)             AS hours,
       count(s.margin_slow) AS priced_hours,
       count(s.v)           AS metric_hours,
       round(avg(s.v)) AS avg,
       round(percentile_cont(0.5)  WITHIN GROUP (ORDER BY s.v)::numeric) AS median,
       round(percentile_cont(0.25) WITHIN GROUP (ORDER BY s.v)::numeric) AS p25,
       round(100.0 * count(*) FILTER (WHERE s.v > 0) / count(*), 1) AS pct_hours_positive,
       count(*) FILTER (WHERE s.rk <= 10 AND s.v IS NOT NULL) AS hours_in_top10,
       round(avg(s.v) FILTER (WHERE s.ts >= now() - interval '24 hours')) AS avg_last_24h
FROM s JOIN process_recipes p USING (recipe_id)
WHERE ($2::text IS NULL OR p.category ILIKE '%%' || $2 || '%%')
GROUP BY s.recipe_id, p.name, p.category
HAVING count(s.v) >= $4::numeric / 100 * count(*)
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
	MetricHours      int64    `json:"metric_hours"`
	Avg              *float64 `json:"avg"`
	Median           *float64 `json:"median"`
	P25              *float64 `json:"p25"`
	PctHoursPositive *float64 `json:"pct_hours_positive"`
	HoursInTop10     int64    `json:"hours_in_top10"`
	AvgLast24h       *float64 `json:"avg_last_24h"`
}

func NewProcessTopTool() mcp.Tool {
	return mcp.NewTool("process_top",
		mcp.WithDescription("Top processing recipes over a window, from the hourly snapshots, ranked by MEDIAN of the metric (robust to one-hour spikes). Per recipe: hours, priced_hours (hours with a margin), metric_hours (hours with a metric value), avg / median / p25 over metric_hours, pct_hours_positive (of ALL hours), hours_in_top10 (hours ranked top 10 of ALL recipes), avg_last_24h vs avg (rising or fading). Recipes no longer in the catalog are omitted. Recipes with the metric in fewer than min_coverage_pct of hours are left out. Prefer a high p25 with pct_hours_positive near 100: consistently good beats occasionally great. throughput=unknown recipes have no gp_hr: use metric=margin_slow. margin_slow ignores volume, so a frozen stale leg looks consistent: confirm with process_quote's slow_age_s."),
		mcp.WithString("window", mcp.Description("How far back, <n><m|h|d> (default 7d, max 90d)")),
		mcp.WithString("metric", mcp.Enum("gp_hr", "gp_hr_insta", "margin_slow"), mcp.Description("Metric to rank on (default gp_hr)")),
		mcp.WithString("category", mcp.Description("Optional substring filter on category")),
		mcp.WithNumber("min_coverage_pct", mcp.Description("Minimum % of hours with a metric value (default 50, 0-100)")),
		mcp.WithNumber("limit", mcp.Description("Rows (default 20, max 100)")),
	)
}

func ProcessTopHandler(pool *pgxpool.Pool) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		window, errResult := windowParam(req)
		if errResult != nil {
			return errResult, nil
		}
		metric, ok := processTopMetrics[req.GetString("metric", "gp_hr")]
		if !ok {
			return mcp.NewToolResultError(envelope.ErrorJSON("bad_param", "metric must be gp_hr, gp_hr_insta or margin_slow")), nil
		}
		coverage := req.GetInt("min_coverage_pct", 50)
		if coverage < 0 || coverage > 100 {
			return mcp.NewToolResultError(envelope.ErrorJSON("bad_param", "min_coverage_pct must be 0-100")), nil
		}
		category := categoryParam(req)
		limit := clampLimit(req.GetInt("limit", 20))
		to := time.Now().UTC()
		from := to.Add(-window)

		rows, err := pool.Query(ctx, sprintfSQL(processTopSQL, metric), from, category, limit, coverage)
		if err != nil {
			return processFail(err)
		}
		defer rows.Close()
		out := []processTopRow{}
		for rows.Next() {
			var r processTopRow
			if err := rows.Scan(&r.RecipeID, &r.Name, &r.Category, &r.Hours, &r.PricedHours, &r.MetricHours, &r.Avg,
				&r.Median, &r.P25, &r.PctHoursPositive, &r.HoursInTop10, &r.AvgLast24h); err != nil {
				return nil, err
			}
			out = append(out, r)
		}
		if err := rows.Err(); err != nil {
			return processFail(err)
		}

		env := envelope.New(out, len(out))
		env.DataWindow = &envelope.Window{From: from, To: to}
		env.Meta = map[string]any{"metric": metric, "min_coverage_pct": coverage}
		if len(out) == 0 {
			if category != nil {
				env.Note = "no recipes in this category met the coverage floor in this window"
			} else {
				env.Note = "no recipes met the coverage floor in this window: history may not have started yet (one snapshot per recipe per hour), or lower min_coverage_pct"
			}
		}
		return mcp.NewToolResultText(env.JSON()), nil
	}
}
