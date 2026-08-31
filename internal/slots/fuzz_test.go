package slots

import (
	"testing"

	"github.com/scute-db/scutedb/internal/page"
)

func FuzzPutGetNeverPanics(f *testing.F) {
	f.Add(16, 8, 0, 16)
	f.Add(1, 1, 0, 1)
	f.Add(-5, 8, 0, 4)
	f.Add(4096, 8, 999999, 0)
	f.Add(13, 3, -1, 13)

	f.Fuzz(func(t *testing.T, recordSize, align, slot, recLen int) {
		if recLen < 0 || recLen > page.Size {
			return
		}
		l, err := NewLayout(recordSize, align)
		if err != nil {
			return
		}
		p := page.New(1, page.KindHeap)
		l.Put(p, slot, make([]byte, recLen))
		l.Get(p, slot)
		l.GetRef(p, slot)
		l.Offset(slot)
		l.Waste()
		l.Tail()
		_ = l.String()
	})
}
