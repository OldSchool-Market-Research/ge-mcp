package tools

import "fmt"

// The two persistence stats (persistence-fields, 2026-08). They exist so
// "the margin is persistent" can be a cited tool number instead of an
// eyeballed OHLC read — the same law as every other field: the agent can't
// cite what a tool didn't return.
//
//   margin_persistence_24h — share of the last 24 wall-clock hours whose
//     hourly average post-tax spread held >= 50% of the CURRENT margin.
//     Fixed /24 denominator: an hour where either side didn't trade counts
//     as not-persistent (an unobserved spread is not a demonstrated spread);
//     persist_obs_hours carries the both-sides sample count. NULL when the
//     current margin is NULL — no reference spread, no persistence claim.
//   roundtrips_24h — 30-min buckets in the last 24h where BOTH sides traded
//     and the bucket's own average prices cleared a positive post-tax
//     spread: how many times a profitable round trip actually printed
//     today. The high-value screen's dead-book detector: a quoted 2M spread
//     whose legs are days apart scores 0 here.
//
// Tax matches the schema rule exactly: least(floor(high/50), 5000000).

// persistenceSelect returns the three output columns. marginRef is a trusted
// Go-side column reference (sprintfSQL contract: closed set, never input).
func persistenceSelect(marginRef string) string {
	return fmt.Sprintf(`CASE WHEN %s IS NULL THEN NULL
       ELSE round(persist.ok_hours / 24.0, 2) END::float8 AS margin_persistence_24h,
       persist.obs_hours::int AS persist_obs_hours,
       rt.roundtrips::int AS roundtrips_24h`, marginRef)
}

// persistenceJoins returns the two LEFT JOIN LATERAL clauses computing the
// stats for one outer row. itemRef/marginRef are trusted Go-side column
// references (sprintfSQL contract). Each lateral touches only the outer
// row's item via the (item_id, ts DESC) index on prices_5m.
func persistenceJoins(itemRef, marginRef string) string {
	return fmt.Sprintf(`LEFT JOIN LATERAL (
  SELECT count(*) FILTER (WHERE h.hi - least(floor(h.hi/50), 5000000) - h.lo >= %[2]s * 0.5) AS ok_hours,
         count(*) AS obs_hours
  FROM (
    SELECT avg(avg_high_price) FILTER (WHERE high_volume > 0) AS hi,
           avg(avg_low_price)  FILTER (WHERE low_volume  > 0) AS lo
    FROM prices_5m
    WHERE item_id = %[1]s AND ts > now() - interval '24 hours'
    GROUP BY date_trunc('hour', ts)
  ) h
  WHERE h.hi IS NOT NULL AND h.lo IS NOT NULL
) persist ON true
LEFT JOIN LATERAL (
  SELECT count(*) AS roundtrips FROM (
    SELECT 1
    FROM prices_5m
    WHERE item_id = %[1]s AND ts > now() - interval '24 hours'
    GROUP BY date_trunc('hour', ts), (extract(minute from ts)::int / 30)
    HAVING sum(high_volume) > 0 AND sum(low_volume) > 0
       AND avg(avg_high_price) FILTER (WHERE high_volume > 0)
           - least(floor(avg(avg_high_price) FILTER (WHERE high_volume > 0) / 50), 5000000)
           - avg(avg_low_price) FILTER (WHERE low_volume > 0) > 0
  ) w
) rt ON true`, itemRef, marginRef)
}
