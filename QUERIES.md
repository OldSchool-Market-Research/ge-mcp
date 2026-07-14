# ge-mcp query library

The **validated SQL** behind `ge-mcp`'s tools. Every query here has been run against the
live Timescale DB on eldo (`prices_1m` / `prices_5m` / `items`) and returns sensible
results. This is the concrete deliverable the [SPIKE.md](./SPIKE.md) calls for: capture
the query shapes first, then encode the recurring ones as read-only MCP tools.

Each entry says **what it answers**, the **SQL**, which **tables/joins/lookback** it needs,
the **gotchas**, and the **candidate tool** it maps to.

> Read [SPIKE.md](./SPIKE.md) first for the schema contract and the read-only-role setup.
> These are SELECT-only; `ge-mcp` never writes.

---

## Conventions every query respects

- **Nulls are a liquidity signal — never zero-fill.** A null price/volume means nothing
  traded that side. Filter with `IS NOT NULL`, don't `COALESCE(x,0)` *for prices*.
  (Coalescing volume to 0 *is* fine when summing a liquidity gate — a missing side
  genuinely contributes 0 volume.)
- **`prices_1m.margin` is already post-tax** (`high − LEAST(high/50, 5M) − low`, the 2%
  GE tax on the sell leg since 2025-05-29). Read it; never recompute. `prices_5m` has
  **no** margin column.
- **"Current price" = latest row per item, not a fixed timestamp.** `prices_1m` is
  dedup-on-change, so use the `DISTINCT ON (item_id) … ORDER BY item_id, ts DESC` pattern
  below. Rows are **irregularly spaced** as a result — any evenly-gridded read needs
  `time_bucket_gapfill()` + `locf()`.
- **Volume lives only in `prices_5m`.** `prices_1m` has no volume, so every liquidity
  gate joins the latest 5m row. (And `/latest` is Cloudflare-cached `max-age=60`, so 1m
  is really ~60s-grained no matter how often we poll.)
- **Keep lookbacks recent.** Both hypertables compress after 7 days; scanning older
  ranges decompresses. Anything wanting multi-week history should ride a continuous
  aggregate, not raw rows (see [Needs more history](#needs-more-history)).
- **Volume is units, not trades.** A liquidity proxy, not a transaction count.
- `timescaledb_toolkit` is **not** installed — only core `timescaledb`. So `first()`,
  `last()`, `time_bucket`, `time_bucket_gapfill`, `locf` are available; `candlestick_agg`
  / `stats_agg` are not. The queries below use core only.

> The database name is `ge-data` (hyphenated) — quote it as `"ge-data"` in DSNs/`\c`.

---

## Building blocks

These aren't tools themselves; they're the CTEs the real queries compose.

### Latest row per item (the "current price" pattern)
```sql
SELECT DISTINCT ON (item_id) item_id, ts, high, high_time, low, low_time, margin
FROM prices_1m
ORDER BY item_id, ts DESC;     -- uses the (item_id, ts DESC) index
```

### Liquidity gate (latest 5m volume per item)
```sql
SELECT DISTINCT ON (item_id) item_id,
       coalesce(high_volume,0) + coalesce(low_volume,0) AS vol5m
FROM prices_5m
WHERE ts > now() - interval '15 minutes'
ORDER BY item_id, ts DESC;
```

### Gap-filled regular series (for charts / evenly-spaced features)
```sql
SELECT time_bucket_gapfill('1 min', ts) AS bucket,
       locf(last(high, ts)) AS high       -- carry last value into empty buckets
FROM prices_1m
WHERE item_id = $1 AND ts > now() - interval '2 hours' AND ts <= now()
GROUP BY bucket
ORDER BY bucket;
```

---

## Point-in-time quote (one item)

### 0. Current quote for one item ✅ ship this — the falsification primitive
**Answers:** the live both-leg snapshot for a single item, with **per-leg freshness** and
the latest 5m volume. This is what the directive's falsification step needs ("are *both*
legs fresh, or is the margin a stale-leg artifact?") and what the worked example asserts
(*"both legs fresh within 6 min ✓"*). No screening tool returns per-leg timestamps for an
arbitrary item, so this is its own tool.
```sql
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
SELECT i.name, q.ts, q.high, q.high_time, q.low, q.low_time, q.margin,
       extract(epoch from now() - q.high_time)::int AS high_age_s,
       extract(epoch from now() - q.low_time)::int  AS low_age_s,
       coalesce(liq.vol5m, 0) AS vol5m
FROM q JOIN items i USING (item_id) LEFT JOIN liq USING (item_id);
```
- **Tables:** `prices_1m` + `prices_5m` + `items`. **Lookback:** latest (+15m for volume).
- **Gotcha:** `LEFT JOIN` the liquidity CTE — an item can have a fresh quote but no 5m
  volume row in the last 15m. Don't drop the quote when volume is absent; return `vol5m=0`.
- **→ tool:** `quote(name_or_id)`

---

## Flip selection (point-in-time "what should I buy/sell now")

### 1. Best flips right now (literal) — ⚠️ surfaces stale junk
**Answers:** each item's latest margin, ranked. **Don't ship this alone** — it's
dominated by illiquid items whose `high` and `low` are hours/days apart and can't
actually be flipped. Kept here as the baseline that motivates #2.
```sql
WITH latest AS (
  SELECT DISTINCT ON (item_id) item_id, high, low, margin
  FROM prices_1m ORDER BY item_id, ts DESC
)
SELECT i.name, l.low AS buy_at, l.high AS sell_at, l.margin
FROM latest l JOIN items i USING (item_id)
WHERE l.margin IS NOT NULL
ORDER BY l.margin DESC
LIMIT 25;
```
- **Tables:** `prices_1m` + `items`. **Lookback:** latest. **Gotcha:** no freshness or
  liquidity gate → unflippable.

### 2. Real flips — fresh, liquid, ranked by profit-per-limit ✅ ship this
**Answers:** the actual flip watchlist — latest margin, *both legs fresh*, with ROI% and
4-hour profit ceiling (`margin × buy_limit`).
```sql
WITH latest AS (
  SELECT DISTINCT ON (item_id) item_id, ts, high, high_time, low, low_time, margin
  FROM prices_1m ORDER BY item_id, ts DESC
)
SELECT i.name,
       l.low  AS buy_at,
       l.high AS sell_at,
       l.margin,
       round(l.margin::numeric / NULLIF(l.low,0) * 100, 2) AS roi_pct,
       i.buy_limit AS lim,
       l.margin * i.buy_limit AS profit_per_limit
FROM latest l JOIN items i USING (item_id)
WHERE l.margin > 0
  AND l.high_time > now() - interval '30 minutes'   -- freshness gate (tighten to 5m for active flipping)
  AND l.low_time  > now() - interval '30 minutes'
ORDER BY profit_per_limit DESC NULLS LAST
LIMIT 25;
```
- **Tables:** `prices_1m` + `items`. **Lookback:** latest + 30m freshness.
- **Knobs:** freshness window; sort key (`margin` raw / `roi_pct` capital-efficiency /
  `profit_per_limit` whale mode); the liquidity gate (join building-block #2,
  `WHERE vol5m > min_volume`) for "can I actually fill this size".
- **→ tool:** `top_flips(min_volume=50, max_age, members?, sort_by, limit)` — the tool
  **always** applies the building-block #2 liquidity join (`vol5m > min_volume`); default
  `min_volume=50` so it's liquid out of the box, `min_volume=0` loosens to freshness-only.

---

## Temporal / market analysis

The interesting half — patterns over time, for spike research and the scoring agent.

### 3. Movers — biggest % price change over a window
**Answers:** items that spiked or crashed (event/news detection). Liquidity-gated.
```sql
WITH w AS (
  SELECT item_id,
         first(coalesce(high,low), ts) AS p_start,
         last (coalesce(high,low), ts) AS p_end
  FROM prices_1m
  WHERE ts > now() - interval '2 hours'
  GROUP BY item_id
),
liq AS (
  SELECT DISTINCT ON (item_id) item_id,
         coalesce(high_volume,0)+coalesce(low_volume,0) AS vol
  FROM prices_5m WHERE ts > now() - interval '15 min'
  ORDER BY item_id, ts DESC
)
SELECT i.name, w.p_start, w.p_end,
       round((w.p_end-w.p_start)::numeric/nullif(w.p_start,0)*100, 2) AS pct_chg,
       liq.vol AS vol5m
FROM w JOIN items i USING (item_id) JOIN liq USING (item_id)
WHERE w.p_start > 50 AND liq.vol > 100        -- price floor kills penny-item % noise
ORDER BY abs((w.p_end-w.p_start)::numeric/nullif(w.p_start,0)) DESC
LIMIT 25;
```
- **Tables:** `prices_1m` + `prices_5m` + `items`. **Lookback:** window (param).
- **Gotcha:** without the `p_start > 50` floor, 1→2 gp penny items dominate with huge %.
- **→ tool:** `movers(window, min_price, min_volume, limit)`

### 4. Margin z-score — mean-reversion / abnormally wide spread ✅ core signal
**Answers:** items whose *current* margin is far above their *own* recent baseline — a
transient spread that tends to revert. The mechanical flip the naive ranker misses
(e.g. items that are normally negative-margin but blew out positive right now).
```sql
WITH stats AS (
  SELECT item_id, avg(margin) mu, stddev_samp(margin) sd, count(*) n
  FROM prices_1m
  WHERE ts > now() - interval '2 hours' AND margin IS NOT NULL
  GROUP BY item_id
),
latest AS (
  SELECT DISTINCT ON (item_id) item_id, margin, low, high_time, low_time
  FROM prices_1m ORDER BY item_id, ts DESC
)
SELECT i.name,
       l.margin                                  AS cur_margin,
       round(s.mu)                               AS avg_margin,
       round((l.margin - s.mu)/nullif(s.sd,0),2) AS z_score,
       round(l.margin::numeric/nullif(l.low,0)*100,2) AS roi_pct,
       s.n AS samples
FROM latest l JOIN stats s USING (item_id) JOIN items i USING (item_id)
WHERE s.n >= 10 AND s.sd > 0 AND l.margin > 0
  AND l.high_time > now() - interval '20 min'
  AND l.low_time  > now() - interval '20 min'
ORDER BY z_score DESC
LIMIT 25;
```
- **Tables:** `prices_1m` + `items`. **Lookback:** trailing window for baseline (2h) +
  latest. **Gotcha:** require a minimum `n` (samples) and `sd > 0`, else thin items give
  garbage z-scores.
- **→ tool:** `margin_zscore(baseline_window, min_samples, max_age, limit)`

### 5. OHLC candles — the charting / feature primitive
**Answers:** open/high/low/close per bucket for one item. Foundation for any chart or
derived temporal feature.
```sql
SELECT time_bucket('15 min', ts) AS bucket,
       first(high, ts) AS open, max(high) AS hi, min(low) AS lo, last(high, ts) AS close
FROM prices_1m
WHERE item_id = $1 AND ts > now() - interval '6 hours'
GROUP BY bucket ORDER BY bucket;
```
- **→ tool:** `item_history(name_or_id, grain, lookback, source='1m')` (resolve name via
  `items_name_lower_idx`).
- **Candles are NOT gap-filled** — an empty bucket is an omitted row, not an `locf`
  carry-forward. The gapfill building block is for the separate evenly-gridded feature path,
  not OHLC.

### 5b. OHLC + volume from 5m — the volume-bearing history source ⏳ validate before coding
**Answers:** same shape as #5 but from `prices_5m`, which is the **only** source with volume.
Note `prices_5m` carries **block-average** prices (`avg_high_price` / `avg_low_price`), not
last-trade quotes — a different price basis, labelled in the tool response.
```sql
SELECT time_bucket('15 min', ts) AS bucket,
       first(avg_high_price, ts) AS open_high,
       max(avg_high_price)       AS hi,
       min(avg_low_price)        AS lo,
       last(avg_high_price, ts)  AS close_high,
       avg(avg_high_price)::bigint AS avg_high,
       avg(avg_low_price)::bigint  AS avg_low,
       sum(high_volume)          AS high_volume,
       sum(low_volume)           AS low_volume,
       count(*)                  AS obs
FROM prices_5m
WHERE item_id = $1 AND ts > now() - interval '6 hours'
GROUP BY bucket ORDER BY bucket;
```
- **Gotcha:** nulls preserved (a side with no trade in a 5m block is null); only the volume
  *sums* coalesce to 0 implicitly via `sum`. Don't `COALESCE` the avg prices.
- **Status:** ✅ validated live 2026-07-13 as the `ge-mcp` role via the `item_history`
  tool (source=5m) — result shape confirmed against real `prices_5m` rows.
- **→ tool:** `item_history(name_or_id, grain, lookback, source='5m')`.

### 6. Volatility ranking — best range-trade candidates
**Answers:** items with the highest price dispersion (coefficient of variation) over a
window — the ones worth repeatedly buying-low/selling-high.
```sql
SELECT p.item_id, i.name,
       round(stddev_samp(coalesce(p.high,p.low)) /
             nullif(avg(coalesce(p.high,p.low)),0), 4) AS cv,
       count(*) AS obs
FROM prices_1m p JOIN items i USING (item_id)
WHERE p.ts > now() - interval '2 hours'
GROUP BY p.item_id, i.name HAVING count(*) >= 10
ORDER BY cv DESC LIMIT 25;
```
> `cv` mixes the high (ask) and low (bid) legs via `coalesce(high,low)`, so the bid-ask
> bounce itself reads as dispersion — `cv` is "spread bounce + price movement," not a clean
> mid volatility. Acceptable for research; the tool tags this in `meta`.

### 7. Volume surge — unusual activity (often precedes moves)
**Answers:** items whose latest 5m volume is far above their recent average.
```sql
WITH v AS (
  SELECT item_id, ts, coalesce(high_volume,0)+coalesce(low_volume,0) AS vol
  FROM prices_5m WHERE ts > now() - interval '3 hours'
)
SELECT v.item_id, i.name, last(v.vol, v.ts) AS cur, round(avg(v.vol)) AS baseline,
       round(last(v.vol, v.ts)/nullif(avg(v.vol),0), 2) AS surge
FROM v JOIN items i USING (item_id)
GROUP BY v.item_id, i.name HAVING avg(v.vol) > 0
ORDER BY surge DESC LIMIT 25;
```

### 8. Spread persistence — is a margin reliable or fleeting?
**Answers:** what fraction of recent observations had a flippable (positive) margin —
fill-probability, distinguishing durable spreads from one-print flukes.
```sql
SELECT p.item_id, i.name,
       round(count(*) FILTER (WHERE p.margin > 0)::numeric / count(*), 2) AS pct_flippable,
       count(*) AS obs
FROM prices_1m p JOIN items i USING (item_id)
WHERE p.ts > now() - interval '2 hours' AND p.margin IS NOT NULL
GROUP BY p.item_id, i.name HAVING count(*) >= 10
ORDER BY pct_flippable DESC LIMIT 25;
```

### 9. Momentum / trend slope
**Answers:** items trending up/down — `regr_slope` gives gp-per-second; ride the trend
or fade the extreme.
```sql
SELECT p.item_id, i.name,
       regr_slope(coalesce(p.high,p.low), extract(epoch from p.ts)) AS slope_gp_per_sec,
       count(*) AS obs
FROM prices_1m p JOIN items i USING (item_id)
WHERE p.ts > now() - interval '1 hour'
GROUP BY p.item_id, i.name HAVING count(*) >= 10
ORDER BY slope_gp_per_sec DESC LIMIT 25;
```

---

## Money signals (2026-07-13 amendment — all validated live)

Added after reviewing 26 days of accumulated data for gaps in the original surface.
Each of these was run against prod as-is and returned sensible results.

### 12. High-alch arbitrage — removed
**Removed 2026-07-14:** alching was dropped as a strategy (over-popular, weak gp/hr, and
the "best alch right now" is trivially discoverable — no research edge). The `alch_screen`
tool that this query backed was deleted; the number is kept so cross-references stay stable.

### 13. Order-flow imbalance — screen metric
**Answers:** which items have sustained one-directional pressure — `high_volume` is
insta-buys (demand), `low_volume` is insta-sells (supply). A 7d probe on Twisted bow
showed hourly imbalance vs next-hour move correlating at **−0.165 (n=169)** — weakly
*contrarian*. Not a proven signal either way: expose the evidence, let the directive
loop falsify it per-item.
```sql
SELECT p.item_id, i.name,
       coalesce(sum(high_volume),0)::bigint AS buys,
       coalesce(sum(low_volume),0)::bigint  AS sells,
       round((coalesce(sum(high_volume),0)-coalesce(sum(low_volume),0))::numeric
             / nullif(coalesce(sum(high_volume),0)+coalesce(sum(low_volume),0),0), 3) AS imbalance,
       count(*) AS obs
FROM prices_5m p JOIN items i USING (item_id)
WHERE p.ts > now() - interval '2 hours'
GROUP BY p.item_id, i.name
HAVING coalesce(sum(high_volume),0)+coalesce(sum(low_volume),0) >= 100 AND count(*) >= 10
ORDER BY abs((coalesce(sum(high_volume),0)-coalesce(sum(low_volume),0))::numeric
             / nullif(coalesce(sum(high_volume),0)+coalesce(sum(low_volume),0),0)) DESC
LIMIT 25;
```
- **Gotcha:** without the `count(*) >= min_obs` gate, single-bucket items pin to ±1.0
  and drown the list (found during validation).
- **→ tool:** `screen(metric='imbalance', window, min_obs, limit)` (+ a `min_volume` knob)

### 14. Range position — screen metric
**Answers:** where the current price sits inside its N-day band (0 = at range low,
1 = at range high). The actionable half of a range trade: `volatility` (#6) finds wide
bands, this finds *entries* — high-cv items currently near their floor.
```sql
WITH r AS (
  SELECT item_id,
         min(coalesce(low,high))  AS range_low,
         max(coalesce(high,low))  AS range_high,
         count(*) AS obs
  FROM prices_1m
  WHERE ts > now() - interval '7 days'
  GROUP BY item_id
  HAVING count(*) >= 50
),
latest AS (
  SELECT DISTINCT ON (item_id) item_id, coalesce(high,low) AS px, ts
  FROM prices_1m ORDER BY item_id, ts DESC
),
liq AS (
  SELECT DISTINCT ON (item_id) item_id,
         coalesce(high_volume,0)+coalesce(low_volume,0) AS vol5m
  FROM prices_5m WHERE ts > now() - interval '15 min'
  ORDER BY item_id, ts DESC
)
SELECT l.item_id, i.name, l.px AS cur_price, r.range_low, r.range_high,
       round((l.px - r.range_low)::numeric / nullif(r.range_high - r.range_low, 0), 3) AS range_pos,
       round((r.range_high - r.range_low)::numeric / nullif(r.range_low,0) * 100, 1) AS range_width_pct,
       r.obs, liq.vol5m
FROM latest l JOIN r USING (item_id) JOIN items i USING (item_id) JOIN liq USING (item_id)
WHERE l.ts > now() - interval '30 min'
  AND l.px > 50                                                     -- min_price floor
  AND (r.range_high - r.range_low)::numeric / nullif(r.range_low,0) > 0.05
  AND liq.vol5m >= 100
ORDER BY range_pos ASC
LIMIT 25;
```
- **Gotchas:** needs the `min_price` floor (penny items pin to 0.000 — 1gp can't go
  lower; found during validation) and the range-width gate (a 5% band isn't worth
  trading). Uses the `coalesce(high,low)` proxy — same spread-bounce caveat as #6/#9.
- **→ tool:** `screen(metric='range_position', window='7d', min_price=50, min_obs, limit)`

### 15. Spread gap — instantaneous margin vs realized spread — screen metric
**Answers:** items whose *current* 1m margin is a large multiple of what the spread
*actually averaged* (5m block averages) while volume flowed — the stale-print trap that
freshness gates alone don't catch. High ratio = the quoted margin probably isn't fillable.
```sql
WITH rs AS (
  SELECT item_id, avg(avg_high_price - avg_low_price)::bigint AS realized_spread, count(*) AS obs
  FROM prices_5m
  WHERE ts > now() - interval '2 hours'
    AND avg_high_price IS NOT NULL AND avg_low_price IS NOT NULL
  GROUP BY item_id HAVING count(*) >= 10
),
latest AS (
  SELECT DISTINCT ON (item_id) item_id, margin, high_time, low_time
  FROM prices_1m ORDER BY item_id, ts DESC
)
SELECT l.item_id, i.name, l.margin AS cur_margin, rs.realized_spread,
       round(l.margin::numeric / nullif(rs.realized_spread,0), 2) AS spread_ratio, rs.obs
FROM latest l JOIN rs USING (item_id) JOIN items i USING (item_id)
WHERE l.margin > 0 AND rs.realized_spread > 0
  AND l.high_time > now() - interval '30 min' AND l.low_time > now() - interval '30 min'
ORDER BY spread_ratio DESC LIMIT 25;
```
- **Gotcha:** `cur_margin` is **post-tax**, `realized_spread` is **pre-tax** (5m has no
  margin column and we never recompute tax — SPIKE decision #2). The ratio is therefore
  slightly *understated*; fine for a trap detector, labelled in `meta`. Sort ascending
  to instead find margins *narrower* than realized — spreads likely to widen back.
- **Validated:** top hit was Old school bond at 23.45× (642k quoted vs 27k realized) —
  precisely the artifact class this exists to expose.
- **→ tool:** `screen(metric='spread_gap', window, min_obs, limit)`

### 16. Batch quotes
**Answers:** #0 for a watchlist in one call — re-checking N candidates currently costs
N round-trips. Same SQL as #0 with `item_id = ANY($1)` in both CTEs (validated live).
- **→ tool:** `quotes(names_or_ids[], limit≤25)`

---

## Seasonality (unlocked 2026-07-13)

Originally parked under "needs more history" — **26 days have now accumulated**, past
the ~3–4 week bar for both dimensions. Validated live: hour-of-day shows real structure
(UTC hour 6 avg margin ≈ +1116 vs hour 11 ≈ −196, ~700k obs per bucket). Raw scans are
acceptable per SPEC §6 (slow is fine; no CAGG needed).

### 10. Hour-of-day seasonality ✅ unlocked
When do spreads widen / volume peak (player-population cycle)?
```sql
SELECT extract(hour from ts) AS hour_utc, round(avg(margin)) AS avg_margin, count(*) AS obs
FROM prices_1m
WHERE margin IS NOT NULL
GROUP BY hour_utc ORDER BY hour_utc;
```

### 11. Day-of-week effects ✅ unlocked (just — keep `obs` in view)
Same shape with `extract(dow from ts)`. Weekend vs weekday liquidity/spread. At ~26
days each dow bucket has only ~4 samples of that weekday — real but young; the `obs`
column keeps the confidence rules honest.

- **→ tool:** `seasonality(dimension ∈ hour|dow, name_or_id?)` — optional item filter;
  global when omitted.

---

## From queries to tools

The recurring shapes above collapse into the `ge-mcp` tool surface:

| Tool | Backed by | Params |
|---|---|---|
| `top_flips` | #2 | `min_volume, max_age, members?, sort_by, limit` |
| `movers` | #3 | `window, min_price, min_volume, limit` |
| `margin_zscore` | #4 | `baseline_window, min_samples, max_age, limit` |
| `screen` | #6–#9 | `metric, window, min_obs, limit` |
| `item_history` | #5 (+gapfill block) | `name_or_id, grain, lookback, source` |
| `quote` | #0 (building blocks #1+#2) | `name_or_id` |
| `quotes` | #16 (#0 batched) | `names_or_ids[]` |
| `lookup_item` | building block | `query, limit` (via `items_name_lower_idx`) |
| `liquidity` | building block #2 | `name_or_id, window` |
| `seasonality` | #10–#11 | `dimension, name_or_id?` |

#6–#9 and #13–#15 fold into a single `screen(metric, window, limit)` tool (volatility |
surge | persistence | momentum | imbalance | range_position | spread_gap) rather than
seven tools, unless the agent uses them distinctly. Revisit once the directive runs in
SPIKE.md show which actually get used.

The full tool surface — params, return contracts, the one read-only connection, and the
decisions still open — lives in [SPEC.md](./SPEC.md). Two changes there vs. the original
draft: `quote` (#0) is added as the falsification primitive, and `liquidity` takes
`name_or_id` (not bare `item_id`) to match `item_history`.
