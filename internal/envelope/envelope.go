// Package envelope implements the cross-cutting return contract (SPEC §2,
// DESIGN §3): every tool returns the same outer shape, and bad input is a
// typed error distinct from "no data in window".
package envelope

import (
	"encoding/json"
	"time"
)

// Window is the actual ts range a query scanned (windowed tools) or the
// min/max ts of the returned rows (latest-row tools). Nil when rows is empty.
type Window struct {
	From time.Time `json:"from"`
	To   time.Time `json:"to"`
}

// Resolved echoes what a name_or_id fuzzy-resolved to (single-item tools).
type Resolved struct {
	ItemID int    `json:"item_id"`
	Name   string `json:"name"`
}

type Envelope struct {
	AsOf       time.Time      `json:"as_of"`
	DataWindow *Window        `json:"data_window"`
	RowCount   int            `json:"row_count"`
	Rows       any            `json:"rows"`
	Note       string         `json:"note,omitempty"`
	Resolved   *Resolved      `json:"resolved,omitempty"`
	Meta       map[string]any `json:"meta,omitempty"`
}

// New wraps rows in the standard envelope. rows must be a slice; rowCount is
// passed explicitly so callers can't forget to set it.
func New(rows any, rowCount int) *Envelope {
	return &Envelope{
		AsOf:     time.Now().UTC(),
		RowCount: rowCount,
		Rows:     rows,
	}
}

func (e *Envelope) JSON() string {
	b, err := json.Marshal(e)
	if err != nil {
		// The envelope is built from our own structs; this cannot fail on real data.
		return `{"error":{"code":"internal","reason":"envelope marshal failed"}}`
	}
	return string(b)
}

// ToolError is the typed-error contract (DESIGN §5): bad input only, never
// "nothing traded". Codes: item_not_found, bad_param.
type ToolError struct {
	Code   string `json:"code"`
	Reason string `json:"reason"`
}

func ErrorJSON(code, reason string) string {
	b, _ := json.Marshal(map[string]ToolError{"error": {Code: code, Reason: reason}})
	return string(b)
}
