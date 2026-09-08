package btree

import (
	"bytes"
	"fmt"
	"testing"

	"github.com/scute-db/scutedb/internal/core"
)

func visitsChained(tr *Tree, from, to []byte) (keys, visits int) {
	n := tr.root
	visits = 1
	for !n.leaf {
		n = n.children[n.childIndex(from)]
		visits++
	}
	i := n.leafIndex(from)
	for n != nil {
		for ; i < len(n.keys); i++ {
			if to != nil && bytes.Compare(n.keys[i], to) >= 0 {
				return keys, visits
			}
			keys++
		}
		n = n.next
		i = 0
		if n != nil {
			visits++
		}
	}
	return keys, visits
}

func visitsByRepeatedGet(tr *Tree, lo, hi int) (keys, visits int) {
	h := tr.Height()
	for i := lo; i < hi; i++ {
		if _, ok := tr.Get(key(i)); ok {
			keys++
		}
		visits += h
	}
	return keys, visits
}

func TestNodeVisitsForARangeScan(t *testing.T) {
	tr := mustNew(t, 64)
	for i := 0; i < 100000; i++ {
		tr.Put(key(i), rid(i))
	}
	fmt.Printf("100,000 keys at order 64, height %d\n\n", tr.Height())
	fmt.Printf("%-10s %-16s %-18s %s\n", "range", "Scan (chained)", "Get in a loop", "ratio")
	fmt.Println("------------------------------------------------------------")
	for _, n := range []int{10, 100, 1000, 10000} {
		k1, v1 := visitsChained(tr, key(0), key(n))
		k2, v2 := visitsByRepeatedGet(tr, 0, n)
		if k1 != n || k2 != n {
			t.Fatalf("range %d: chained saw %d, repeated Get saw %d", n, k1, k2)
		}
		fmt.Printf("%-10d %-16d %-18d %.0fx\n", n, v1, v2, float64(v2)/float64(v1))
	}
	fmt.Println("\nScan descends once and then walks sideways.")
	fmt.Println("Get in a loop pays a full descent for every single key.")
	fmt.Println("Once the tree is on disk, a node visit is a page read.")
}

func BenchmarkScanStreaming(b *testing.B) {
	tr := buildTree(64)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		n := 0
		for it := tr.ScanAll(); it.Next(); {
			n++
		}
	}
}

func BenchmarkScanMaterialised(b *testing.B) {
	tr := buildTree(64)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		type row struct {
			k []byte
			r core.RowID
		}
		var all []row
		for it := tr.ScanAll(); it.Next(); {
			all = append(all, row{append([]byte(nil), it.Key()...), it.RowID()})
		}
		_ = len(all)
	}
}

func BenchmarkScanFirstTenOfAHundredThousand(b *testing.B) {
	tr := buildTree(64)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		n := 0
		for it := tr.Scan(key(0), key(10)); it.Next(); {
			n++
		}
	}
}
