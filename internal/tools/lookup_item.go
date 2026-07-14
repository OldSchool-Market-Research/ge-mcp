package tools

import (
	"context"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/osrs-ge/ge-mcp/internal/envelope"
)

// Ranked fuzzy match (SPEC §5.1): exact lower(name) → prefix → substring.
// $1 is the LIKE-escaped query (substring + prefix tests), $2 the raw query
// (exact test). items is small and static, so the substring seq scan is fine;
// exact/prefix ride items_name_lower_idx.
const lookupItemSQL = `
SELECT item_id, name, members, buy_limit, value, lowalch, highalch, icon
FROM items
WHERE lower(name) LIKE '%' || lower($1) || '%' ESCAPE '\'
ORDER BY (lower(name) = lower($2)) DESC,
         (lower(name) LIKE lower($1) || '%' ESCAPE '\') DESC,
         name
LIMIT $3`

type itemRow struct {
	ItemID   int    `json:"item_id"`
	Name     string `json:"name"`
	Members  *bool  `json:"members"`
	BuyLimit *int   `json:"buy_limit"`
	Value    *int   `json:"value"`
	Lowalch  *int   `json:"lowalch"`
	Highalch *int   `json:"highalch"`
	Icon     *string `json:"icon"`
}

func NewLookupItemTool() mcp.Tool {
	return mcp.NewTool("lookup_item",
		mcp.WithDescription("Resolve an item name to item metadata (fuzzy, ranked: exact > prefix > substring). The only source of buy_limit / members / alch values."),
		mcp.WithString("query", mcp.Required(), mcp.Description("Item name or fragment, case-insensitive")),
		mcp.WithNumber("limit", mcp.Description("Max candidates to return (1-100, default 10)")),
	)
}

func LookupItemHandler(pool *pgxpool.Pool) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		query, err := req.RequireString("query")
		if err != nil || strings.TrimSpace(query) == "" {
			return mcp.NewToolResultError(envelope.ErrorJSON("bad_param", "query must be a non-empty string")), nil
		}
		limit := clampLimit(req.GetInt("limit", 10))

		rows, err := pool.Query(ctx, lookupItemSQL, escapeLike(query), query, limit)
		if err != nil {
			return nil, err
		}
		defer rows.Close()

		items := []itemRow{}
		for rows.Next() {
			var r itemRow
			if err := rows.Scan(&r.ItemID, &r.Name, &r.Members, &r.BuyLimit, &r.Value, &r.Lowalch, &r.Highalch, &r.Icon); err != nil {
				return nil, err
			}
			items = append(items, r)
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}

		// No fuzzy match at all is bad input, not an empty result (SPEC §5.2).
		if len(items) == 0 {
			return mcp.NewToolResultError(envelope.ErrorJSON("item_not_found", "no item matches "+strings.TrimSpace(query))), nil
		}

		return mcp.NewToolResultText(envelope.New(items, len(items)).JSON()), nil
	}
}

func clampLimit(n int) int {
	if n < 1 {
		return 1
	}
	if n > 100 {
		return 100
	}
	return n
}

// escapeLike neutralizes LIKE metacharacters in user input so a query like
// "100%" matches literally instead of as a wildcard.
func escapeLike(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}
