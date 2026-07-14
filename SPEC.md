# ge-mcp tool surface spec

What `ge-mcp` exposes to the `ge-agent` directive loop, and the contract each tool
returns. Derived from what [`ge-agent/DIRECTIVE.md`](../ge-agent/DIRECTIVE.md) forces the
agent to *do* each run, backed by the validated SQL in [`QUERIES.md`](./QUERIES.md), against
the schema in [`../ge-data/init/01_schema.sql`](../ge-data/init/01_schema.sql).

`ge-mcp` is a read-only MCP server (Go, `mark3labs/mcp-go`). It exposes **domain tools, not
raw SQL**, and it **never writes**.

---

## 1. The connection (just one)

One **read-only Postgres DSN** to the Timescale DB on eldo over the tailnet, scoped to
`SELECT` on `public` (`items`, `prices_1m`, `prices_5m`). No write path, no SQL passthrough
to the agent. That is the entire external surface the server needs.

```
postgresql://ge-mcp:<pw>@eldo.<tailnet>:5432/ge-data?sslmode=disable
```

> DB name is hyphenated (`ge-data`) — quote it in DSNs/`\c`. The permanent `ge-mcp` role
> (vs. the throwaway `ge_mcp_spike`) + its `pg_hba` rule are declared in `~/cfg`; see
> [SPIKE.md](./SPIKE.md) setup.

---

## 2. Cross-cutting return contract (every tool)

The directive's guardrails — "cite only real tool output," `n < 10 → discard`, "never
recompute margin," and the reproduce trail — are unenforceable unless every tool returns
the same shape. Lock these globally:

- **Enveloped response:** `{ as_of, data_window: {from, to}, row_count, rows: [...] }`.
  `as_of` = server `now()`; `data_window` = the actual `ts` range scanned. The report
  header needs both to state "the window this run looked at."
- **Raw numerics only** — prices / margins / volumes as integer gp, never formatted
  strings. The Proof section must be re-checkable.
- **Every row carries `item_id` + `name`.** No bare ids, no name-only rows.
- **Every aggregate row carries its sample count** (`n` / `obs`). Non-negotiable — the
  confidence rules key off it.
- **Nulls preserved for prices** (never zero-filled). Volume *sums* may coalesce to 0.
- **Timestamps as ISO-8601 + a computed `*_age_s`** (seconds since the event) so the agent
  can assert leg freshness without doing time math.

---

## 3. The tool surface (11 tools)

Split into **discovery** (cast wide, ranked candidate sets) and **evidence** (drill one
item). Seven are the directive's; `quote` is added because the directive's falsification
step requires per-leg freshness for a single item and no other tool returns it.
`alch_screen`, `quotes`, and `seasonality` (plus three new `screen` metrics) were added
in the 2026-07-13 money-signals amendment ([QUERIES #12–#16](./QUERIES.md#money-signals-2026-07-13-amendment--all-validated-live))
after reviewing 26 days of accumulated data.

### Discovery

#### `top_flips`
The fresh, liquid flip watchlist ranked by margin / ROI% / profit-per-limit.
- **Params:** `min_volume=50, max_age='30min', members=null, sort_by='profit_per_limit', limit=25`
  - `sort_by ∈ margin | roi_pct | profit_per_limit | filled_profit`
  - `filled_profit = margin × least(buy_limit, vol5m)` — a deliberately conservative
    fill-aware ranking: `profit_per_limit` is a 4h ceiling that ignores whether volume
    can actually fill the limit. (2026-07-13 amendment.)
  - Default `min_volume=50` keeps the tool genuinely *liquid* (the liquidity-gate join is
    always applied); pass `min_volume=0` to loosen to a freshness-only baseline.
- **Returns per row:** `buy_at, sell_at, margin, roi_pct, buy_limit, profit_per_limit, high_age_s, low_age_s, vol5m`
- **Backed by:** [QUERIES #2](./QUERIES.md#2-real-flips--fresh-liquid-ranked-by-profit-per-limit--ship-this)

#### `margin_zscore`
Spreads abnormally wide vs the item's *own* recent baseline (mean reversion).
- **Params:** `baseline_window='2h', min_samples=10, max_age='20min', limit=25`
- **Returns per row:** `cur_margin, avg_margin, sd, z_score, roi_pct, samples, high_age_s, low_age_s`
- **Backed by:** [QUERIES #4](./QUERIES.md#4-margin-z-score--mean-reversion--abnormally-wide-spread--core-signal)

#### `movers`
Biggest % price moves over a window (events / news). Liquidity-gated.
- **Params:** `window='2h', min_price=50, min_volume=100, limit=25`
- **Returns per row:** `p_start, p_end, pct_chg, vol5m, n`
- **Backed by:** [QUERIES #3](./QUERIES.md#3-movers--biggest--price-change-over-a-window)

#### `screen`
One tool, **metric-tagged**, for the seven ranking lenses.
- **Params:** `metric, window='2h', min_obs=10, limit=25`
  - `metric ∈ volatility | surge | persistence | momentum | imbalance | range_position | spread_gap`
  - `range_position` additionally takes `min_price=50` + `min_volume=100` and defaults
    `window='7d'` (a `range_pos` below 0 means the latest price sits *under* the band —
    the legs use different coalesce order); `imbalance` additionally takes `min_volume=100`.
- **Returns per row:** `obs` always, plus metric-specific:
  - `volatility` → `cv`
  - `surge` → `cur_vol, baseline_vol, surge_ratio`
  - `persistence` → `pct_flippable`
  - `momentum` → `slope_gp_per_sec`
  - `imbalance` → `buys, sells, imbalance` (insta-buy vs insta-sell flow, −1…1)
  - `range_position` → `cur_price, range_low, range_high, range_pos, range_width_pct`
  - `spread_gap` → `cur_margin, realized_spread, spread_ratio` (post-tax vs pre-tax —
    labelled in `meta`; high ratio = stale-print trap)
- The response envelope names which `metric` ran so the consumer knows which columns are
  populated.
- **Backed by:** [QUERIES #6–9](./QUERIES.md#6-volatility-ranking--best-range-trade-candidates),
  [#13–#15](./QUERIES.md#13-order-flow-imbalance--screen-metric)

#### `alch_screen`
High-alch arbitrage: items whose insta-buy cost + a nature rune is under their
`highalch` value. The classic low-risk money-maker; 191 items qualified at validation.
- **Params:** `min_volume=50, max_age='30min', limit=25`
- **Returns per row:** `buy_at, highalch, nat_cost, alch_margin, buy_limit,
  profit_per_limit, buy_age_s, vol5m`
- Throughput caveat carried in the tool description: capped by ~1,200 casts/hr *and*
  `buy_limit`/4h; `profit_per_limit` is the 4h ceiling, not gp/hr.
- **Backed by:** [QUERIES #12](./QUERIES.md#12-high-alch-arbitrage--ship-this)

#### `seasonality`
Hour-of-day / day-of-week structure in margins and volume (archetype F — unlocked
2026-07-13 on data age; raw scans, no CAGG, slow is fine per §6).
- **Params:** `dimension ∈ hour | dow`, `name_or_id?` (optional item filter; global
  when omitted)
- **Returns per row:** `bucket` (hour 0–23 UTC or dow 0–6), `avg_margin, obs`
- dow buckets have ~4 samples of each weekday per month of data — `obs` keeps the
  confidence rules honest.
- **Backed by:** [QUERIES #10–11](./QUERIES.md#10-hour-of-day-seasonality--unlocked)

### Evidence

#### `lookup_item`
Resolve a name → item metadata. The **only** source of `buy_limit` / `members` / alch.
- **Params:** `query, limit=10`
- **Returns per row:** `item_id, name, members, buy_limit, value, lowalch, highalch, icon`
- **Backed by:** `items` via `items_name_lower_idx`

#### `quote`  *(new — the falsification primitive)*
Current both-leg snapshot + freshness for one item. Required by the directive's
falsification check ("are both legs fresh, or is the margin a stale-leg artifact?") and the
worked example's *"both legs fresh within 6 min ✓"*.
- **Params:** `name_or_id`
- **Returns:** `high, high_time, high_age_s, low, low_time, low_age_s, margin, ts, vol5m`
- **Backed by:** [QUERIES building blocks #1 + #2](./QUERIES.md#building-blocks)

#### `quotes`  *(batch — the watchlist primitive)*
`quote` for up to 25 items in one call — re-checking N candidates otherwise costs N
round-trips. Same row shape as `quote`; unresolvable names are reported in a per-item
`errors` list in the envelope rather than failing the whole call.
- **Params:** `names_or_ids` (array, 1–25)
- **Returns per row:** as `quote`
- **Backed by:** [QUERIES #16](./QUERIES.md#16-batch-quotes) (#0 with `ANY($1)`)

#### `item_history`
OHLC / series for one item — the evidence backbone.
- **Params:** `name_or_id, grain='15min', lookback='6h', source='1m'`
  - `source ∈ 1m | 5m`. **`5m` prices are block averages** (`avg_high_price` /
    `avg_low_price`), not last-trade quotes — the only source with volume, but a different
    price basis. Labelled in the response.
- **Returns per row:**
  - `source='1m'`: `bucket, open, high, low, close, obs`
  - `source='5m'`: `bucket, avg_high, avg_low, high_volume, low_volume, obs`
- **Backed by:** [QUERIES #5](./QUERIES.md#5-ohlc-candles--the-charting--feature-primitive) (+ gapfill block)

#### `liquidity`
Summed recent 5m volume for sizing. Accepts `name_or_id` (not bare `item_id`) to match
`item_history` and avoid a `lookup_item` round-trip every sizing step.
- **Params:** `name_or_id, window='15min'`
- **Returns:** `vol5m_total, high_volume, low_volume, window, n_buckets`
- **Backed by:** [QUERIES building block #2](./QUERIES.md#building-blocks)

---

## 4. Out of scope for v1

- ~~**Seasonality**~~ — **moved in-scope 2026-07-13**: the data-age bar (~3–4 weeks) has
  been reached, and §6 already resolved that raw scans are acceptable (no CAGG, no schema
  change). Now specced as the `seasonality` tool in §3.
- **Combo / related items** (set-vs-components, raw-vs-processed). No relationship data
  exists in `items`, so the directive's combo paragraph is currently unbacked. Either cut
  it from v1 or add a `quotes([name_or_id])` batch tool *and* accept there is no
  set→component mapping yet.

---

## 5. Decisions

### Locked

1. **`lookup_item` is fuzzy.** Case-insensitive, ranked: exact `lower(name)` match → prefix
   (`lower(name) LIKE q || '%'`) → substring (`LIKE '%' || q || '%'`). Return up to `limit`
   candidates, each with full metadata. The existing btree `items_name_lower_idx` serves
   exact + prefix; substring is a seq scan but `items` is a small static table, so fine.
   *Typo-tolerance (e.g. "twsited bow") is a later upgrade needing `pg_trgm` + a GIN index —
   not v1.*
   - **Single-item tools** (`quote`, `liquidity`, `item_history`) accept `name_or_id`: a
     numeric id is used directly; a name resolves to the **best-ranked** match and the
     response echoes `resolved: {item_id, name}` so the agent sees what it got. To
     disambiguate deliberately, call `lookup_item` first.

2. **Error contract — typed errors for bad input, empty results for "nothing traded".**
   - `item_not_found` (no fuzzy match at all) → typed error `{error: {code, reason}}`.
   - **No data in the window is NOT an error** → return `rows: []` with a `note` in the
     envelope. "Nothing traded" is a legitimate liquidity signal the directive explicitly
     values (a null/empty side = real information), not a failure. Conflating the two would
     hide signal. All other tools follow the same split.

3. **DSN-from-config.** Permanent `ge-mcp` read-only role + its `pg_hba` rule declared in
   `~/cfg`; the server reads the DSN from config, never hardcoded.

4. **Long-range / "holistic" history — resolved: raw reads, any range, speed not a
   concern.** See [§6](#6-long-range-history-decision-4).

---

## 6. Long-range history (decision #4)

**This is a research tool, not a customer-facing product — query speed is explicitly not a
concern.** That collapses the whole decision:

- **Compression stays exactly as-is** (`7 days` on both hypertables). It never blocks
  analysis — compressed chunks are still fully queryable by the same SQL; Timescale
  decompresses transparently on read. Nothing is archived or made unavailable at any age.
- **Raw reads serve any range, including months back.** Item-scoped reads (`quote`,
  `item_history`, `liquidity`) stay fast regardless of age thanks to
  `segmentby = item_id`. Broad multi-month scans (archetype F seasonality) will be *slow* —
  seconds to minutes — and **that is fine** for a periodic research agent.
- **No continuous aggregates.** Not built, not deferred-with-a-trigger — simply not needed.
  If a future scan ever becomes painful enough to matter, a CAGG can be added then with no
  API change (`item_history` would just gain a coarser `source`). Until someone is actually
  bothered by the wait, raw is the answer.

So archetype F unlocks purely on **data age** (enough history accumulated), not on any infra
work. The tools query raw chunks for whatever window the analysis wants; the directive's
"lookbacks recent" guidance for A–E is a *relevance* choice, not a performance constraint.
