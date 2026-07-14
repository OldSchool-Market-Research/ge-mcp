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

// QUERIES #0 — the falsification primitive. LEFT JOIN the liquidity CTE: an
// item can have a fresh quote but no 5m row in the last 15m; return vol5m=0
// rather than dropping the quote. Prices stay nullable.
const quoteSQL = `
WITH q AS (
  SELECT DISTINCT ON (item_id) item_id, ts, high, high_time, low, low_time, margin
  FROM prices_1m WHERE item_id = $1 ORDER BY item_id, ts DESC
),
liq AS (
  SELECT DISTINCT ON (item_id) item_id,
         coalesce(high_volume,0)+coalesce(low_volume,0) AS vol5m
  FROM prices_5m WHERE item_id = $1 AND ts > now() - interval '15 min'
  ORDER BY item_id, ts DESC
)
SELECT q.item_id, i.name, q.ts,
       q.high, q.high_time, extract(epoch from now() - q.high_time)::int AS high_age_s,
       q.low,  q.low_time,  extract(epoch from now() - q.low_time)::int  AS low_age_s,
       q.margin,
       coalesce(liq.vol5m, 0) AS vol5m
FROM q JOIN items i USING (item_id) LEFT JOIN liq USING (item_id)`

type quoteRow struct {
	ItemID   int        `json:"item_id"`
	Name     string     `json:"name"`
	Ts       time.Time  `json:"ts"`
	High     *int64     `json:"high"`
	HighTime *time.Time `json:"high_time"`
	HighAgeS *int       `json:"high_age_s"`
	Low      *int64     `json:"low"`
	LowTime  *time.Time `json:"low_time"`
	LowAgeS  *int       `json:"low_age_s"`
	Margin   *int64     `json:"margin"`
	Vol5m    int64      `json:"vol5m"`
}

func NewQuoteTool() mcp.Tool {
	return mcp.NewTool("quote",
		mcp.WithDescription("Current both-leg snapshot + per-leg freshness for one item (the falsification primitive: are both legs fresh, or is the margin a stale-leg artifact?). margin is post-tax, read from storage, never recomputed. Null prices mean nothing traded that side."),
		mcp.WithString("name_or_id", mcp.Required(), mcp.Description("Item name (fuzzy, best match) or numeric item_id")),
	)
}

func QuoteHandler(pool *pgxpool.Pool) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		nameOrID, err := req.RequireString("name_or_id")
		if err != nil {
			return mcp.NewToolResultError(envelope.ErrorJSON("bad_param", "name_or_id is required")), nil
		}
		res, errResult, err := resolveItem(ctx, pool, nameOrID)
		if err != nil || errResult != nil {
			return errResult, err
		}

		var r quoteRow
		scanErr := pool.QueryRow(ctx, quoteSQL, res.ItemID).Scan(
			&r.ItemID, &r.Name, &r.Ts,
			&r.High, &r.HighTime, &r.HighAgeS,
			&r.Low, &r.LowTime, &r.LowAgeS,
			&r.Margin, &r.Vol5m)

		env := envelope.New([]quoteRow{}, 0)
		env.Resolved = res
		if scanErr == pgx.ErrNoRows {
			// Item exists but has never traded: legitimate empty, not an error.
			env.Note = "no price rows exist for this item"
			return mcp.NewToolResultText(env.JSON()), nil
		}
		if scanErr != nil {
			return nil, scanErr
		}

		env.Rows = []quoteRow{r}
		env.RowCount = 1
		// Latest-row tool: data_window = ts bounds of the returned rows (DESIGN §3).
		env.DataWindow = &envelope.Window{From: r.Ts, To: r.Ts}
		return mcp.NewToolResultText(env.JSON()), nil
	}
}
