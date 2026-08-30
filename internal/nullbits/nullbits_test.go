package nullbits

import "testing"

func TestSizeRoundsUp(t *testing.T) {
	cases := map[int]int{0: 0, 1: 1, 7: 1, 8: 1, 9: 2, 16: 2, 17: 3, 64: 8}
	for fields, want := range cases {
		if got := Size(fields); got != want {
			t.Fatalf("Size(%d) = %d, want %d", fields, got, want)
		}
	}
}

func TestNewIsAllPresent(t *testing.T) {
	b := New(8)
	for i := 0; i < 8; i++ {
		if b.IsNull(i) {
			t.Fatalf("field %d is null in a fresh bitmap", i)
		}
	}
	if b.Any(16) {
		t.Fatal("fresh bitmap reports a null")
	}
	if b.Count(16) != 0 {
		t.Fatalf("fresh bitmap counts %d nulls", b.Count(16))
	}
}

func TestSetAndClear(t *testing.T) {
	b := New(16)
	for _, i := range []int{0, 3, 8, 15} {
		b.SetNull(i)
	}
	for i := 0; i < 16; i++ {
		want := i == 0 || i == 3 || i == 8 || i == 15
		if b.IsNull(i) != want {
			t.Fatalf("field %d: IsNull = %v, want %v (bitmap %s)", i, b.IsNull(i), want, b)
		}
	}
	if b.Count(16) != 4 {
		t.Fatalf("Count = %d, want 4", b.Count(16))
	}
	b.SetPresent(3)
	if b.IsNull(3) {
		t.Fatal("field 3 still null after SetPresent")
	}
	if b.Count(16) != 3 {
		t.Fatalf("Count = %d, want 3", b.Count(16))
	}
}

func TestBitLayoutIsStable(t *testing.T) {
	b := New(8)
	b.SetNull(0)
	if b[0] != 0x01 {
		t.Fatalf("field 0 set byte to %02X, want 01", b[0])
	}
	b = New(8)
	b.SetNull(7)
	if b[0] != 0x80 {
		t.Fatalf("field 7 set byte to %02X, want 80", b[0])
	}
	b = New(16)
	b.SetNull(8)
	if b[0] != 0x00 || b[1] != 0x01 {
		t.Fatalf("field 8 gave % 02X, want 00 01", []byte(b))
	}
}

func TestOutOfRangeIsIgnoredNotPanicking(t *testing.T) {
	b := New(8)
	b.SetNull(-1)
	b.SetNull(999)
	b.SetPresent(-1)
	b.SetPresent(999)
	if b.IsNull(-1) || b.IsNull(999) {
		t.Fatal("out-of-range index reported as null")
	}
	if b.Any(16) {
		t.Fatal("out-of-range writes modified the bitmap")
	}
}

func TestBitmapSurvivesRoundTrip(t *testing.T) {
	orig := New(24)
	for _, i := range []int{1, 5, 12, 23} {
		orig.SetNull(i)
	}
	wire := append([]byte(nil), orig...)
	back := Bitmap(wire)
	for i := 0; i < 24; i++ {
		if back.IsNull(i) != orig.IsNull(i) {
			t.Fatalf("field %d differs after round trip", i)
		}
	}
}

func TestDescribeMarksPaddingBits(t *testing.T) {
	b := New(5)
	b.SetNull(2)
	b.SetNull(3)
	if got, want := b.Describe(5), "..NN.---"; got != want {
		t.Fatalf("Describe(5) = %q, want %q", got, want)
	}
	if got, want := b.Describe(8), "..NN...."; got != want {
		t.Fatalf("Describe(8) = %q, want %q", got, want)
	}
}
