package tools

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mark3labs/mcp-go/mcp"

	"github.com/osrs-ge/ge-mcp/internal/envelope"
)

// resolveItem implements the name_or_id contract (SPEC §5.1): a numeric id is
// used directly (but still verified so the response can echo the name); a name
// resolves to the best-ranked fuzzy match. The second return is a non-nil
// typed-error tool result when resolution fails.
func resolveItem(ctx context.Context, pool *pgxpool.Pool, nameOrID string) (*envelope.Resolved, *mcp.CallToolResult, error) {
	nameOrID = strings.TrimSpace(nameOrID)
	if nameOrID == "" {
		return nil, mcp.NewToolResultError(envelope.ErrorJSON("bad_param", "name_or_id must be a non-empty string or numeric id")), nil
	}

	var r envelope.Resolved
	var err error
	if id, convErr := strconv.Atoi(nameOrID); convErr == nil {
		err = pool.QueryRow(ctx, `SELECT item_id, name FROM items WHERE item_id = $1`, id).Scan(&r.ItemID, &r.Name)
	} else {
		err = pool.QueryRow(ctx, `
			SELECT item_id, name FROM items
			WHERE lower(name) LIKE '%' || lower($1) || '%' ESCAPE '\'
			ORDER BY (lower(name) = lower($2)) DESC,
			         (lower(name) LIKE lower($1) || '%' ESCAPE '\') DESC,
			         name
			LIMIT 1`, escapeLike(nameOrID), nameOrID).Scan(&r.ItemID, &r.Name)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, mcp.NewToolResultError(envelope.ErrorJSON("item_not_found", "no item matches "+nameOrID)), nil
	}
	if err != nil {
		return nil, nil, err
	}
	return &r, nil, nil
}

// durationRe is the strict whitelist grammar from DESIGN §4. Durations never
// reach SQL as text: they become a timestamptz cutoff computed in Go.
var durationRe = regexp.MustCompile(`^(\d+)(s|m|min|h|d)$`)

func parseDuration(s string) (time.Duration, error) {
	m := durationRe.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return 0, fmt.Errorf("duration %q must match <number><s|m|min|h|d>", s)
	}
	n, err := strconv.Atoi(m[1])
	if err != nil || n == 0 {
		return 0, fmt.Errorf("duration %q must be a positive integer amount", s)
	}
	unit := map[string]time.Duration{"s": time.Second, "m": time.Minute, "min": time.Minute, "h": time.Hour, "d": 24 * time.Hour}[m[2]]
	return time.Duration(n) * unit, nil
}

// sprintfSQL splices a fragment into a query. It exists to make the one
// allowed non-bound substitution greppable: the fragment must come from a
// closed Go-side set (sort column / metric switch), NEVER from user input.
func sprintfSQL(query, fragment string) string {
	return fmt.Sprintf(query, fragment)
}

// durationParam parses an optional duration argument, returning a typed
// bad_param result on grammar violations.
func durationParam(req mcp.CallToolRequest, key, def string) (time.Duration, *mcp.CallToolResult) {
	d, err := parseDuration(req.GetString(key, def))
	if err != nil {
		return 0, mcp.NewToolResultError(envelope.ErrorJSON("bad_param", key+": "+err.Error()))
	}
	return d, nil
}
