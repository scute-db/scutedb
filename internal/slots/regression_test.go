package slots

import (
	"errors"
	"testing"

	"github.com/scute-db/scutedb/internal/page"
)

func TestNegativeOrZeroRecordSizeIsRejected(t *testing.T) {
	for _, size := range []int{-5, -1, 0, page.Size + 1} {
		if _, err := NewLayout(size, 8); !errors.Is(err, ErrNoFit) {
			t.Fatalf("NewLayout(%d, 8) gave %v, want ErrNoFit", size, err)
		}
	}
}

func TestAlignIgnoresNonPositiveInput(t *testing.T) {
	for _, n := range []int{-100, -5, 0} {
		if got := Align(n, 8); got != n {
			t.Fatalf("Align(%d, 8) = %d, want %d unchanged", n, got, n)
		}
		if got := Padding(n, 8); got != 0 {
			t.Fatalf("Padding(%d, 8) = %d, want 0", n, got)
		}
	}
}

func TestZeroLayoutErrorsRatherThanPanicking(t *testing.T) {
	var l Layout
	p := page.New(1, page.KindHeap)
	if err := l.Put(p, 0, []byte{1}); !errors.Is(err, ErrZeroLayout) {
		t.Fatalf("Put on a zero Layout gave %v, want ErrZeroLayout", err)
	}
	if _, err := l.Get(p, 0); !errors.Is(err, ErrZeroLayout) {
		t.Fatalf("Get on a zero Layout gave %v, want ErrZeroLayout", err)
	}
	if got := l.String(); got != "invalid layout" {
		t.Fatalf("String on a zero Layout = %q", got)
	}
}

func TestShortPageErrorsRatherThanPanicking(t *testing.T) {
	l, err := NewLayout(16, 8)
	if err != nil {
		t.Fatal(err)
	}
	short := make(page.Page, 100)
	if err := l.Put(short, 50, make([]byte, 16)); !errors.Is(err, ErrNoFit) {
		t.Fatalf("Put past a truncated page gave %v, want ErrNoFit", err)
	}
	if _, err := l.Get(short, 50); !errors.Is(err, ErrNoFit) {
		t.Fatalf("Get past a truncated page gave %v, want ErrNoFit", err)
	}
	if err := l.Put(short, 0, make([]byte, 16)); err != nil {
		t.Fatalf("slot 0 fits in 100 bytes but gave %v", err)
	}
}

func TestLastSlotEndsInsideThePage(t *testing.T) {
	for size := 1; size <= 512; size++ {
		l, err := NewLayout(size, 8)
		if err != nil {
			t.Fatalf("NewLayout(%d, 8): %v", size, err)
		}
		last := l.Offset(l.Capacity()-1) + l.SlotSize()
		if last > page.Size {
			t.Fatalf("record %d: last slot ends at %d, past a %d-byte page",
				size, last, page.Size)
		}
		if l.Tail() < 0 {
			t.Fatalf("record %d: negative tail %d", size, l.Tail())
		}
		if l.Waste() < 0 {
			t.Fatalf("record %d: negative waste %d", size, l.Waste())
		}
	}
}
