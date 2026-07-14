package tools

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/osrs-ge/ge-mcp/internal/envelope"
)

// QUERIES #5 (+obs). Candles are NOT gap-filled: an empty bucket is an
// omitted row, not an locf carry-forward.
const history1mSQL = `
SELECT time_bucket($1, ts) AS bucket,
       first(high, ts) AS open, max(high) AS high, min(low) AS low, last(high, ts) AS close,
       count(*) AS obs
FROM prices_1m
WHERE item_id = $2 AND ts > $3
GROUP BY bucket ORDER BY bucket`

// QUERIES #5b. prices_5m carries block-average prices, not last-trade quotes —
// a different price basis, tagged in meta. Avg prices stay null when a side
// never traded in the bucket; volume sums coalesce to 0.
const history5mSQL = `
SELECT time_bucket($1, ts) AS bucket,
       avg(avg_high_price)::bigint AS avg_high,
       avg(avg_low_price)::bigint  AS avg_low,
       coalesce(sum(high_volume),0) AS high_volume,
       coalesce(sum(low_volume),0)  AS low_volume,
       count(*) AS obs
FROM prices_5m
WHERE item_id = $2 AND ts > $3
GROUP BY bucket ORDER BY bucket`

type candle1m struct {
	Bucket time.Time `json:"bucket"`
	Open   *int64    `json:"open"`
	High   *int64    `json:"high"`
	Low    *int64    `json:"low"`
	Close  *int64    `json:"close"`
	Obs    int       `json:"obs"`
}

type candle5m struct {
	Bucket     time.Time `json:"bucket"`
	AvgHigh    *int64    `json:"avg_high"`
	AvgLow     *int64    `json:"avg_low"`
	HighVolume int64     `json:"high_volume"`
	LowVolume  int64     `json:"low_volume"`
	Obs        int       `json:"obs"`
}

func NewItemHistoryTool() mcp.Tool {
	return mcp.NewTool("item_history",
		mcp.WithDescription("OHLC / series for one item — the evidence backbone. source=1m gives last-trade OHLC candles; source=5m gives block-average prices (a different price basis) and is the only source with volume. Empty buckets are omitted, never gap-filled."),
		mcp.WithString("name_or_id", mcp.Required(), mcp.Description("Item name (fuzzy, best match) or numeric item_id")),
		mcp.WithString("grain", mcp.Description("Bucket size, e.g. 15min / 1h (default 15min)")),
		mcp.WithString("lookback", mcp.Description("How far back, e.g. 6h / 7d (default 6h). No cap: long research scans are allowed to be slow")),
		mcp.WithString("source", mcp.Description("1m (last-trade OHLC, default) or 5m (block averages + volume)"), mcp.Enum("1m", "5m")),
	)
}

func ItemHistoryHandler(pool *pgxpool.Pool) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		nameOrID, err := req.RequireString("name_or_id")
		if err != nil {
			return mcp.NewToolResultError(envelope.ErrorJSON("bad_param", "name_or_id is required")), nil
		}
		grain, badParam := durationParam(req, "grain", "15min")
		if badParam != nil {
			return badParam, nil
		}
		lookback, badParam := durationParam(req, "lookback", "6h")
		if badParam != nil {
			return badParam, nil
		}
		source := req.GetString("source", "1m")
		// Enum → fixed SQL chosen by switch; the input string never reaches SQL.
		var query string
		switch source {
		case "1m":
			query = history1mSQL
		case "5m":
			query = history5mSQL
		default:
			return mcp.NewToolResultError(envelope.ErrorJSON("bad_param", "source must be 1m or 5m")), nil
		}
		res, errResult, err := resolveItem(ctx, pool, nameOrID)
		if err != nil || errResult != nil {
			return errResult, err
		}

		now := time.Now().UTC()
		from := now.Add(-lookback)

		// Research tool with uncapped lookback (DESIGN §4): lift the pool's
		// point-tool statement_timeout for this transaction only.
		tx, err := pool.Begin(ctx)
		if err != nil {
			return nil, err
		}
		defer tx.Rollback(ctx)
		if _, err := tx.Exec(ctx, "SET LOCAL statement_timeout = 0"); err != nil {
			return nil, err
		}

		var rowsAny any
		var n int
		if source == "1m" {
			rows, err := tx.Query(ctx, query, grain, res.ItemID, from)
			if err != nil {
				return nil, err
			}
			out := []candle1m{}
			for rows.Next() {
				var c candle1m
				if err := rows.Scan(&c.Bucket, &c.Open, &c.High, &c.Low, &c.Close, &c.Obs); err != nil {
					rows.Close()
					return nil, err
				}
				out = append(out, c)
			}
			rows.Close()
			if err := rows.Err(); err != nil {
				return nil, err
			}
			rowsAny, n = out, len(out)
		} else {
			rows, err := tx.Query(ctx, query, grain, res.ItemID, from)
			if err != nil {
				return nil, err
			}
			out := []candle5m{}
			for rows.Next() {
				var c candle5m
				if err := rows.Scan(&c.Bucket, &c.AvgHigh, &c.AvgLow, &c.HighVolume, &c.LowVolume, &c.Obs); err != nil {
					rows.Close()
					return nil, err
				}
				out = append(out, c)
			}
			rows.Close()
			if err := rows.Err(); err != nil {
				return nil, err
			}
			rowsAny, n = out, len(out)
		}
		if err := tx.Commit(ctx); err != nil {
			return nil, err
		}

		env := envelope.New(rowsAny, n)
		env.Resolved = res
		env.DataWindow = &envelope.Window{From: from, To: now}
		env.Meta = map[string]any{
			"source": source,
			"grain":  req.GetString("grain", "15min"),
		}
		if source == "5m" {
			env.Meta["price_basis"] = "block_average" // not last-trade quotes
		}
		if n == 0 {
			env.Note = "no rows in window (nothing traded)"
		}
		return mcp.NewToolResultText(env.JSON()), nil
	}
}
