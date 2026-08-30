package codec

import (
	"bytes"
	"encoding/binary"
	"math"
	"math/rand"
	"sort"
	"testing"
)

func TestVarintMatchesStdlib(t *testing.T) {
	values := []uint64{0, 1, 127, 128, 300, 16383, 16384, 1 << 32, math.MaxUint64}
	for _, v := range values {
		ours := AppendUvarint(nil, v)
		theirs := binary.AppendUvarint(nil, v)
		if !bytes.Equal(ours, theirs) {
			t.Fatalf("uvarint(%d): ours % 02X, stdlib % 02X", v, ours, theirs)
		}
		if got := UvarintLen(v); got != len(ours) {
			t.Fatalf("UvarintLen(%d) = %d, encoded %d bytes", v, got, len(ours))
		}
	}
	for _, v := range []int64{0, 1, -1, 63, -64, math.MaxInt64, math.MinInt64} {
		ours := AppendVarint(nil, v)
		theirs := binary.AppendVarint(nil, v)
		if !bytes.Equal(ours, theirs) {
			t.Fatalf("varint(%d): ours % 02X, stdlib % 02X", v, ours, theirs)
		}
	}
}

func TestValueRoundTrip(t *testing.T) {
	buf := []byte{}
	buf = AppendUint64(buf, 4096)
	buf = AppendInt64(buf, -42)
	buf = AppendBool(buf, true)
	buf = AppendFloat64(buf, 3.5)
	buf = AppendString(buf, "scute-db")
	buf = AppendBytes(buf, []byte{0xDE, 0xAD})

	off := 0
	u, n, err := Uint64(buf[off:])
	if err != nil || u != 4096 {
		t.Fatalf("uint64 = %d, %v", u, err)
	}
	off += n
	i, n, err := Int64(buf[off:])
	if err != nil || i != -42 {
		t.Fatalf("int64 = %d, %v", i, err)
	}
	off += n
	b, n, err := Bool(buf[off:])
	if err != nil || !b {
		t.Fatalf("bool = %v, %v", b, err)
	}
	off += n
	f, n, err := Float64(buf[off:])
	if err != nil || f != 3.5 {
		t.Fatalf("float64 = %v, %v", f, err)
	}
	off += n
	s, n, err := String(buf[off:])
	if err != nil || s != "scute-db" {
		t.Fatalf("string = %q, %v", s, err)
	}
	off += n
	raw, n, err := Bytes(buf[off:])
	if err != nil || !bytes.Equal(raw, []byte{0xDE, 0xAD}) {
		t.Fatalf("bytes = % 02X, %v", raw, err)
	}
	off += n
	if off != len(buf) {
		t.Fatalf("consumed %d of %d bytes", off, len(buf))
	}
}

func TestTruncatedInputIsAnError(t *testing.T) {
	full := AppendString(nil, "hello")
	for cut := 0; cut < len(full); cut++ {
		if _, _, err := String(full[:cut]); err == nil {
			t.Fatalf("String(%d of %d bytes) returned no error", cut, len(full))
		}
	}
	if _, _, err := Uvarint([]byte{0x80, 0x80, 0x80}); err != ErrTruncated {
		t.Fatalf("unterminated varint gave %v, want ErrTruncated", err)
	}
	overflow := []byte{0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x02}
	if _, _, err := Uvarint(overflow); err != ErrOverflow {
		t.Fatalf("11-byte varint gave %v, want ErrOverflow", err)
	}
}

func TestKeyOrderingUint64(t *testing.T) {
	r := rand.New(rand.NewSource(1))
	nums := make([]uint64, 500)
	for i := range nums {
		nums[i] = r.Uint64()
	}
	sort.Slice(nums, func(i, j int) bool { return nums[i] < nums[j] })
	for i := 1; i < len(nums); i++ {
		a := AppendKeyUint64(nil, nums[i-1])
		b := AppendKeyUint64(nil, nums[i])
		if bytes.Compare(a, b) > 0 {
			t.Fatalf("%d encodes above %d", nums[i-1], nums[i])
		}
	}
}

func TestKeyOrderingInt64(t *testing.T) {
	nums := []int64{math.MinInt64, -1 << 40, -300, -2, -1, 0, 1, 2, 300, 1 << 40, math.MaxInt64}
	for i := 1; i < len(nums); i++ {
		a := AppendKeyInt64(nil, nums[i-1])
		b := AppendKeyInt64(nil, nums[i])
		if bytes.Compare(a, b) >= 0 {
			t.Fatalf("%d encodes >= %d\n  %d -> % 02X\n  %d -> % 02X",
				nums[i-1], nums[i], nums[i-1], a, nums[i], b)
		}
	}
}

func TestKeyOrderingFloat64(t *testing.T) {
	nums := []float64{math.Inf(-1), -1e10, -1.5, -0.0, 0.0, 1.5, 1e10, math.Inf(1)}
	for i := 1; i < len(nums); i++ {
		a := AppendKeyFloat64(nil, nums[i-1])
		b := AppendKeyFloat64(nil, nums[i])
		if bytes.Compare(a, b) > 0 {
			t.Fatalf("%v encodes above %v", nums[i-1], nums[i])
		}
	}
}

func TestKeyRoundTrip(t *testing.T) {
	for _, v := range []int64{math.MinInt64, -1, 0, 1, math.MaxInt64} {
		got, _, err := KeyInt64(AppendKeyInt64(nil, v))
		if err != nil || got != v {
			t.Fatalf("int64 key %d round-tripped to %d (%v)", v, got, err)
		}
	}
	for _, v := range []float64{math.Inf(-1), -1.5, -0.0, 0.0, 1.5, math.Inf(1), math.NaN()} {
		got, _, err := KeyFloat64(AppendKeyFloat64(nil, v))
		if err != nil || math.Float64bits(got) != math.Float64bits(v) {
			t.Fatalf("float64 key %v round-tripped to %v (%v)", v, got, err)
		}
	}
}

func TestVarintIsNotOrderPreserving(t *testing.T) {
	a := AppendUvarint(nil, 255)
	b := AppendUvarint(nil, 256)
	if bytes.Compare(a, b) <= 0 {
		t.Fatal("varint preserved order for 255 vs 256; the test intends to show it does not")
	}
	t.Logf("255 -> % 02X", a)
	t.Logf("256 -> % 02X", b)
	t.Logf("bytewise, 255 sorts ABOVE 256, because a varint puts the LOW bits first")

	big := AppendKeyUint64(nil, 255)
	bigger := AppendKeyUint64(nil, 256)
	if bytes.Compare(big, bigger) >= 0 {
		t.Fatal("big-endian key encoding failed to preserve order")
	}
	t.Logf("as keys: 255 -> % 02X", big)
	t.Logf("         256 -> % 02X   correct order", bigger)
}

func TestLittleEndianBreaksKeyOrdering(t *testing.T) {
	nums := []uint64{1, 2, 256, 300}
	le := make([][]byte, len(nums))
	for i, n := range nums {
		le[i] = binary.LittleEndian.AppendUint64(nil, n)
	}
	sorted := append([][]byte(nil), le...)
	sort.Slice(sorted, func(i, j int) bool { return bytes.Compare(sorted[i], sorted[j]) < 0 })
	if bytes.Equal(sorted[0], le[0]) {
		t.Fatal("little-endian preserved order; expected it not to")
	}
	t.Logf("input order:  %v", nums)
	t.Logf("little-endian byte order puts %d first", binary.LittleEndian.Uint64(sorted[0]))
}
