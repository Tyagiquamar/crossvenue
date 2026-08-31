// Package decimal provides deterministic fixed-point arithmetic for prices,
// quantities, and money. All values share a single internal scale of 1e8.
// float64 is never used for financial values.
package decimal

import (
	"errors"
	"fmt"
	"math"
	"math/bits"
	"strings"
)

// Scale is the fixed-point multiplier (1e8), matching common crypto
// quantity precision.
const Scale int64 = 100_000_000

const maxDigits = 18 // int64 guard: 9e18 / 1e8 headroom

var (
	ErrOverflow     = errors.New("decimal: overflow")
	ErrInvalidInput = errors.New("decimal: invalid numeric input")
	ErrDivByZero    = errors.New("decimal: division by zero")
)

// Fixed is a base-10 fixed-point number with Scale denominator.
type Fixed int64

// Zero is the additive identity.
const Zero Fixed = 0

// FromInt returns n as a whole-unit Fixed value.
func FromInt(n int64) Fixed { return Fixed(n * Scale) }

// FromRaw wraps an already-scaled integer.
func FromRaw(raw int64) Fixed { return Fixed(raw) }

// Raw returns the scaled integer representation.
func (f Fixed) Raw() int64 { return int64(f) }

// Parse converts a plain decimal string ("123", "0.5", "-12.34567890") to
// Fixed. Exponents and non-numeric input are rejected. More than 8
// fractional digits is an error (no silent rounding).
func Parse(s string) (Fixed, error) {
	if s == "" {
		return 0, ErrInvalidInput
	}
	neg := false
	if s[0] == '-' || s[0] == '+' {
		neg = s[0] == '-'
		s = s[1:]
	}
	if s == "" {
		return 0, ErrInvalidInput
	}
	intPart := s
	fracPart := ""
	if i := strings.IndexByte(s, '.'); i >= 0 {
		intPart = s[:i]
		fracPart = s[i+1:]
		if strings.IndexByte(fracPart, '.') >= 0 {
			return 0, ErrInvalidInput
		}
	}
	if intPart == "" && fracPart == "" {
		return 0, ErrInvalidInput
	}
	if len(fracPart) > 8 {
		return 0, fmt.Errorf("%w: more than 8 fractional digits in %q", ErrInvalidInput, s)
	}
	var whole int64
	for _, c := range intPart {
		if c < '0' || c > '9' {
			return 0, ErrInvalidInput
		}
		d := int64(c - '0')
		if whole > (math.MaxInt64-d)/10 {
			return 0, ErrOverflow
		}
		whole = whole*10 + d
	}
	if len(intPart) > maxDigits {
		return 0, ErrOverflow
	}
	var frac int64
	fracScale := int64(1)
	for _, c := range fracPart {
		if c < '0' || c > '9' {
			return 0, ErrInvalidInput
		}
		frac = frac*10 + int64(c-'0')
		fracScale *= 10
	}
	// Scale fraction up to 1e8.
	for fracScale < Scale {
		frac *= 10
		fracScale *= 10
	}
	if whole > (math.MaxInt64-frac)/Scale {
		return 0, ErrOverflow
	}
	v := Fixed(whole*Scale + frac)
	if neg {
		v = -v
	}
	return v, nil
}

// String renders the value as a plain decimal, trimming trailing zeros.
func (f Fixed) String() string {
	v := int64(f)
	neg := v < 0
	if neg {
		v = -v
	}
	whole := v / Scale
	frac := v % Scale
	if frac == 0 {
		if neg {
			return fmt.Sprintf("-%d", whole)
		}
		return fmt.Sprintf("%d", whole)
	}
	fs := fmt.Sprintf("%08d", frac)
	fs = strings.TrimRight(fs, "0")
	if neg {
		return fmt.Sprintf("-%d.%s", whole, fs)
	}
	return fmt.Sprintf("%d.%s", whole, fs)
}

// Add returns a+b, saturating never; overflow wraps are guarded.
func (f Fixed) Add(o Fixed) Fixed {
	r := int64(f) + int64(o)
	if (int64(o) > 0 && r < int64(f)) || (int64(o) < 0 && r > int64(f)) {
		panic(ErrOverflow)
	}
	return Fixed(r)
}

// Sub returns a-o.
func (f Fixed) Sub(o Fixed) Fixed { return f.Add(-o) }

// Mul returns a*b at Scale precision. Intermediate uses 128-bit via big-free
// checked arithmetic.
func (f Fixed) Mul(o Fixed) Fixed {
	r := mulDiv(int64(f), int64(o), Scale)
	return Fixed(r)
}

// MulInt multiplies by a small integer factor.
func (f Fixed) MulInt(n int64) Fixed {
	if n != 0 && int64(f) != 0 {
		an := n
		if an < 0 {
			an = -an
		}
		af := int64(f)
		if af < 0 {
			af = -af
		}
		if af > math.MaxInt64/an {
			panic(ErrOverflow)
		}
	}
	return Fixed(int64(f) * n)
}

// Div returns a/b rounded toward zero.
func (f Fixed) Div(o Fixed) Fixed {
	if o == 0 {
		panic(ErrDivByZero)
	}
	return Fixed(mulDiv(int64(f), Scale, int64(o)))
}

// Cmp compares two values.
func (f Fixed) Cmp(o Fixed) int {
	switch {
	case f < o:
		return -1
	case f > o:
		return 1
	}
	return 0
}

func (f Fixed) IsZero() bool     { return f == 0 }
func (f Fixed) IsPositive() bool { return f > 0 }
func (f Fixed) IsNegative() bool { return f < 0 }

// Abs returns |f|.
func (f Fixed) Abs() Fixed {
	if f < 0 {
		return -f
	}
	return f
}

// MulBps multiplies f by bps/10000 (basis points), deterministic.
func (f Fixed) MulBps(bps int64) Fixed {
	return Fixed(mulDiv(int64(f), bps, 10_000))
}

// Float64 returns an approximate float for metrics/reporting only.
func (f Fixed) Float64() float64 { return float64(f) / float64(Scale) }

// mulDiv computes a*b/c with 128-bit intermediate via math/bits semantics.
// Panics on overflow.
func mulDiv(a, b, c int64) int64 {
	if c == 0 {
		panic(ErrDivByZero)
	}
	neg := (a < 0) != (b < 0)
	ua := abs64(a)
	ub := abs64(b)
	uc := abs64(c)
	hi, lo := mul64(ua, ub)
	q, _ := div64(hi, lo, uc)
	if q > math.MaxInt64 {
		panic(ErrOverflow)
	}
	r := int64(q)
	if neg {
		r = -r
	}
	return r
}

func abs64(v int64) uint64 {
	if v < 0 {
		return uint64(-v)
	}
	return uint64(v)
}

// mul64 and div64 wrap math/bits for 128-bit intermediates.
func mul64(a, b uint64) (hi, lo uint64) { return bits.Mul64(a, b) }
func div64(hi, lo, y uint64) (q, r uint64) {
	if hi >= y {
		panic(ErrOverflow)
	}
	return bits.Div64(hi, lo, y)
}
