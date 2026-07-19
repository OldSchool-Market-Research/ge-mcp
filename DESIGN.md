# ge-mcp design

How `ge-mcp` is built. This is the bridge between the *what* — the tool surface in
[SPEC.md](./SPEC.md), backed by the validated SQL in [QUERIES.md](./QUERIES.md) — and the
*how to ship it*. It folds in the open issues found reviewing those docs and resolves them
into buildable decisions, so the implementation plan that follows has no unknowns left.

Read order: [SPIKE.md](./SPIKE.md) (why) → [QUERIES.md](./QUERIES.md) (the SQL) →
[SPEC.md](./SPEC.md) (the contract) → this (the build).

---

## 1. What we are building

A **read-only MCP server** (Go, `mark3labs/mcp-go`) that runs in k8s next to the ingester
and exposes **8 domain tools** to the `ge-agent` directive loop over a single `SELECT`-only
Postgres connection to the Timescale DB on eldo. No writes, no raw-SQL passthrough. The agent
asks for *flips / movers / history*; it never sees SQL.

The whole point: the directive ([`ge-agent/DIRECTIVE.md`](../ge-agent/DIRECTIVE.md)) forces
the agent to produce **falsifiable, post-tax, buy-limit-aware** strategies and to cite only
real tool output. That is only enforceable if the tools return a uniform, re-checkable shape
and encode the schema's invariants (post-tax margin, nulls-are-signal, freshness gates) so the
agent *cannot* violate them. The server is where those invariants live.

### Non-goals (v1)
- No writes, ever. No SQL passthrough. No mutation tools.
- No seasonality (archetype F) — unlocks on data age, needs a CAGG later (SPEC §4/§6).
- No combo/related-item tools — no relationship data exists in `items` (SPEC §4).
- No typo-tolerant search (`pg_trgm`) — fuzzy ranking only (SPEC §5.1).
- Not customer-facing: query latency is explicitly not a concern (SPEC §6).

---

## 2. Architecture

```
 ge-agent (directive loop, MiniMax M3 via Hermes tool-calling)
        │  MCP (stdio or streamable-HTTP)
        ▼
 ┌─────────────────────────────┐
 │ ge-mcp (Go, mcp-go)         │
 │  ┌───────────────────────┐  │
 │  │ tool handlers (8)     │  │  param structs → validate → bind
 │  └─────────┬─────────────┘  │
 │  ┌─────────▼─────────────┐  │
 │  │ query layer           │  │  one *.sql per tool, named params only
 │  │ (envelope builder)    │  │  → {as_of, data_window, row_count, rows}
 │  └─────────┬─────────────┘  │
 │  ┌─────────▼─────────────┐  │
 │  │ pgxpool (read-only)   │  │  role ge-mcp, SELECT-only, statement_timeout
 │  └─────────┬─────────────┘  │
 └────────────┼────────────────┘
              │ tailnet (WireGuard), DSN from config
              ▼
        Timescale on eldo : "ge-data"  (items, prices_1m, prices_5m)
```

Layers, top to bottom:

1. **Tool handlers** — one per tool. Decode the MCP arguments into a typed Go struct, run
   **validation** (see §4), call the query layer, return the envelope. No SQL here.
2. **Query layer** — owns the parameterized SQL (lifted verbatim from QUERIES.md), executes
   via `pgxpool`, and wraps rows in the standard envelope (§3). Every query uses **bound
   parameters only** — no string interpolation of intervals, limits, or names.
3. **Connection** — a single `pgxpool.Pool` on the read-only `ge-mcp` role with a
   `statement_timeout` and a small max-conn count (the agent is single-threaded-ish).

**Transport: stdio (locked for v1).** The agent spawns/owns the process — no Service, no
port, no endpoint secret. HTTP only earns its complexity if multiple clients need to share one
`ge-mcp`, which isn't the case yet, and the switch is transport-only (no tool changes). Keep
the door open to `mcp-go`'s streamable-HTTP server for that later.

**Language/lib:** Go + `mark3labs/mcp-go` (already chosen in SPEC/SPIKE), `jackc/pgx/v5` +
`pgxpool` for the DB. pgx because it does real prepared statements / typed binding (kills the
interval-injection risk) and handles `timestamptz`/`bigint`/nulls cleanly.

---

## 3. The response envelope (locked)

Every tool returns the same outer shape (SPEC §2). The MCP tool result is this JSON as text
content.

```jsonc
{
  "as_of":       "2026-06-19T14:03:22Z",         // server now()
  "data_window": { "from": "...", "to": "..." }, // see semantics below
  "row_count":   12,
  "rows":        [ /* tool-specific, raw numerics, item_id+name on every row */ ],
  "note":        "no rows in window",            // optional, present when rows:[]
  "resolved":    { "item_id": 4151, "name": "Abyssal whip" }, // single-item tools only
  "meta":        { "metric": "volatility", "source": "1m" }   // tool-specific tags
}
```

Rules carried from SPEC §2, all enforced in the envelope builder / query SQL:
- **Raw integer gp** — never formatted strings.
- **`item_id` + `name` on every row** — including the `screen` rows (this is a fix; see §6).
- **Sample count on every aggregate row** (`n` / `obs`).
- **Nulls preserved for prices**; volume *sums* may coalesce to 0.
- **Timestamps ISO-8601 + a computed `*_age_s`** so the agent never does time math.

**`data_window` semantics (resolved — was underspecified in SPEC §2).** Two cases:
- **Windowed tools** (`movers`, `margin_zscore` baseline, `screen`, `item_history`,
  `liquidity`): `from`/`to` = the actual scanned `ts` bounds (`now() - window` … `now()`).
- **Latest-row tools** (`top_flips`, `quote`, and the `latest` CTEs): there is no scanned
  window, so `from` = `min(ts)` and `to` = `max(ts)` **of the returned rows** (the freshest
  data the answer rests on). The builder computes this from the result set; if `rows: []`,
  `data_window` is `null` and `note` explains.

---

## 4. Input validation & safety (the part the specs were thin on)

This is the security boundary. "No SQL passthrough" is necessary but not sufficient — the
**interval and limit params still reach SQL**. Rules:

1. **Durations** (`max_age`, `window`, `baseline_window`, `lookback`) — parse to a
   `time.Duration` with a strict whitelist grammar (`^\d+(s|m|min|h|d)$`), reject anything
   else with a typed `bad_param` error. Bind as a `timestamptz` cutoff computed in Go
   (`now().Add(-d)`), **not** by concatenating into `interval '...'`. **No max-lookback cap**
   (decision: trust the agent — SPEC §6, speed is explicitly not a concern). The grammar
   whitelist still rejects malformed input; we just don't bound how far back a *well-formed*
   lookback may reach. A multi-month research scan is allowed to be slow.
2. **`limit`** — bind as `int`, clamp to `[1, 100]`. Default 25.
3. **Enums** (`sort_by`, `metric`, `source`) — validate against the closed set; map to a fixed
   SQL fragment chosen by a Go `switch`, never interpolated from the input string.
4. **`name_or_id` / `query`** — always a **bound parameter**. Numeric → used as `item_id`
   directly; otherwise fuzzy-resolve (SPEC §5.1) and echo `resolved`.
5. **DB-side defense in depth:** the `ge-mcp` role has only `SELECT` and
   `default_transaction_read_only = on` — even a logic bug cannot write. Since there is **no
   lookback cap**, `statement_timeout` is **not** used as a scan-length guard: point tools
   keep a few-second timeout, but windowed/research tools run with a generous (or disabled)
   timeout so a legitimately slow multi-month scan isn't killed mid-flight.

---

## 5. Error contract (locked, SPEC §5.2)

Two outcomes, never conflated:
- **Bad input** → typed error `{ error: { code, reason } }`. Codes: `item_not_found` (no
  fuzzy match at all), `bad_param` (malformed duration/enum/limit). Returned as an MCP tool
  error so the agent sees a structured failure.
- **No data in window** → **not an error.** `rows: []` + `note`. "Nothing traded" is a real
  liquidity signal the directive values (constraint #2). All tools follow this split.

---

## 6. Issues found reviewing SPEC/QUERIES — and how this design resolves them

| # | Issue (where) | Resolution in this design |
|---|---|---|
| R1 | Interval/limit params flow into SQL; no validation specced | §4 — parse durations to bound `timestamptz` cutoffs, clamp `limit`, enum→switch. No string interpolation anywhere. |
| R2 | `screen` queries (QUERIES #6–#9) `GROUP BY i.name`, return **no `item_id`** — violates SPEC §2 "every row carries item_id" | Rewrite to `GROUP BY p.item_id, i.name`, select both. Tracked as a QUERIES.md fix (§9). |
| R3 | `item_history(source='5m')` is specced but **no validated 5m query exists** (QUERIES #5 is 1m only) | Add + validate the 5m OHLC/volume query before coding the tool. Decide gapfill explicitly: **candles are not gap-filled** (empty bucket = omitted row); `locf` gapfill is a separate concern only the feature/charting path would want. |
| R4 | `coalesce(high, low)` proxy (movers/volatility/momentum) conflates bid & ask → spread bounce reads as volatility | Keep the proxy (fine for research) but **document it in `meta`/the row contract** so the consumer knows `cv`/`slope` include spread bounce, not a clean mid. |
| R5 | `data_window` undefined for latest-row tools (SPEC §2) | §3 — min/max `ts` of returned rows; `null` when empty. |
| R6 | Permanent `ge-mcp` role DDL referenced ("in `~/cfg`") but only the throwaway spike role is written down | Check in the real role's `GRANT SELECT` + `pg_hba` + `default_transaction_read_only` as reviewable DDL (alongside `ge-data/init/` or a `ge-mcp/db/` snippet), not only `~/cfg`. |
| R7 | `top_flips` billed "liquid" but `min_volume=0` default = no liquidity gate | **Resolved: default `min_volume=50`** so the headline tool is genuinely liquid by default; pass `min_volume=0` to loosen. The liquidity-gate join (QUERIES building-block #2) is therefore **always** applied. |
| R8 | DIRECTIVE drift: lists `liquidity(item_id,…)` & `screen(metric,window,limit)`; SPEC has `name_or_id` & `min_obs` | Reconcile to SPEC (the newer doc) and patch DIRECTIVE's toolbelt table so the agent's mental model matches the wire contract. |
| R9 | SPIKE.md "Decisions" section blank, says "4–6 tools" (SPEC landed on 8); cleanup checklist unchecked | Add "superseded by SPEC §5" pointer; confirm `ge_mcp_spike` role is dropped on eldo (this dir runs *on* prod). |

R2, R3, R8, R9 are doc/SQL edits to do **before** scaffolding. R1, R4, R5, R6, R7 are
build-time decisions captured here.

---

## 7. Configuration & deployment

- **DSN from config**, never hardcoded (SPEC §5.3). Source order: env var
  `GE_MCP_DSN` → config file. Shape:
  `postgresql://ge-mcp:<pw>@eldo.<tailnet>:5432/ge-data?sslmode=disable`. `sslmode=disable`
  is acceptable because the tailnet (WireGuard) already encrypts; note it so it doesn't read
  as an oversight.
- **DB name is hyphenated** (`ge-data`) → quote in any `\c`, fine inside a DSN path.
- **k8s:** runs in the same namespace as the ingester. If stdio, the agent pod execs the
  binary; if HTTP, a small `Deployment` + `Service`. Secret holds the DSN. Read-only
  `securityContext`, no egress beyond the tailnet DB.
- **Pool:** small (`max_conns` ~4), `statement_timeout` set per §4.

---

## 8. Testing strategy

- **Query/golden tests** against a throwaway Timescale container seeded with a few items and
  synthetic `prices_1m`/`prices_5m` rows covering: fresh-both-legs, stale-one-leg, null
  price, zero-volume side, abnormally wide margin (z-score), a mover. Assert the envelope
  shape and the invariants (item_id+name present, nulls preserved, margin read not recomputed,
  `*_age_s` correct).
- **Validation tests** — every bad-param path returns `bad_param`, not a 500 and not SQL.
  Explicitly test injection-ish durations (`'2h; DROP'`, `'9000h'`) are rejected/clamped.
- **Contract test** — for each tool, the columns it returns match SPEC §3 exactly (catches
  the R2 class of drift).
- **No live-prod test in CI** — eldo is prod; use the container.

---

## 9. Implementation plan (proposed — for finalizing)

**Phase 0 — doc/SQL fixes (no Go). ✅ done 2026-06-19.** Landed: R2 (screen queries now carry
`item_id`), R3 (5m history query added — columns verified vs schema, `prices_5m` confirmed
populated; a live result-shape run deferred to Phase 2 when a `ge-mcp` cred exists), R8
(DIRECTIVE toolbelt reconciled + `quote` added), R9 (`ge_mcp_spike` confirmed never created —
no prod exposure), R6 (`ge-mcp/db/grants.sql` checked in), R7 (`min_volume=50` default).
**Prereq for Phase 1:** apply `db/grants.sql` on eldo (create the `ge-mcp` role) and set its
password out-of-band — there is currently no read-only role to connect as.

**Phase 1 — skeleton.** `go mod init`, `mcp-go` stdio server, `pgxpool` wired to the
read-only DSN from config, `statement_timeout` + `read_only` session settings, health check.
One trivial tool (`lookup_item`) end-to-end to prove the path.

**Phase 2 — evidence tools.** `quote`, `item_history` (1m + 5m), `liquidity`. These are the
falsification backbone and exercise `name_or_id` resolution + the envelope builder.

**Phase 3 — discovery tools.** `top_flips`, `margin_zscore`, `movers`, `screen`. Reuse the
validated SQL; apply §4 validation; tag `meta.metric`/`meta.source`.

**Phase 4 — wire to ge-agent.** Register `ge-mcp` with the directive loop, run the SPIKE.md
directives end-to-end, confirm the agent produces strategies citing real envelopes. This is
also where R7 (`min_volume` default) and the screen-as-one-tool hedge get settled by observing
real usage.

**Phase 5 — deploy.** k8s manifests, DSN secret, run alongside the ingester.

---

## 10. Decisions (resolved 2026-06-19)

1. **Transport — stdio for v1.** Agent owns the process; HTTP deferred (§2).
2. **`top_flips` default `min_volume=50`** — liquid by default; `min_volume=0` to loosen (R7).
3. **No max-lookback cap** — trust the agent, slow research scans allowed (§4).
4. **`ge-mcp` role DDL checked in** at `ge-mcp/db/grants.sql` (R6, §7).
5. **`screen` stays one metric-tagged tool** for v1; revisit only if Phase 4 shows the metrics
   are used distinctly enough to warrant splitting.

Open only after Phase 4 (real-usage signal): whether to split `screen`. Everything else is
final — the §9 plan is ready to execute.

---

## 11. Flips-first amendment (2026-07-18)

The sweetcorn incident (ge-agent run 123: a 423gp strategy shipped off a 103% amplitude
ranking) established that **ratio rankings without an absolute-gp column are a footgun**:
every discovery surface must expose what an opportunity is worth in gp at fillable size,
not only how large it is relative to itself.

1. **`top_flips` is the discovery primitive for the two flip lanes** (volume flips:
   `min_vol24h=100000`; high-value flips: `min_price=10000000, min_vol24h=200,
   sort_by=margin`). It gains a `vol24` CTE (24h summed volume), `min_vol24h`/`min_price`
   gates, and a `gp_day` capacity column/sort
   (`margin × least(buy_limit×6, floor(vol24h×0.15))`).
2. **`seasonal_scan` demoted to evidence.** Ranking stays `amplitude_pct` but each row
   now carries `gp_cycle` (amplitude in gp × fillable units) so a ratio is never
   presented without its absolute scale.
3. The capacity constants (6 cycles/day, 15% participation) are deliberately simple
   ceilings, documented in each tool's meta; the paper-trading evaluator owns the
   honest haircut.
