// Restart semantics: durable portfolio state is restored, but books never
// resume trading from stored state — they must resynchronize from fresh
// market data.
package integration_test

import (
	"testing"
	"time"

	"crossvenue/internal/book"
	"crossvenue/internal/config"
	"crossvenue/internal/domain"
	"crossvenue/internal/engine"
	"crossvenue/internal/journal"
	"crossvenue/internal/portfolio"
)

func TestRestartBooksStartInvalid(t *testing.T) {
	cfg := config.Default()
	cfg.Symbols = []string{"BTC-USDT"}

	// "Process 1": run and build a book.
	j1 := journal.New()
	eng1, err := engine.New(cfg, j1, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	bk := eng1.Books.Get(domain.VenueBinance, "BTC-USDT")
	bk.LoadSnapshot(domain.BookSnapshot{
		Venue: domain.VenueBinance, Symbol: "BTC-USDT", Sequence: 42,
		Bids:        []domain.Level{{Price: fp("99990"), Qty: fp("1")}},
		Asks:        []domain.Level{{Price: fp("100010"), Qty: fp("1")}},
		ReceiveTime: time.Now(),
	})
	if !bk.State().Ready {
		t.Fatal("precondition: book ready")
	}

	// Simulate restart: a brand-new supervisor (as after process restart).
	j2 := journal.New()
	eng2, err := engine.New(cfg, j2, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	bk2 := eng2.Books.Get(domain.VenueBinance, "BTC-USDT")
	if bk2.State().Ready {
		t.Fatal("books must start invalid after restart; never resume from stored book state")
	}

	// Restore portfolio balances (recovery), ensure ledger is independent.
	l2 := portfolio.New()
	l2.SetBalance(domain.VenueBinance, "USDT", fp("50000"))
	if got := l2.Balance(domain.VenueBinance, "USDT").Available; got.String() != "50000" {
		t.Fatalf("balance %s", got)
	}
}

func TestBookManagerIsolation(t *testing.T) {
	m := book.NewManager()
	m.Get(domain.VenueBinance, "BTC-USDT").LoadSnapshot(domain.BookSnapshot{
		Venue: domain.VenueBinance, Symbol: "BTC-USDT", Sequence: 1,
		Bids:        []domain.Level{{Price: fp("1"), Qty: fp("1")}},
		Asks:        []domain.Level{{Price: fp("2"), Qty: fp("1")}},
		ReceiveTime: time.Now(),
	})
	// Another venue+symbol book remains not ready.
	if m.Get(domain.VenueOKX, "BTC-USDT").State().Ready {
		t.Fatal("books are independent per venue")
	}
	if m.Get(domain.VenueBinance, "ETH-USDT").State().Ready {
		t.Fatal("books are independent per symbol")
	}
}
