package tools

import "testing"

func fp(v float64) *float64 { return &v }
func ip(v int64) *int64     { return &v }

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
	h := harnessQuote(q, processLegs(1000, 2000), false)
	if !h.Shippable || !h.ClearsFloor || h.Why != "" {
		t.Fatalf("%+v", h)
	}
	// Slippage: 0.5% of 3000 is 15, so 885 x 550.
	if *h.MarginHaircut != 885 || *h.ActionsPerHour != 550 || *h.GpHr != 885*550 || *h.UnitsPer4h != 2200 {
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
	h := harnessQuote(q, processLegs(14_000_000, 14_500_000), false)
	if *h.ActionsPerHour >= 1 || *h.UnitsPer4h != 3 {
		t.Errorf("rate %v units %v", *h.ActionsPerHour, *h.UnitsPer4h)
	}
	if h.ClearsFloor || h.Shippable || h.Why == "" {
		t.Errorf("under the floor: %+v", h)
	}
}

func TestHarnessQuoteRefuses(t *testing.T) {
	good := processQuoteRow{Throughput: "fixed", MarginSlow: fp(900), ActionsPerHour: fp(600), LimitActionsPerHour: fp(600), MarketActionsPerHour: fp(9000)}

	if h := harnessQuote(good, processLegs(1000, 2000), true); h.Shippable || h.Why == "" {
		t.Errorf("a C relation ships through C: %+v", h)
	}
	unknown := good
	unknown.Throughput = "unknown"
	if h := harnessQuote(unknown, processLegs(1000, 2000), false); h.Shippable {
		t.Errorf("no known rate: %+v", h)
	}
	limitless := good
	limitless.UnknownLimitLegs = 1
	if h := harnessQuote(limitless, processLegs(1000, 2000), false); h.Shippable {
		t.Errorf("unknown limit: %+v", h)
	}
	// Nulls are signal: an unpriced leg gives null numbers, never zero.
	h := harnessQuote(good, []processLeg{{Side: "buy", Qty: fp(1)}, {Side: "sell", Qty: fp(1), PriceSlow: ip(2000)}}, false)
	if h.Shippable || h.GpHr != nil || h.EntryPerAction != nil || h.Why == "" {
		t.Errorf("unpriced leg: %+v", h)
	}
	noMargin := good
	noMargin.MarginSlow = nil
	if h := harnessQuote(noMargin, processLegs(1000, 2000), false); h.GpHr != nil || h.Shippable {
		t.Errorf("no margin: %+v", h)
	}
}
