package risk

import (
	"testing"
	"time"

	"crossvenue/internal/book"
	"crossvenue/internal/domain"
	"crossvenue/pkg/decimal"
)

func fx(s string) decimal.Fixed {
	v, err := decimal.Parse(s)
	if err != nil {
		panic(err)
	}
	return v
}

type fakeExpo struct{ venue, total, realized decimal.Fixed }

func (f fakeExpo) VenueExposure(domain.Venue) decimal.Fixed { return f.venue }
func (f fakeExpo) TotalExposure() decimal.Fixed             { return f.total }
func (f fakeExpo) Realized() decimal.Fixed                  { return f.realized }

func readyBooks(t *testing.T) *book.Manager {
	t.Helper()
	m := book.NewManager()
	now := time.Now()
	for _, v := range []domain.Venue{domain.VenueBinance, domain.VenueOKX} {
		b := m.Get(v, "BTC-USDT")
		b.LoadSnapshot(domain.BookSnapshot{
			Venue: v, Symbol: "BTC-USDT", Sequence: 1,
			Bids:        []domain.Level{{Price: fx("100000"), Qty: fx("1")}},
			Asks:        []domain.Level{{Price: fx("100010"), Qty: fx("1")}},
			ReceiveTime: now,
		})
	}
	return m
}

func TestAllowsValidTrade(t *testing.T) {
	e := New(Limits{
		MaxTradeNotional: fx("10000"),
		MinExpectedEdge:  fx("1"),
		MaxQuoteAge:      time.Second,
	}, readyBooks(t), fakeExpo{})
	d := e.CheckOpportunity(time.Now(), "BTC-USDT",
		domain.VenueBinance, domain.VenueOKX,
		fx("0.01"), fx("100000"), fx("100030"), fx("2"), nil)
	if !d.Allowed {
		t.Fatalf("should allow: %v", d.Reasons)
	}
}

func TestKillSwitchBlocks(t *testing.T) {
	e := New(Limits{}, readyBooks(t), fakeExpo{})
	e.ActivateKillSwitch("manual test")
	d := e.CheckOpportunity(time.Now(), "BTC-USDT",
		domain.VenueBinance, domain.VenueOKX,
		fx("0.01"), fx("100000"), fx("100030"), fx("2"), nil)
	if d.Allowed {
		t.Fatal("kill switch must block")
	}
	found := false
	for _, r := range d.Reasons {
		if r == "kill_switch_active:manual test" {
			found = true
		}
	}
	if !found {
		t.Fatalf("reasons %v", d.Reasons)
	}
	e.ResetKillSwitch()
	d = e.CheckOpportunity(time.Now(), "BTC-USDT",
		domain.VenueBinance, domain.VenueOKX,
		fx("0.01"), fx("100000"), fx("100030"), fx("2"), nil)
	if !d.Allowed {
		t.Fatalf("after reset should allow: %v", d.Reasons)
	}
}

func TestNotionalLimit(t *testing.T) {
	e := New(Limits{MaxTradeNotional: fx("100")}, readyBooks(t), fakeExpo{})
	d := e.CheckOpportunity(time.Now(), "BTC-USDT",
		domain.VenueBinance, domain.VenueOKX,
		fx("1"), fx("100000"), fx("100030"), fx("100"), nil)
	if d.Allowed {
		t.Fatal("must reject oversized trade")
	}
}

func TestStaleBookRejected(t *testing.T) {
	m := readyBooks(t)
	m.Get(domain.VenueBinance, "BTC-USDT").MarkStale(true)
	e := New(Limits{}, m, fakeExpo{})
	d := e.CheckOpportunity(time.Now(), "BTC-USDT",
		domain.VenueBinance, domain.VenueOKX,
		fx("0.01"), fx("100000"), fx("100030"), fx("2"), nil)
	if d.Allowed {
		t.Fatal("stale book must reject")
	}
}

func TestDailyLossLimit(t *testing.T) {
	e := New(Limits{DailyLossLimit: fx("100")}, readyBooks(t), fakeExpo{realized: fx("-150")})
	d := e.CheckOpportunity(time.Now(), "BTC-USDT",
		domain.VenueBinance, domain.VenueOKX,
		fx("0.01"), fx("100000"), fx("100030"), fx("2"), nil)
	if d.Allowed {
		t.Fatal("daily loss limit must reject")
	}
}

func TestConsecutiveFailures(t *testing.T) {
	e := New(Limits{MaxConsecutiveFails: 2}, readyBooks(t), fakeExpo{})
	e.NoteExecutionResult(false)
	e.NoteExecutionResult(false)
	d := e.CheckOpportunity(time.Now(), "BTC-USDT",
		domain.VenueBinance, domain.VenueOKX,
		fx("0.01"), fx("100000"), fx("100030"), fx("2"), nil)
	if d.Allowed {
		t.Fatal("consecutive failures must reject")
	}
	e.NoteExecutionResult(true)
	d = e.CheckOpportunity(time.Now(), "BTC-USDT",
		domain.VenueBinance, domain.VenueOKX,
		fx("0.01"), fx("100000"), fx("100030"), fx("2"), nil)
	if !d.Allowed {
		t.Fatalf("success resets counter: %v", d.Reasons)
	}
}
