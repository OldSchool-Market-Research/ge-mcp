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

// QUERIES #22 (flip-quote, 2026-08). The ship-time sizing primitive for
// lanes F/B: the orchestrator's exact veto arithmetic, exposed BEFORE the
// pitch instead of discovered in a veto message after it. The first
// post-gate week vetoed 35 of 37 ships on "claimed per_cycle_gp exceeds 2x
// the live recomputation" — arithmetic the agent literally could not run.
//
// The constants are deliberate duplicates of the orchestrator's evaluator
// (separate Go modules cannot share code — the same established pattern as
// top_flips mirroring the lane floors):
//
//	participation 0.15, slippage 0.005, cycle 4h, vol window 30m x 8
//	  -> ge-orchestrator/internal/eval/eval.go (defaultParticipation,
//	     defaultSlippage), flip.go (flipCycleHours, ProjectFlipPer1h)
//	fillable = max(1, floor(0.15 x vol30m x 8)); ceiling = margin x
//	  min(units, fillable); veto above 2 x ceiling
//	  -> ge-orchestrator/internal/runner/vet.go (evSanity, evSanityFactor)
//
// Change any of those there and this tool must move in the same PR train.
//
// Nulls are signal: a missing leg keeps margin/ceiling/projection null with
// a note — never zero-filled.
// vol30m matches the orchestrator snapshot exactly (eval/source.go
// snapshotSQL): both sides summed over the last 30 minutes of prices_5m.
var flipQuoteSQL = `
WITH q AS (
  SELECT DISTINCT ON (item_id) item_id, ts, high, high_time, low, low_time, margin
  FROM prices_1m WHERE item_id = $1 ORDER BY item_id, ts DESC
),
vol AS (
  SELECT coalesce(sum(coalesce(high_volume,0)+coalesce(low_volume,0)),0) AS vol30m
  FROM prices_5m WHERE item_id = $1 AND ts > now() - interval '30 minutes'
)
SELECT q.item_id, i.name, i.buy_limit, q.ts,
       q.high, extract(epoch from now() - q.high_time)::int AS high_age_s,
       q.low,  extract(epoch from now() - q.low_time)::int  AS low_age_s,
       q.margin,
       v.vol30m,
       ` + persistenceSelect("q.margin") + `
FROM q JOIN items i USING (item_id) CROSS JOIN vol v
` + persistenceJoins("q.item_id", "q.margin")

const (
	// vet.go evSanity mirrors (see header comment).
	flipParticipation = 0.15
	flipSlippage      = 0.005
	flipCycleHours    = 4
	flipVetFactor     = 2
	flipSellTaxCap    = int64(5_000_000)
)

func flipSellTax(p int64) int64 {
	t := p / 50
	if t > flipSellTaxCap {
		return flipSellTaxCap
	}
	return t
}

type flipQuoteRow struct {
	ItemID   int       `json:"item_id"`
	Name     string    `json:"name"`
	BuyLimit *int64    `json:"buy_limit"`
	Ts       time.Time `json:"ts"`
	High     *int64    `json:"high"`
	HighAgeS *int      `json:"high_age_s"`
	Low      *int64    `json:"low"`
	LowAgeS  *int      `json:"low_age_s"`
	Margin   *int64    `json:"margin"`
	Vol30m   int64     `json:"vol30m"`

	MarginPersistence24h *float64 `json:"margin_persistence_24h"`
	PersistObsHours      int      `json:"persist_obs_hours"`
	Roundtrips24h        int      `json:"roundtrips_24h"`
}

func NewFlipQuoteTool() mcp.Tool {
	return mcp.NewTool("flip_quote",
		mcp.WithDescription("Size a lane-F/B pitch against the orchestrator's OWN ship-time arithmetic — call this (then quote) as the LAST two calls before shipping any flip. Returns fillable_units = max(1, floor(15% of vol30m x 8)) and per_cycle_ceiling = stored post-tax margin x min(units, fillable_units): the harness recomputes exactly this at ship time and VETOES any per_cycle_gp claim above vet_max_claim = 2 x ceiling. Set per_cycle_gp no higher than per_cycle_ceiling. harness_per_1h is the harness's own projection of your offers (slippage 0.5% both sides, sell tax, participation-capped, one 4h cycle) — the denominator your realized pace is judged against. Also carries buy_limit (your other sizing bound) and the persistence stats (the 0.4 lane-F gate reference). Null margin or a missing leg makes ceiling/projection null: the flip is unpriceable right now, not free."),
		mcp.WithString("name_or_id", mcp.Required(), mcp.Description("Item name (fuzzy, best match) or numeric item_id")),
		mcp.WithNumber("units", mcp.Required(), mcp.Description("Your intended units_used (the size field of the pitch)")),
		mcp.WithNumber("entry_price", mcp.Description("Your intended buy offer; defaults to the live low")),
		mcp.WithNumber("exit_price", mcp.Description("Your intended sell offer; defaults to the live high")),
	)
}

func FlipQuoteHandler(pool *pgxpool.Pool) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		nameOrID, err := req.RequireString("name_or_id")
		if err != nil {
			return mcp.NewToolResultError(envelope.ErrorJSON("bad_param", "name_or_id is required")), nil
		}
		units := int64(req.GetInt("units", 0))
		if units <= 0 {
			return mcp.NewToolResultError(envelope.ErrorJSON("bad_param", "units is required and must be positive")), nil
		}
		entryArg := int64(req.GetInt("entry_price", 0))
		exitArg := int64(req.GetInt("exit_price", 0))
		if entryArg < 0 || exitArg < 0 {
			return mcp.NewToolResultError(envelope.ErrorJSON("bad_param", "prices must be positive")), nil
		}
		res, errResult, err := resolveItem(ctx, pool, nameOrID)
		if err != nil || errResult != nil {
			return errResult, err
		}

		var r flipQuoteRow
		scanErr := pool.QueryRow(ctx, flipQuoteSQL, res.ItemID).Scan(
			&r.ItemID, &r.Name, &r.BuyLimit, &r.Ts,
			&r.High, &r.HighAgeS,
			&r.Low, &r.LowAgeS,
			&r.Margin, &r.Vol30m,
			&r.MarginPersistence24h, &r.PersistObsHours, &r.Roundtrips24h)

		env := envelope.New([]flipQuoteRow{}, 0)
		env.Resolved = res
		if scanErr == pgx.ErrNoRows {
			env.Note = "no price rows exist for this item"
			return mcp.NewToolResultText(env.JSON()), nil
		}
		if scanErr != nil {
			return nil, scanErr
		}
		env.Rows = []flipQuoteRow{r}
		env.RowCount = 1
		env.DataWindow = &envelope.Window{From: r.Ts, To: r.Ts}

		// evSanity mirror (vet.go): fillable, ceiling, veto line.
		fillable := int64(flipParticipation * float64(r.Vol30m*8))
		if fillable < 1 {
			fillable = 1
		}
		cappedUnits := units
		if fillable < cappedUnits {
			cappedUnits = fillable
		}
		summary := map[string]any{
			"units_requested": units,
			"fillable_units":  fillable,
			"units_capped":    cappedUnits,
			"vet_factor":      flipVetFactor,
			"buy_limit":       r.BuyLimit,
			"margin_post_tax": r.Margin,
			"vol30m":          r.Vol30m,
		}
		if r.Margin == nil {
			summary["per_cycle_ceiling"] = nil
			summary["vet_max_claim"] = nil
			summary["note"] = "no live post-tax margin (a leg has never traded or the book is one-sided) — the flip is unpriceable right now (null is signal, not zero)"
		} else {
			ceiling := *r.Margin * cappedUnits
			if ceiling < 0 {
				ceiling = 0
			}
			summary["per_cycle_ceiling"] = ceiling
			summary["vet_max_claim"] = ceiling * flipVetFactor
			summary["note"] = "per_cycle_gp claims above vet_max_claim are vetoed at ship time; size at or below per_cycle_ceiling"
		}

		// ProjectFlipPer1h mirror (flip.go): the harness projection your
		// realized pace is judged against. Defaults to the live legs when no
		// explicit offers are given.
		entry, exit := entryArg, exitArg
		if entry == 0 && r.Low != nil {
			entry = *r.Low
		}
		if exit == 0 && r.High != nil {
			exit = *r.High
		}
		if entry > 0 && exit > entry && r.Vol30m > 0 && cappedUnits > 0 {
			slipExit := int64(float64(exit) * (1 - flipSlippage))
			slipEntry := int64(float64(entry) * (1 + flipSlippage))
			perUnit := slipExit - slipEntry - flipSellTax(slipExit)
			if perUnit > 0 {
				summary["harness_per_1h"] = perUnit * cappedUnits / flipCycleHours
			} else {
				summary["harness_per_1h"] = nil
				summary["projection_note"] = "offers do not clear slippage + tax — harness projection is null"
			}
		} else {
			summary["harness_per_1h"] = nil
		}
		summary["entry_used"] = entry
		summary["exit_used"] = exit

		env.Meta = map[string]any{"summary": summary}
		return mcp.NewToolResultText(env.JSON()), nil
	}
}
