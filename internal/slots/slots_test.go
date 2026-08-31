package slots

import (
	"bytes"
	"errors"
	"testing"

	"github.com/scute-db/scutedb/internal/page"
)

func TestAlign(t *testing.T) {
	cases := []struct{ n, to, want int }{
		{0, 8, 0}, {1, 8, 8}, {7, 8, 8}, {8, 8, 8}, {9, 8, 16},
		{13, 8, 16}, {16, 16, 16}, {17, 16, 32}, {5, 1, 5},
	}
	for _, c := range cases {
		if got := Align(c.n, c.to); got != c.want {
			t.Fatalf("Align(%d, %d) = %d, want %d", c.n, c.to, got, c.want)
		}
	}
	if got := Padding(13, 8); got != 3 {
		t.Fatalf("Padding(13, 8) = %d, want 3", got)
	}
}

func TestLayoutArithmetic(t *testing.T) {
	l, err := NewLayout(13, 8)
	if err != nil {
		t.Fatal(err)
	}
	if l.SlotSize() != 16 {
		t.Fatalf("SlotSize = %d, want 16", l.SlotSize())
	}
	if l.PadPerSlot() != 3 {
		t.Fatalf("PadPerSlot = %d, want 3", l.PadPerSlot())
	}
	if l.Base() != page.HeaderSize {
		t.Fatalf("Base = %d, want %d", l.Base(), page.HeaderSize)
	}
	wantCap := (page.Size - page.HeaderSize) / 16
	if l.Capacity() != wantCap {
		t.Fatalf("Capacity = %d, want %d", l.Capacity(), wantCap)
	}
	if l.Waste() != l.PadPerSlot()*l.Capacity()+l.Tail() {
		t.Fatal("Waste does not equal padding plus tail")
	}
}

func TestEverySlotIsAligned(t *testing.T) {
	for _, size := range []int{1, 5, 13, 16, 31, 64, 100} {
		l, err := NewLayout(size, 8)
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i < l.Capacity(); i++ {
			if l.Offset(i)%8 != 0 {
				t.Fatalf("record %d, slot %d starts at %d which is not 8-aligned",
					size, i, l.Offset(i))
			}
		}
		last := l.Offset(l.Capacity()-1) + l.SlotSize()
		if last > page.Size {
			t.Fatalf("record %d: last slot ends at %d, past the page", size, last)
		}
	}
}

func TestOffsetIsConstantTime(t *testing.T) {
	l, _ := NewLayout(16, 8)
	for _, i := range []int{0, 1, 100, 200} {
		want := l.Base() + i*l.SlotSize()
		if got := l.Offset(i); got != want {
			t.Fatalf("Offset(%d) = %d, want %d", i, got, want)
		}
	}
}

func TestPutGetRoundTrip(t *testing.T) {
	l, _ := NewLayout(8, 8)
	p := page.New(1, page.KindHeap)
	for i := 0; i < 10; i++ {
		rec := []byte{byte(i), 'a', 'b', 'c', 'd', 'e', 'f', 'g'}
		if err := l.Put(p, i, rec); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 10; i++ {
		got, err := l.Get(p, i)
		if err != nil {
			t.Fatal(err)
		}
		if got[0] != byte(i) {
			t.Fatalf("slot %d holds %d", i, got[0])
		}
	}
	if p.ItemCount() != 10 {
		t.Fatalf("ItemCount = %d, want 10", p.ItemCount())
	}
}

func TestPutZeroesThePadding(t *testing.T) {
	l, _ := NewLayout(5, 8)
	p := page.New(1, page.KindHeap)
	for i := l.Offset(0); i < l.Offset(1); i++ {
		p[i] = 0xFF
	}
	if err := l.Put(p, 0, []byte("hello")); err != nil {
		t.Fatal(err)
	}
	pad := p[l.Offset(0)+5 : l.Offset(1)]
	if !bytes.Equal(pad, make([]byte, len(pad))) {
		t.Fatalf("padding left dirty: % 02X", pad)
	}
}

func TestGetDoesNotAliasButGetRefDoes(t *testing.T) {
	l, _ := NewLayout(4, 8)
	p := page.New(1, page.KindHeap)
	l.Put(p, 0, []byte{1, 2, 3, 4})

	cp, _ := l.Get(p, 0)
	ref, _ := l.GetRef(p, 0)
	p[l.Offset(0)] = 99

	if cp[0] != 1 {
		t.Fatal("Get returned an aliasing slice")
	}
	if ref[0] != 99 {
		t.Fatal("GetRef copied; it is documented as a view")
	}
}

func TestErrors(t *testing.T) {
	if _, err := NewLayout(16, 3); !errors.Is(err, ErrBadAlign) {
		t.Fatalf("align 3 gave %v, want ErrBadAlign", err)
	}
	if _, err := NewLayout(page.Size, 8); !errors.Is(err, ErrNoFit) {
		t.Fatalf("oversized record gave %v, want ErrNoFit", err)
	}
	l, _ := NewLayout(8, 8)
	p := page.New(1, page.KindHeap)
	if err := l.Put(p, -1, make([]byte, 8)); !errors.Is(err, ErrOutOfRange) {
		t.Fatalf("negative slot gave %v", err)
	}
	if err := l.Put(p, l.Capacity(), make([]byte, 8)); !errors.Is(err, ErrOutOfRange) {
		t.Fatalf("slot past capacity gave %v", err)
	}
	if err := l.Put(p, 0, make([]byte, 7)); !errors.Is(err, ErrRecordSize) {
		t.Fatalf("wrong-sized record gave %v", err)
	}
}
