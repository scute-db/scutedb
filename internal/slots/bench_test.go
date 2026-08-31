package slots

import (
	"testing"

	"github.com/scute-db/scutedb/internal/codec"
	"github.com/scute-db/scutedb/internal/page"
)

func fixedPage(n int) (Layout, page.Page) {
	l, _ := NewLayout(16, 8)
	p := page.New(1, page.KindHeap)
	rec := make([]byte, 16)
	for i := 0; i < n && i < l.Capacity(); i++ {
		rec[0] = byte(i)
		l.Put(p, i, rec)
	}
	return l, p
}

func packedPage(n int) page.Page {
	p := page.New(1, page.KindHeap)
	for i := 0; i < n; i++ {
		rec := make([]byte, 15)
		rec[0] = byte(i)
		if _, err := p.Append(codec.AppendBytes(nil, rec)); err != nil {
			break
		}
	}
	return p
}

func packedGet(p page.Page, want int) []byte {
	body := p[page.HeaderSize:p.FreeStart()]
	off := 0
	for i := 0; i <= want; i++ {
		rec, n, err := codec.BytesRef(body[off:])
		if err != nil {
			return nil
		}
		if i == want {
			return rec
		}
		off += n
	}
	return nil
}

func BenchmarkFixedSlotFirst(b *testing.B) {
	l, p := fixedPage(200)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		l.GetRef(p, 0)
	}
}

func BenchmarkFixedSlotLast(b *testing.B) {
	l, p := fixedPage(200)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		l.GetRef(p, 199)
	}
}

func BenchmarkPackedFirst(b *testing.B) {
	p := packedPage(200)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		packedGet(p, 0)
	}
}

func BenchmarkPackedLast(b *testing.B) {
	p := packedPage(200)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		packedGet(p, 199)
	}
}
