package nodepage

import (
	"bytes"
	"errors"
	"sort"
	"testing"

	"github.com/scute-db/scutedb/internal/core"
	"github.com/scute-db/scutedb/internal/page"
)

func FuzzNodePageSurvivesArbitraryKeys(f *testing.F) {
	f.Add([]byte("abcdefghijklmnop"), uint8(4), true)
	f.Add([]byte{0, 0, 0, 1, 0, 0, 0, 2}, uint8(4), false)
	f.Add([]byte{0xFF}, uint8(1), true)

	f.Fuzz(func(t *testing.T, raw []byte, width uint8, leaf bool) {
		w := int(width)%40 + 1
		if len(raw) < w {
			t.Skip()
		}

		var keys [][]byte
		for i := 0; i+w <= len(raw); i += w {
			keys = append(keys, raw[i:i+w])
		}
		sort.Slice(keys, func(a, b int) bool { return bytes.Compare(keys[a], keys[b]) < 0 })
		uniq := keys[:0]
		for i, k := range keys {
			if i == 0 || !bytes.Equal(k, keys[i-1]) {
				uniq = append(uniq, k)
			}
		}
		keys = uniq
		if len(keys) == 0 {
			t.Skip()
		}

		var n Node
		var err error
		if leaf {
			n = NewLeaf(1)
		} else {
			n, err = NewInternal(1, 1)
			if err != nil {
				t.Fatal(err)
			}
			n.SetFirstChild(1000)
		}

		written := 0
		for i, k := range keys {
			if leaf {
				err = n.AppendLeaf(k, core.RowID{Page: core.PageID(i), Slot: uint16(i)})
			} else {
				err = n.AppendInternal(k, core.PageID(2000+i))
			}
			if err != nil {
				if !errors.Is(err, ErrNoFit) {
					t.Fatalf("append %d failed with %v, want ErrNoFit", i, err)
				}
				break
			}
			written++
		}

		if err := n.Validate(); err != nil {
			t.Fatalf("Validate after %d appends: %v", written, err)
		}
		if n.KeyCount() != written {
			t.Fatalf("key count %d, appended %d", n.KeyCount(), written)
		}

		max := MaxKeys(n.Kind(), w)
		if written > max {
			t.Fatalf("fitted %d keys of %d bytes, MaxKeys says at most %d", written, w, max)
		}

		for i := 0; i < written; i++ {
			if !bytes.Equal(n.KeyRef(i), keys[i]) {
				t.Fatalf("key %d is % 02X, want % 02X", i, n.KeyRef(i), keys[i])
			}
			at, found := n.Search(keys[i])
			if !found || at != i {
				t.Fatalf("Search for key %d returned (%d, %v)", i, at, found)
			}
			if leaf {
				if want := (core.RowID{Page: core.PageID(i), Slot: uint16(i)}); n.Row(i) != want {
					t.Fatalf("row %d is %+v, want %+v", i, n.Row(i), want)
				}
			} else if want := core.PageID(2000 + i); n.Child(i+1) != want {
				t.Fatalf("child %d is %d, want %d", i+1, n.Child(i+1), want)
			}
		}

		if !leaf && written > 0 {
			if n.Child(0) != 1000 {
				t.Fatalf("first child is %d, want 1000", n.Child(0))
			}
			if got := n.ChildFor(keys[0]); got != 2000 {
				t.Fatalf("ChildFor(first key) = %d, want 2000", got)
			}
		}
	})
}

func BenchmarkSearchFullPage(b *testing.B) {
	n := NewLeaf(1)
	i := 0
	for n.AppendLeaf(key(i), rid(i)) == nil {
		i++
	}
	b.ReportMetric(float64(n.KeyCount()), "keys/page")
	probe := key(i / 2)
	b.ResetTimer()
	for j := 0; j < b.N; j++ {
		if _, found := n.Search(probe); !found {
			b.Fatal("probe key vanished")
		}
	}
}

func BenchmarkDecodeAndValidate(b *testing.B) {
	n := NewLeaf(1)
	i := 0
	for n.AppendLeaf(key(i), rid(i)) == nil {
		i++
	}
	raw := page.Page(append([]byte(nil), n.Page...))
	b.ResetTimer()
	for j := 0; j < b.N; j++ {
		loaded, err := Load(raw)
		if err != nil {
			b.Fatal(err)
		}
		if err := loaded.Validate(); err != nil {
			b.Fatal(err)
		}
	}
}
