package tools

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/osrs-ge/ge-mcp/internal/envelope"
)

// Summed 5m volume over the window (QUERIES building block #2, summed form).
// Volume sums coalesce to 0 by contract — a missing side genuinely contributes
// zero volume (unlike prices, which stay null).
const liquiditySQL = `
SELECT coalesce(sum(high_volume),0) + coalesce(sum(low_volume),0) AS vol5m_total,
       coalesce(sum(high_volume),0) AS high_volume,
       coalesce(sum(low_volume),0)  AS low_volume,
       count(*) AS n_buckets
FROM prices_5m
WHERE item_id = $1 AND ts > $2`

type liquidityRow struct {
	ItemID     int    `json:"item_id"`
	Name       string `json:"name"`
	Vol5mTotal int64  `json:"vol5m_total"`
	HighVolume int64  `json:"high_volume"`
	LowVolume  int64  `json:"low_volume"`
	Window     string `json:"window"`
	NBuckets   int    `json:"n_buckets"`
}

func NewLiquidityTool() mcp.Tool {
	return mcp.NewTool("liquidity",
		mcp.WithDescription("Summed recent 5m volume for one item, for position sizing. Volume is units traded, not transaction count. n_buckets=0 means nothing traded in the window — that is a real liquidity signal, not an error."),
		mcp.WithString("name_or_id", mcp.Required(), mcp.Description("Item name (fuzzy, best match) or numeric item_id")),
		mcp.WithString("window", mcp.Description("Lookback window, e.g. 15min / 1h (default 15min)")),
	)
}

func LiquidityHandler(pool *pgxpool.Pool) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		nameOrID, err := req.RequireString("name_or_id")
		if err != nil {
			return mcp.NewToolResultError(envelope.ErrorJSON("bad_param", "name_or_id is required")), nil
		}
		window, badParam := durationParam(req, "window", "15min")
		if badParam != nil {
			return badParam, nil
		}
		res, errResult, err := resolveItem(ctx, pool, nameOrID)
		if err != nil || errResult != nil {
			return errResult, err
		}

		now := time.Now().UTC()
		from := now.Add(-window)

		r := liquidityRow{ItemID: res.ItemID, Name: res.Name, Window: req.GetString("window", "15min")}
		if err := pool.QueryRow(ctx, liquiditySQL, res.ItemID, from).Scan(
			&r.Vol5mTotal, &r.HighVolume, &r.LowVolume, &r.NBuckets); err != nil {
			return nil, err
		}

		env := envelope.New([]liquidityRow{r}, 1)
		env.Resolved = res
		// Windowed tool: data_window = the scanned ts bounds (DESIGN §3).
		env.DataWindow = &envelope.Window{From: from, To: now}
		if r.NBuckets == 0 {
			env.Note = "no 5m volume rows in window (nothing traded)"
		}
		return mcp.NewToolResultText(env.JSON()), nil
	}
}
