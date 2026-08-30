package codec

import (
	"bytes"
	"math"
	"testing"
)

func FuzzValueRoundTrip(f *testing.F) {
	f.Add(uint64(0), int64(0), false, 0.0, "")
	f.Add(uint64(4096), int64(-42), true, 3.5, "scute-db")
	f.Add(uint64(math.MaxUint64), int64(math.MinInt64), true, math.Inf(-1), "\x00\xff mixed")

	f.Fuzz(func(t *testing.T, u uint64, i int64, b bool, fl float64, s string) {
		buf := AppendUint64(nil, u)
		buf = AppendInt64(buf, i)
		buf = AppendBool(buf, b)
		buf = AppendFloat64(buf, fl)
		buf = AppendString(buf, s)

		off := 0
		gu, n, err := Uint64(buf[off:])
		if err != nil || gu != u {
			t.Fatalf("uint64 %d -> %d (%v)", u, gu, err)
		}
		off += n
		gi, n, err := Int64(buf[off:])
		if err != nil || gi != i {
			t.Fatalf("int64 %d -> %d (%v)", i, gi, err)
		}
		off += n
		gb, n, err := Bool(buf[off:])
		if err != nil || gb != b {
			t.Fatalf("bool %v -> %v (%v)", b, gb, err)
		}
		off += n
		gf, n, err := Float64(buf[off:])
		if err != nil || math.Float64bits(gf) != math.Float64bits(fl) {
			t.Fatalf("float64 %v -> %v (%v)", fl, gf, err)
		}
		off += n
		gs, n, err := String(buf[off:])
		if err != nil || gs != s {
			t.Fatalf("string %q -> %q (%v)", s, gs, err)
		}
		off += n
		if off != len(buf) {
			t.Fatalf("consumed %d of %d bytes", off, len(buf))
		}
	})
}

func FuzzKeyOrderingInt64(f *testing.F) {
	f.Add(int64(0), int64(1))
	f.Add(int64(-1), int64(1))
	f.Add(int64(math.MinInt64), int64(math.MaxInt64))

	f.Fuzz(func(t *testing.T, a, b int64) {
		ea := AppendKeyInt64(nil, a)
		eb := AppendKeyInt64(nil, b)
		cmpNum := 0
		switch {
		case a < b:
			cmpNum = -1
		case a > b:
			cmpNum = 1
		}
		if got := bytes.Compare(ea, eb); got != cmpNum {
			t.Fatalf("%d vs %d: numeric %d, bytewise %d\n  % 02X\n  % 02X",
				a, b, cmpNum, got, ea, eb)
		}
	})
}

func FuzzDecoderNeverPanics(f *testing.F) {
	f.Add([]byte{})
	f.Add([]byte{0xFF})
	f.Add([]byte{0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80})
	f.Add([]byte{0x05, 'h', 'e'})

	f.Fuzz(func(t *testing.T, data []byte) {
		Uvarint(data)
		Varint(data)
		Bool(data)
		Float64(data)
		Bytes(data)
		String(data)
		KeyUint64(data)
		KeyInt64(data)
		KeyFloat64(data)
	})
}
