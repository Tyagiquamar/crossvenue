package execution

import (
	"sync"
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

func mkBooks(t *testing.T) *book.Manager {
	t.Helper()
	m := book.NewManager()
	for _, v := range []domain.Venue{domain.VenueBinance, domain.VenueOKX} {
		b := m.Get(v, "BTC-USDT")
		b.LoadSnapshot(domain.BookSnapshot{
			Venue:  v,
			Symbol: "BTC-USDT",
			Bids: []domain.Level{
				{Price: fx("100000"), Qty: fx("1")},
				{Price: fx("99990"), Qty: fx("2")},
			},
			Asks: []domain.Level{
				{Price: fx("100010"), Qty: fx("0.4")},
				{Price: fx("100020"), Qty: fx("0.6")},
				{Price: fx("100050"), Qty: fx("1")},
			},
			Sequence:    1,
			ReceiveTime: time.Unix(1000, 0),
		})
	}
	return m
}

func TestStateMachine(t *testing.T) {
	o := &Order{State: Pending, Quantity: fx("1")}
	if err := o.Transition(Filled, time.Now()); err == nil {
		t.Fatal("pending->filled must be rejected")
	}
	if err := o.Transition(Accepted, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := o.Transition(PartiallyFilled, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := o.Transition(Filled, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := o.Transition(Accepted, time.Now()); err == nil {
		t.Fatal("filled->accepted must be rejected")
	}
}

func TestPartialFillVWAP(t *testing.T) {
	m := mkBooks(t)
	s := NewSimulator(m, map[domain.Venue]int64{domain.VenueBinance: 10}, nil)
	ord, err := s.Submit(Order{
		ClientOrderID: "c1", Venue: domain.VenueBinance, Symbol: "BTC-USDT",
		Side: domain.Buy, Type: Market, Quantity: fx("2"),
	}, time.Unix(1000, 0))
	if err != nil {
		t.Fatal(err)
	}
	lr, err := s.FillAgainstBook(ord, time.Unix(1000, 0))
	if err != nil {
		t.Fatal(err)
	}
	// fills 0.4@100010 + 0.6@100020 + 1@100050 = 2.0
	if lr.FilledQty.String() != "2" {
		t.Fatalf("filled %s", lr.FilledQty)
	}
	// cost = 40004 + 60012 + 100050 = 200066, vwap = 100033
	if lr.VWAP.String() != "100033" {
		t.Fatalf("vwap %s", lr.VWAP)
	}
	if ord.State != Filled {
		t.Fatalf("state %s", ord.State)
	}
	// fee: 10bps of 200066 = 200.066
	if lr.Fee.String() != "200.066" {
		t.Fatalf("fee %s", lr.Fee)
	}
}

func TestIOCPartialThenCancel(t *testing.T) {
	m := mkBooks(t)
	s := NewSimulator(m, nil, nil)
	// IOC buy 3 with limit 100020 -> fills 1.0 (0.4+0.6), remainder cancelled.
	ord, _ := s.Submit(Order{
		ClientOrderID: "ioc1", Venue: domain.VenueBinance, Symbol: "BTC-USDT",
		Side: domain.Buy, Type: IOCLimit, Price: fx("100020"), Quantity: fx("3"),
	}, time.Unix(1000, 0))
	lr, err := s.FillAgainstBook(ord, time.Unix(1000, 0))
	if err != nil {
		t.Fatal(err)
	}
	if lr.FilledQty.String() != "1" {
		t.Fatalf("filled %s", lr.FilledQty)
	}
	if ord.State != Cancelled {
		t.Fatalf("IOC remainder must cancel, state %s", ord.State)
	}
}

func TestIdempotentSubmit(t *testing.T) {
	s := NewSimulator(mkBooks(t), nil, nil)
	now := time.Unix(1000, 0)
	o1, _ := s.Submit(Order{ClientOrderID: "dup", Venue: domain.VenueBinance, Symbol: "BTC-USDT", Side: domain.Buy, Type: Market, Quantity: fx("1")}, now)
	var wg sync.WaitGroup
	ids := make([]string, 32)
	for i := range ids {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			o, err := s.Submit(Order{ClientOrderID: "dup", Venue: domain.VenueBinance, Symbol: "BTC-USDT", Side: domain.Buy, Type: Market, Quantity: fx("1")}, now)
			if err != nil {
				t.Error(err)
				return
			}
			ids[i] = o.ID
		}(i)
	}
	wg.Wait()
	for _, id := range ids {
		if id != o1.ID {
			t.Fatalf("duplicate client order id produced new order: %s vs %s", id, o1.ID)
		}
	}
	if len(s.Orders()) != 1 {
		t.Fatalf("expected 1 order, got %d", len(s.Orders()))
	}
}

func TestTwoLegResidualExposure(t *testing.T) {
	m := book.NewManager()
	// Buy venue has deep asks; sell venue has only 0.7 bid depth.
	buy := m.Get(domain.VenueBinance, "BTC-USDT")
	buy.LoadSnapshot(domain.BookSnapshot{
		Venue: domain.VenueBinance, Symbol: "BTC-USDT", Sequence: 1,
		Asks:        []domain.Level{{Price: fx("100000"), Qty: fx("5")}},
		Bids:        []domain.Level{{Price: fx("99990"), Qty: fx("5")}},
		ReceiveTime: time.Unix(1000, 0),
	})
	sell := m.Get(domain.VenueOKX, "BTC-USDT")
	sell.LoadSnapshot(domain.BookSnapshot{
		Venue: domain.VenueOKX, Symbol: "BTC-USDT", Sequence: 1,
		Bids:        []domain.Level{{Price: fx("100030"), Qty: fx("0.7")}},
		Asks:        []domain.Level{{Price: fx("100040"), Qty: fx("5")}},
		ReceiveTime: time.Unix(1000, 0),
	})
	s := NewSimulator(m, nil, nil)
	now := time.Unix(1000, 0)
	buyOrd, _ := s.Submit(Order{ClientOrderID: "b1", Venue: domain.VenueBinance, Symbol: "BTC-USDT", Side: domain.Buy, Type: Market, Quantity: fx("1")}, now)
	sellOrd, _ := s.Submit(Order{ClientOrderID: "s1", Venue: domain.VenueOKX, Symbol: "BTC-USDT", Side: domain.Sell, Type: Market, Quantity: fx("1")}, now)
	res, err := s.ExecuteArbitrage("opp-1", buyOrd, sellOrd, PolicyParallel, now)
	if err != nil {
		t.Fatal(err)
	}
	if res.BuyLeg.FilledQty.String() != "1" || res.SellLeg.FilledQty.String() != "0.7" {
		t.Fatalf("legs: %s / %s", res.BuyLeg.FilledQty, res.SellLeg.FilledQty)
	}
	if res.ResidualQty.String() != "0.3" {
		t.Fatalf("residual %s", res.ResidualQty)
	}
	// Realized on matched 0.7: 0.7*(100030-100000) = 21
	if res.RealizedPnL.String() != "21" {
		t.Fatalf("pnl %s", res.RealizedPnL)
	}
}
