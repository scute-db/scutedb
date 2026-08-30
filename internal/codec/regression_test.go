package codec

import (
	"bytes"
	"math"
	"testing"
)

func TestNegativeZeroEncodesLikePositiveZero(t *testing.T) {
	pos := AppendKeyFloat64(nil, 0.0)
	neg := AppendKeyFloat64(nil, math.Copysign(0, -1))
	if !bytes.Equal(pos, neg) {
		t.Fatalf("-0.0 encodes to % 02X but +0.0 encodes to % 02X; they are numerically equal", neg, pos)
	}
}

func TestAllNaNsEncodeIdentically(t *testing.T) {
	a := AppendKeyFloat64(nil, math.NaN())
	b := AppendKeyFloat64(nil, math.Float64frombits(0x7FF8000000000042))
	if !bytes.Equal(a, b) {
		t.Fatalf("two NaN payloads encode differently: % 02X vs % 02X", a, b)
	}
}

func TestNaNSortsAboveEverything(t *testing.T) {
	nan := AppendKeyFloat64(nil, math.NaN())
	for _, v := range []float64{math.Inf(-1), -1e300, 0, 1e300, math.Inf(1)} {
		if bytes.Compare(nan, AppendKeyFloat64(nil, v)) <= 0 {
			t.Fatalf("NaN does not sort above %v", v)
		}
	}
}

func TestBytesDoesNotAliasSource(t *testing.T) {
	buf := AppendBytes(nil, []byte{1, 2, 3})
	out, _, err := Bytes(buf)
	if err != nil {
		t.Fatal(err)
	}
	buf[1] = 99
	if out[0] != 1 {
		t.Fatal("Bytes() returned a slice aliasing the caller's buffer")
	}
}

func TestBytesRefDoesAliasSource(t *testing.T) {
	buf := AppendBytes(nil, []byte{1, 2, 3})
	out, _, err := BytesRef(buf)
	if err != nil {
		t.Fatal(err)
	}
	buf[1] = 99
	if out[0] != 99 {
		t.Fatal("BytesRef() copied; it is documented as returning a view")
	}
}

func TestStringDoesNotAliasSource(t *testing.T) {
	buf := AppendString(nil, "abc")
	out, _, err := String(buf)
	if err != nil {
		t.Fatal(err)
	}
	buf[1] = 'z'
	if out != "abc" {
		t.Fatalf("String() = %q after mutating the source; it must copy", out)
	}
}
