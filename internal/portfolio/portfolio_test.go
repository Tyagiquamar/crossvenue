package portfolio

import (
	"testing"

	"crossvenue/internal/domain"
	"crossvenue/pkg/decimal"
)

func fp(s string) decimal.Fixed {
	v, err := decimal.Parse(s)
	if err != nil {
		panic(err)
	}
	return v
}

func TestBalancesAndSettlement(t *testing.T) {
	l := New()
	l.SetBalance(domain.VenueBinance, "USDT", fp("200000"))
	l.SetBalance(domain.VenueBinance, "BTC", fp("0"))
	l.SetBalance(domain.VenueOKX, "USDT", fp("200000"))
	l.SetBalance(domain.VenueOKX, "BTC", fp("3"))

	// Buy 1 BTC @ 100000 on binance, fee 100.
	if err := l.ApplyBuy(domain.VenueBinance, "BTC-USDT", "BTC", "USDT", fp("100000"), fp("1"), fp("100")); err != nil {
		t.Fatal(err)
	}
	b := l.Balance(domain.VenueBinance, "USDT")
	if b.Available.String() != "99900" {
		t.Fatalf("usdt %s", b.Available)
	}
	if got := l.Balance(domain.VenueBinance, "BTC").Available; got.String() != "1" {
		t.Fatalf("btc %s", got)
	}

	// Sell 0.7 BTC @ 100030 on OKX, fee 70.021
	if err := l.ApplySell(domain.VenueOKX, "BTC-USDT", "BTC", "USDT", fp("100030"), fp("0.7"), fp("70.021")); err != nil {
		t.Fatal(err)
	}
	if got := l.Balance(domain.VenueOKX, "BTC").Available; got.String() != "2.3" {
		t.Fatalf("okx btc %s", got)
	}
	// proceeds 70021 - 70.021 = 69950.979
	if got := l.Balance(domain.VenueOKX, "USDT").Available; got.String() != "269950.979" {
		t.Fatalf("okx usdt %s", got)
	}
}

func TestInsufficientFunds(t *testing.T) {
	l := New()
	l.SetBalance(domain.VenueBinance, "USDT", fp("10"))
	if err := l.ApplyBuy(domain.VenueBinance, "BTC-USDT", "BTC", "USDT", fp("100000"), fp("1"), fp("0")); err == nil {
		t.Fatal("must reject insufficient funds")
	}
}

func TestExposure(t *testing.T) {
	l := New()
	l.SetBalance(domain.VenueBinance, "USDT", fp("1000000"))
	if err := l.ApplyBuy(domain.VenueBinance, "BTC-USDT", "BTC", "USDT", fp("100000"), fp("2"), fp("0")); err != nil {
		t.Fatal(err)
	}
	if got := l.VenueExposure(domain.VenueBinance); got.String() != "2" {
		t.Fatalf("venue exposure %s", got)
	}
	if got := l.TotalExposure(); got.String() != "2" {
		t.Fatalf("total exposure %s", got)
	}
	l.AddRealized(fp("-50"))
	if got := l.Realized(); got.String() != "-50" {
		t.Fatalf("realized %s", got)
	}
}
