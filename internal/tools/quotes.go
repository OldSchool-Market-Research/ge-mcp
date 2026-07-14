package tools

import (
	"context"
	"fmt"
	"strconv"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/osrs-ge/ge-mcp/internal/envelope"
)

const maxQuotesBatch = 25

// QUERIES #16 — #0 batched with ANY($1).
const quotesSQL = `
WITH q AS (
  SELECT DISTINCT ON (item_id) item_id, ts, high, high_time, low, low_time, margin
  FROM prices_1m WHERE item_id = ANY($1) ORDER BY item_id, ts DESC
),
liq AS (
  SELECT DISTINCT ON (item_id) item_id,
         coalesce(high_volume,0)+coalesce(low_volume,0) AS vol5m
  FROM prices_5m WHERE item_id = ANY($1) AND ts > now() - interval '15 min'
  ORDER BY item_id, ts DESC
)
SELECT q.item_id, i.name, q.ts,
       q.high, q.high_time, extract(epoch from now() - q.high_time)::int AS high_age_s,
       q.low,  q.low_time,  extract(epoch from now() - q.low_time)::int  AS low_age_s,
       q.margin,
       coalesce(liq.vol5m, 0) AS vol5m
FROM q JOIN items i USING (item_id) LEFT JOIN liq USING (item_id)
ORDER BY q.item_id`

func NewQuotesTool() mcp.Tool {
	return mcp.NewTool("quotes",
		mcp.WithDescription("Batch quote for a watchlist (1-25 items) — same row shape as quote. Unresolvable names go to meta.errors instead of failing the call; an item with no price rows is simply absent from rows (compare row_count to the request)."),
		mcp.WithArray("names_or_ids", mcp.Required(), mcp.Description("Item names (fuzzy, best match) and/or numeric ids"), mcp.WithStringItems()),
	)
}

func QuotesHandler(pool *pgxpool.Pool) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		raw, present := req.GetArguments()["names_or_ids"]
		list, ok := raw.([]any)
		if !present || !ok || len(list) == 0 {
			return mcp.NewToolResultError(envelope.ErrorJSON("bad_param", "names_or_ids must be a non-empty array")), nil
		}
		if len(list) > maxQuotesBatch {
			return mcp.NewToolResultError(envelope.ErrorJSON("bad_param", fmt.Sprintf("names_or_ids exceeds the batch limit of %d", maxQuotesBatch))), nil
		}

		ids := make([]int, 0, len(list))
		resolvedByID := map[int]string{}
		var resolveErrors []map[string]string
		for _, v := range list {
			var s string
			switch t := v.(type) {
			case string:
				s = t
			case float64:
				s = strconv.Itoa(int(t))
			default:
				return mcp.NewToolResultError(envelope.ErrorJSON("bad_param", "names_or_ids entries must be strings or numbers")), nil
			}
			res, errResult, err := resolveItem(ctx, pool, s)
			if err != nil {
				return nil, err
			}
			if errResult != nil {
				// Per-item failure: report, don't fail the batch.
				resolveErrors = append(resolveErrors, map[string]string{"input": s, "code": "item_not_found"})
				continue
			}
			if _, dup := resolvedByID[res.ItemID]; !dup {
				resolvedByID[res.ItemID] = res.Name
				ids = append(ids, res.ItemID)
			}
		}

		out := []quoteRow{}
		if len(ids) > 0 {
			rows, err := pool.Query(ctx, quotesSQL, ids)
			if err != nil {
				return nil, err
			}
			defer rows.Close()
			for rows.Next() {
				var r quoteRow
				if err := rows.Scan(&r.ItemID, &r.Name, &r.Ts,
					&r.High, &r.HighTime, &r.HighAgeS,
					&r.Low, &r.LowTime, &r.LowAgeS,
					&r.Margin, &r.Vol5m); err != nil {
					return nil, err
				}
				out = append(out, r)
			}
			if err := rows.Err(); err != nil {
				return nil, err
			}
		}

		env := envelope.New(out, len(out))
		env.Meta = map[string]any{"requested": len(list), "resolved": len(ids)}
		if len(resolveErrors) > 0 {
			env.Meta["errors"] = resolveErrors
		}
		if len(out) > 0 {
			w := envelope.Window{From: out[0].Ts, To: out[0].Ts}
			for _, r := range out {
				if r.Ts.Before(w.From) {
					w.From = r.Ts
				}
				if r.Ts.After(w.To) {
					w.To = r.Ts
				}
			}
			env.DataWindow = &w
		}
		if len(out) == 0 {
			env.Note = "no price rows for any resolved item"
		}
		return mcp.NewToolResultText(env.JSON()), nil
	}
}
