package okx

import (
	"context"
	"testing"
)

func TestSnapshotAndUpdate(t *testing.T) {
	a := New()
	snap := []byte(`{"arg":{"channel":"books","instId":"BTC-USDT"},"action":"snapshot",
		"data":[{"bids":[["100000","1","0","1"]],"asks":[["100010","2","0","1"]],"ts":"1756700000000","seqId":100}]}`)
	evs, err := a.handle(context.Background(), snap)
	if err != nil {
		t.Fatal(err)
	}
	if evs[0].Snapshot.Sequence != 100 || evs[0].Snapshot.Symbol != "BTC-USDT" {
		t.Fatalf("snap %+v", evs[0].Snapshot)
	}

	upd := []byte(`{"arg":{"channel":"books","instId":"BTC-USDT"},"action":"update",
		"data":[{"bids":[["100001","0.5","0","1"]],"asks":[],"ts":"1756700000100","seqId":101,"prevSeqId":100}]}`)
	evs, err = a.handle(context.Background(), upd)
	if err != nil {
		t.Fatal(err)
	}
	d := evs[0].Delta
	if d.Sequence != 101 || d.PrevSequence != 100 {
		t.Fatalf("delta %+v", d)
	}
}

func TestPongIgnored(t *testing.T) {
	a := New()
	evs, err := a.handle(context.Background(), []byte("pong"))
	if err != nil || len(evs) != 0 {
		t.Fatal("pong must be ignored")
	}
}
