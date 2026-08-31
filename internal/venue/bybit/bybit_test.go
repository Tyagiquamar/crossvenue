package bybit

import (
	"context"
	"testing"
)

func TestSnapshotAndDelta(t *testing.T) {
	a := New()
	snap := []byte(`{"topic":"orderbook.50.BTCUSDT","type":"snapshot","ts":1756700000000,
		"data":{"s":"BTCUSDT","b":[["100000","1"]],"a":[["100010","2"]],"u":100,"seq":1}}`)
	evs, err := a.handle(context.Background(), snap)
	if err != nil {
		t.Fatal(err)
	}
	if evs[0].Snapshot.Sequence != 100 || evs[0].Snapshot.Symbol != "BTC-USDT" {
		t.Fatalf("snap %+v", evs[0].Snapshot)
	}

	delta := []byte(`{"topic":"orderbook.50.BTCUSDT","type":"delta","ts":1756700000100,
		"data":{"s":"BTCUSDT","b":[["100001","0.5"]],"a":[],"u":101,"seq":2}}`)
	evs, err = a.handle(context.Background(), delta)
	if err != nil {
		t.Fatal(err)
	}
	if evs[0].Delta.Sequence != 101 {
		t.Fatalf("delta %+v", evs[0].Delta)
	}
}

func TestAckIgnored(t *testing.T) {
	a := New()
	evs, err := a.handle(context.Background(), []byte(`{"success":true,"op":"subscribe"}`))
	if err != nil || len(evs) != 0 {
		t.Fatal("ack must be ignored")
	}
}
