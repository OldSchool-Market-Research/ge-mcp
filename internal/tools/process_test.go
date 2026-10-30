package tools

import "testing"

func fp(v float64) *float64 { return &v }
func ip(v int64) *int64     { return &v }

// paid has the history the persistence gate wants.
var paid = processPersistence{Hours: 24, AtFloor: 24}

func processLegs(in, out int64) []processLeg {
	return []processLeg{
		{Side: "buy", Qty: fp(1), PriceSlow: ip(in)},
		{Side: "sell", Qty: fp(1), PriceSlow: ip(out)},
	}
}

func TestHarnessQuote(t *testing.T) {
	// Red-leather shape: 1000 in, 2000 out, margin 900 after tax; player 1600/hr,
	// limit 550/hr, market 5930/hr so 15% is 889.5: the limit binds.
	q := processQuoteRow{Throughput: "fixed", MarginSlow: fp(900), FeeGp: 0,
		ActionsPerHour: fp(1600), LimitActionsPerHour: fp(550), MarketActionsPerHour: fp(5930)}
	h := harnessQuote(q, processLegs(1000, 2000), false, paid)
	if !h.Shippable || !h.ClearsFloor || h.Why != "" {
		t.Fatalf("%+v", h)
	}
	// Slippage: 0.5% of 3000 is 15, so 885 x 550.
	if *h.MarginHaircut != 885 || *h.ActionsPerHour != 550 || *h.GpHr != int64(885*550) || *h.UnitsPer4h != 2200 {
		t.Errorf("margin %v rate %v gp %v units %v", *h.MarginHaircut, *h.ActionsPerHour, *h.GpHr, *h.UnitsPer4h)
	}
	if *h.EntryPerAction != 1000 || *h.ExitPerAction != 1900 {
		t.Errorf("entry %v exit %v", *h.EntryPerAction, *h.ExitPerAction)
	}
}

func TestHarnessQuoteCapsByMarketAndBudget(t *testing.T) {
	// A view reading 10/hr at 300k margin is 3M gp/hr. 15% of the market is 1.5/hr,
	// and 14M inputs on a 50M budget is about 0.89/hr: the budget binds.
	q := processQuoteRow{Throughput: "fixed", MarginSlow: fp(300_000),
		ActionsPerHour: fp(10), LimitActionsPerHour: fp(313), MarketActionsPerHour: fp(10)}
	h := harnessQuote(q, processLegs(14_000_000, 14_500_000), false, paid)
	if *h.ActionsPerHour >= 1 || *h.UnitsPer4h != 3 {
		t.Errorf("rate %v units %v", *h.ActionsPerHour, *h.UnitsPer4h)
	}
	if h.ClearsFloor || h.Shippable || h.Why == "" {
		t.Errorf("under the floor: %+v", h)
	}
}

func TestHarnessQuoteRefuses(t *testing.T) {
	good := processQuoteRow{Throughput: "fixed", MarginSlow: fp(900), ActionsPerHour: fp(600), LimitActionsPerHour: fp(600), MarketActionsPerHour: fp(9000)}

	if h := harnessQuote(good, processLegs(1000, 2000), true, paid); h.Shippable || h.Why == "" {
		t.Errorf("a C relation ships through C: %+v", h)
	}
	unknown := good
	unknown.Throughput = "unknown"
	if h := harnessQuote(unknown, processLegs(1000, 2000), false, paid); h.Shippable {
		t.Errorf("no known rate: %+v", h)
	}
	limitless := good
	limitless.UnknownLimitLegs = 1
	if h := harnessQuote(limitless, processLegs(1000, 2000), false, paid); h.Shippable {
		t.Errorf("unknown limit: %+v", h)
	}
	// Nulls are signal: an unpriced leg gives null numbers, never zero.
	h := harnessQuote(good, []processLeg{{Side: "buy", Qty: fp(1)}, {Side: "sell", Qty: fp(1), PriceSlow: ip(2000)}}, false, paid)
	if h.Shippable || h.GpHr != nil || h.EntryPerAction != nil || h.Why == "" {
		t.Errorf("unpriced leg: %+v", h)
	}
	noMargin := good
	noMargin.MarginSlow = nil
	if h := harnessQuote(noMargin, processLegs(1000, 2000), false, paid); h.GpHr != nil || h.Shippable {
		t.Errorf("no margin: %+v", h)
	}
}

// What a strategy copies must be integers, or the client rejects the whole report.
func TestHarnessQuoteNumbersAreWhole(t *testing.T) {
	// Fractional qty and fee give a fractional cost and margin.
	q := processQuoteRow{Throughput: "fixed", MarginSlow: fp(1234.567), FeeGp: 0.415,
		ActionsPerHour: fp(1000), LimitActionsPerHour: fp(1000), MarketActionsPerHour: fp(10000)}
	legs := []processLeg{{Side: "buy", Qty: fp(1.5), PriceSlow: ip(4001)}, {Side: "sell", Qty: fp(1), PriceSlow: ip(8000)}}
	h := harnessQuote(q, legs, false, paid)
	if *h.EntryPerAction != 6002 || *h.ExitPerAction != 7236 {
		t.Errorf("entry %d exit %d", *h.EntryPerAction, *h.ExitPerAction)
	}
	// 1234.567 - 0.5% of (6001.5 + 8000) = 1164.5595, x 1000/hr, truncated.
	if *h.GpHr != 1164559 {
		t.Errorf("gp %d", *h.GpHr)
	}
	if *h.MarginHaircut != 1164.56 {
		t.Errorf("margin %v", *h.MarginHaircut)
	}
}

// The harness sizes in whole actions per 4h, as the orchestrator does from units_used.
func TestHarnessQuoteUsesWholeUnits(t *testing.T) {
	// 15% of 3.5/hr is 0.525/hr, so 2.1 per 4h is 2, or 0.5/hr: 400k, not 420k.
	q := processQuoteRow{Throughput: "fixed", MarginSlow: fp(800_000),
		ActionsPerHour: fp(10), LimitActionsPerHour: fp(10), MarketActionsPerHour: fp(3.5)}
	h := harnessQuote(q, processLegs(1000, 801_000), false, processPersistence{Hours: 24, AtFloor: 24})
	if *h.UnitsPer4h != 2 || *h.ActionsPerHour != 0.5 {
		t.Errorf("units %d rate %v", *h.UnitsPer4h, *h.ActionsPerHour)
	}
	// Under one action per 4h can't be sized at all.
	q.MarketActionsPerHour = fp(1)
	if h := harnessQuote(q, processLegs(1000, 801_000), false, paid); h.Shippable || *h.UnitsPer4h != 0 {
		t.Errorf("no whole unit must not ship: %+v", h)
	}
}

func TestHarnessQuotePersistenceAndFreshness(t *testing.T) {
	good := processQuoteRow{Throughput: "fixed", MarginSlow: fp(900), ActionsPerHour: fp(600), LimitActionsPerHour: fp(600), MarketActionsPerHour: fp(6000)}
	for _, tc := range []struct {
		name string
		p    processPersistence
		ok   bool
	}{
		{"always at the floor", processPersistence{24, 24}, true},
		{"exactly the gate", processPersistence{20, 16}, true},
		{"too little history", processPersistence{11, 11}, false},
		{"a spike", processPersistence{22, 1}, false},
	} {
		h := harnessQuote(good, processLegs(1000, 2000), false, tc.p)
		if h.Shippable != tc.ok || (h.Why == "") != tc.ok {
			t.Errorf("%s: %+v", tc.name, h)
		}
		if h.Hours24h != tc.p.Hours || h.HoursAtFloor24h != tc.p.AtFloor {
			t.Errorf("%s: counts not reported: %+v", tc.name, h)
		}
	}
	stale := good
	stale.MaxLegAgeS = ip(processMaxLegAgeS + 1)
	if h := harnessQuote(stale, processLegs(1000, 2000), false, paid); h.Shippable {
		t.Errorf("a stale leg must not ship: %+v", h)
	}
	stale.MaxLegAgeS = ip(processMaxLegAgeS)
	if h := harnessQuote(stale, processLegs(1000, 2000), false, paid); !h.Shippable {
		t.Errorf("at the limit still ships: %+v", h)
	}
}

func TestMainOutput(t *testing.T) {
	id := func(v int) *int { return &v }
	legs := []processLeg{
		{Side: "buy", ItemID: id(561), Qty: fp(5)},
		{Side: "sell", ItemID: id(30), Qty: fp(1)},
		{Side: "sell", ItemID: id(20), Qty: fp(2)},
		{Side: "sell", ItemID: id(10), Qty: fp(2)},
	}
	if got := mainOutput(legs); got == nil || *got != 10 {
		t.Errorf("want the most-made sell leg, lowest id on a tie: %v", got)
	}
	if got := mainOutput(legs[:1]); got != nil {
		t.Errorf("no sell leg, no output: %v", *got)
	}
}
