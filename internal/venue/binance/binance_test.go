package binance

import (
	"context"
	"testing"
)

func TestHandleDepthUpdate(t *testing.T) {
	a := New()
	raw := []byte(`{
		"stream": "btcusdt@depth@100ms",
		"data": {"e":"depthUpdate","E":1756700000000,"s":"BTCUSDT","U":100,"u":105,
			"b":[["100000.00","1.50000000"],["99999.00","0"]],
			"a":[["100010.00","2.00000000"]]}
	}`)
	evs, err := a.handle(context.Background(), raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 1 || evs[0].Type != 2 {
		t.Fatalf("expected delta, got %+v", evs)
	}
	d := evs[0].Delta
	if d.Symbol != "BTC-USDT" || d.Sequence != 105 || d.PrevSequence != 100 {
		t.Fatalf("delta meta: %+v", d)
	}
	if len(d.Bids) != 2 || d.Bids[0].Price.String() != "100000" || d.Bids[1].Qty.String() != "0" {
		t.Fatalf("bids: %+v", d.Bids)
	}
}

func TestHandleDepth20Snapshot(t *testing.T) {
	a := New()
	raw := []byte(`{
		"stream": "ethusdt@depth20@100ms",
		"data": {"lastUpdateId": 555,
			"bids":[["4000.00","1"]],
			"asks":[["4001.00","2"]]}
	}`)
	evs, err := a.handle(context.Background(), raw)
	if err != nil {
		t.Fatal(err)
	}
	s := evs[0].Snapshot
	if s.Symbol != "ETH-USDT" || s.Sequence != 555 {
		t.Fatalf("snap: %+v", s)
	}
}

func TestRejectsMalformed(t *testing.T) {
	a := New()
	if _, err := a.handle(context.Background(), []byte(`{"stream":"btcusdt@depth@100ms","data":{"e":"depthUpdate","E":1,"s":"X","U":1,"u":2,"b":[["bad","1"]],"a":[]}}`)); err == nil {
		t.Fatal("malformed price must error")
	}
}
