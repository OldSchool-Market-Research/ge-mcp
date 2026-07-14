package tools

import (
	"context"
	"encoding/json"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/osrs-ge/ge-mcp/internal/envelope"
)

// QUERIES #20 (validated 2026-07-14). Lists the hand-curated item_relations
// rows (ge-data/init/03_item_relations_seed.sql) — the archetype-C universe.
// Legs are enriched with names + buy limits from `items` at query time so
// they can never drift from the seed's raw ids.
const listRelationsSQL = `
SELECT r.relation_id, r.kind, r.name, r.reversible, coalesce(r.notes, ''),
  (SELECT jsonb_agg(jsonb_build_object(
      'item_id', (l->>'item_id')::int, 'qty', (l->>'qty')::int,
      'name', i.name, 'buy_limit', i.buy_limit))
   FROM jsonb_array_elements(r.inputs) l
   JOIN items i ON i.item_id = (l->>'item_id')::int) AS inputs,
  (SELECT jsonb_agg(jsonb_build_object(
      'item_id', (l->>'item_id')::int, 'qty', (l->>'qty')::int,
      'name', i.name, 'buy_limit', i.buy_limit))
   FROM jsonb_array_elements(r.outputs) l
   JOIN items i ON i.item_id = (l->>'item_id')::int) AS outputs
FROM item_relations r
WHERE ($1::text IS NULL OR r.kind = $1)
  AND ($2::int IS NULL OR r.inputs @> jsonb_build_array(jsonb_build_object('item_id', $2::int))
                       OR r.outputs @> jsonb_build_array(jsonb_build_object('item_id', $2::int))
                       OR EXISTS (SELECT 1 FROM jsonb_array_elements(r.inputs || r.outputs) l
                                  WHERE (l->>'item_id')::int = $2::int))
ORDER BY r.kind, r.name
LIMIT $3`

type relationLeg struct {
	ItemID   int    `json:"item_id"`
	Qty      int64  `json:"qty"`
	Name     string `json:"name"`
	BuyLimit *int64 `json:"buy_limit"`
}

type relationRow struct {
	RelationID int           `json:"relation_id"`
	Kind       string        `json:"kind"`
	Name       string        `json:"name"`
	Reversible bool          `json:"reversible"`
	Notes      string        `json:"notes"`
	Inputs     []relationLeg `json:"inputs"`
	Outputs    []relationLeg `json:"outputs"`
}

func NewListRelationsTool() mcp.Tool {
	return mcp.NewTool("list_relations",
		mcp.WithDescription("The archetype-C universe: hand-curated mechanical conversions between tradeable items — potion decants (4<->3 dose), GE-clerk armour sets, combine recipes. Canonical direction is inputs bought -> outputs sold; reversible=true also works backwards (sets, godsword dismantling, decants either way). notes carry skill/quest gates and NPC fees — a conversion the player can't perform is not their edge; always surface them. Price a specific relation end-to-end with combo_quote."),
		mcp.WithString("kind", mcp.Enum("decant", "set", "combine"), mcp.Description("Filter by relation kind; omit for all")),
		mcp.WithString("name_or_id", mcp.Description("Optional item filter: relations where this item appears on either side")),
		mcp.WithNumber("limit", mcp.Description("Max rows (default 50)")),
	)
}

func ListRelationsHandler(pool *pgxpool.Pool) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var kind *string
		if k := req.GetString("kind", ""); k != "" {
			switch k {
			case "decant", "set", "combine":
				kind = &k
			default:
				return mcp.NewToolResultError(envelope.ErrorJSON("bad_param", "kind must be decant, set or combine")), nil
			}
		}
		limit := req.GetInt("limit", 50)
		if limit < 1 {
			limit = 1
		}
		if limit > 200 {
			limit = 200
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

		rows, err := pool.Query(ctx, listRelationsSQL, kind, itemID, limit)
		if err != nil {
			return nil, err
		}
		defer rows.Close()

		out := []relationRow{}
		for rows.Next() {
			var r relationRow
			var inputs, outputs []byte
			if err := rows.Scan(&r.RelationID, &r.Kind, &r.Name, &r.Reversible, &r.Notes, &inputs, &outputs); err != nil {
				return nil, err
			}
			if err := json.Unmarshal(inputs, &r.Inputs); err != nil {
				return nil, err
			}
			if err := json.Unmarshal(outputs, &r.Outputs); err != nil {
				return nil, err
			}
			out = append(out, r)
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}

		env := envelope.New(out, len(out))
		env.Resolved = resolved
		if len(out) == 0 {
			env.Note = "no relations match this scope"
		}
		return mcp.NewToolResultText(env.JSON()), nil
	}
}
