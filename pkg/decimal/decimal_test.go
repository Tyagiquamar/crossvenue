package decimal

import (
	"testing"
)

func mustParse(t *testing.T, s string) Fixed {
	t.Helper()
	f, err := Parse(s)
	if err != nil {
		t.Fatalf("Parse(%q): %v", s, err)
	}
	return f
}

func TestParseRoundTrip(t *testing.T) {
	cases := []string{"0", "1", "-1", "100000.01", "0.00000001", "123456.789", "-0.5", "2"}
	for _, c := range cases {
		f := mustParse(t, c)
		if got := f.String(); got != c {
			t.Errorf("round trip %q -> %q", c, got)
		}
	}
}

func TestParseRejects(t *testing.T) {
	bad := []string{"", "abc", "1.2.3", "1e5", "0.123456789", "--1", "1,000", ".", "-"}
	for _, b := range bad {
		if _, err := Parse(b); err == nil {
			t.Errorf("Parse(%q) expected error", b)
		}
	}
}

func TestArithmetic(t *testing.T) {
	a := mustParse(t, "100000")
	b := mustParse(t, "0.5")
	if got := a.Mul(b); got.String() != "50000" {
		t.Errorf("mul: %s", got)
	}
	if got := a.Add(b); got.String() != "100000.5" {
		t.Errorf("add: %s", got)
	}
	if got := a.Sub(b); got.String() != "99999.5" {
		t.Errorf("sub: %s", got)
	}
	if got := a.Div(mustParse(t, "4")); got.String() != "25000" {
		t.Errorf("div: %s", got)
	}
}

func TestMulBps(t *testing.T) {
	notional := mustParse(t, "200000") // 2 BTC * 100k
	fee := notional.MulBps(10)         // 10 bps = 0.1%
	if fee.String() != "200" {
		t.Errorf("fee: %s", fee)
	}
}

func TestVWAPStyleMath(t *testing.T) {
	// 0.4 @ 100000 + 0.6 @ 100010 => cost 100006 avg
	p1, q1 := mustParse(t, "100000"), mustParse(t, "0.4")
	p2, q2 := mustParse(t, "100010"), mustParse(t, "0.6")
	cost := p1.Mul(q1).Add(p2.Mul(q2))
	vwap := cost.Div(q1.Add(q2))
	if vwap.String() != "100006" {
		t.Errorf("vwap: %s", vwap)
	}
}

func FuzzParse(f *testing.F) {
	f.Add("123.456")
	f.Add("-0.00000001")
	f.Add("99999999999")
	f.Fuzz(func(t *testing.T, s string) {
		v, err := Parse(s)
		if err != nil {
			return
		}
		if _, err := Parse(v.String()); err != nil {
			t.Fatalf("reparse of %q failed: %v", v.String(), err)
		}
	})
}
