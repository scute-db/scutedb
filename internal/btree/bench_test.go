package btree

import (
	"bytes"
	"testing"

	"github.com/scute-db/scutedb/internal/core"
)

const benchN = 100000

func buildTree(order int) *Tree {
	tr, _ := New(order)
	for i := 0; i < benchN; i++ {
		tr.Put(key(i), rid(i))
	}
	return tr
}

func buildSortedSlice() [][]byte {
	out := make([][]byte, benchN)
	for i := range out {
		out[i] = key(i)
	}
	return out
}

func buildMap() map[uint64]core.RowID {
	m := make(map[uint64]core.RowID, benchN)
	for i := 0; i < benchN; i++ {
		m[uint64(i)] = rid(i)
	}
	return m
}

func BenchmarkBTreeGet(b *testing.B) {
	tr := buildTree(64)
	k := key(benchN - 1)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		tr.Get(k)
	}
}

func BenchmarkLinearScan(b *testing.B) {
	s := buildSortedSlice()
	k := key(benchN - 1)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for _, x := range s {
			if bytes.Equal(x, k) {
				break
			}
		}
	}
}

func BenchmarkHashMapGet(b *testing.B) {
	m := buildMap()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = m[uint64(benchN-1)]
	}
}

func BenchmarkBTreePut(b *testing.B) {
	tr, _ := New(64)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		tr.Put(key(i), rid(i))
	}
}
