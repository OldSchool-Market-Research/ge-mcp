package tools

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/osrs-ge/ge-mcp/internal/envelope"
)

// QUERIES #10/#11 (unlocked 2026-07-13 on data age). Scans all history — the
// min/max(ts) window functions report the actual scanned bounds without a
// second pass. Slow is fine (SPEC §6); runs uncapped.
const seasonalityHourSQL = `
SELECT extract(hour from ts)::int AS bucket,
       round(avg(margin))::bigint AS avg_margin,
       count(*) AS obs,
       min(min(ts)) OVER () AS scan_from,
       max(max(ts)) OVER () AS scan_to
FROM prices_1m
WHERE margin IS NOT NULL AND ($1::int IS NULL OR item_id = $1)
GROUP BY bucket ORDER BY bucket`

const seasonalityDowSQL = `
SELECT extract(dow from ts)::int AS bucket,
       round(avg(margin))::bigint AS avg_margin,
       count(*) AS obs,
       min(min(ts)) OVER () AS scan_from,
       max(max(ts)) OVER () AS scan_to
FROM prices_1m
WHERE margin IS NOT NULL AND ($1::int IS NULL OR item_id = $1)
GROUP BY bucket ORDER BY bucket`

type seasonalityRow struct {
	Bucket    int   `json:"bucket"`
	AvgMargin int64 `json:"avg_margin"`
	Obs       int64 `json:"obs"`
}

func NewSeasonalityTool() mcp.Tool {
	return mcp.NewTool("seasonality",
		mcp.WithDescription("Hour-of-day (bucket 0-23 UTC) or day-of-week (bucket 0-6, 0=Sunday) structure in post-tax margins. Global across all items, or filtered to one via name_or_id. Scans all accumulated history — may take a while; obs keeps young dow buckets honest (~4 samples of each weekday per month of data)."),
		mcp.WithString("dimension", mcp.Required(), mcp.Enum("hour", "dow")),
		mcp.WithString("name_or_id", mcp.Description("Optional single-item filter (fuzzy name or numeric id); global when omitted")),
	)
}

func SeasonalityHandler(pool *pgxpool.Pool) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		dimension, err := req.RequireString("dimension")
		if err != nil {
			return mcp.NewToolResultError(envelope.ErrorJSON("bad_param", "dimension is required")), nil
		}
		var query string
		switch dimension {
		case "hour":
			query = seasonalityHourSQL
		case "dow":
			query = seasonalityDowSQL
		default:
			return mcp.NewToolResultError(envelope.ErrorJSON("bad_param", "dimension must be hour or dow")), nil
		}

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

		// Full-history scan: run uncapped.
		tx, err := pool.Begin(ctx)
		if err != nil {
			return nil, err
		}
		defer tx.Rollback(ctx)
		if _, err := tx.Exec(ctx, "SET LOCAL statement_timeout = 0"); err != nil {
			return nil, err
		}
		rows, err := tx.Query(ctx, query, itemID)
		if err != nil {
			return nil, err
		}

		out := []seasonalityRow{}
		var scanFrom, scanTo *time.Time
		for rows.Next() {
			var r seasonalityRow
			if err := rows.Scan(&r.Bucket, &r.AvgMargin, &r.Obs, &scanFrom, &scanTo); err != nil {
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
		env.Meta = map[string]any{"dimension": dimension}
		if scanFrom != nil && scanTo != nil {
			env.DataWindow = &envelope.Window{From: *scanFrom, To: *scanTo}
		}
		if len(out) == 0 {
			env.Note = "no margin observations for this scope"
		}
		return mcp.NewToolResultText(env.JSON()), nil
	}
}
