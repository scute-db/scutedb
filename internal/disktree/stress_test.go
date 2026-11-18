package disktree

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math/rand"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/scute-db/scutedb/internal/bufpool"
	"github.com/scute-db/scutedb/internal/core"
)

func rangeKey(g, i int) []byte {
	k := make([]byte, 12+i%97)
	binary.BigEndian.PutUint32(k, uint32(g))
	binary.BigEndian.PutUint64(k[4:], uint64(i))
	for j := 12; j < len(k); j++ {
		k[j] = byte(i + j)
	}
	return k
}

func TestConcurrentWritersOnDisjointRangesAndReadersRunningValidateEndInTheExactState(t *testing.T) {
	const writers, readers = 8, 4
	keys := 1200
	if testing.Short() {
		keys = 300
	}
	for _, mode := range []bufpool.Mode{bufpool.WriteBack, bufpool.WriteThrough} {
		t.Run(mode.String(), func(t *testing.T) {
			src := bufpool.NewMemSource()
			frames := (writers + readers) * (4 + 3)
			pool := newPool(t, src, frames, bufpool.LRUK, mode)
			tr, root, err := Create(pool)
			if err != nil {
				t.Fatal(err)
			}
			models := make([]map[string]core.RowID, writers)
			var wg sync.WaitGroup
			var validated, scanned atomic.Int64
			stop := make(chan struct{})
			for g := 0; g < writers; g++ {
				models[g] = map[string]core.RowID{}
				wg.Add(1)
				go func(g int) {
					defer wg.Done()
					r := rand.New(rand.NewSource(int64(g)))
					model := models[g]
					for n := 0; n < keys; n++ {
						i := r.Intn(keys)
						k := rangeKey(g, i)
						switch r.Intn(5) {
						case 0:
							ok, err := tr.Delete(k)
							_, want := model[string(k)]
							if err != nil || ok != want {
								t.Errorf("writer %d: Delete = %v %v, model says %v", g, ok, err, want)
								return
							}
							delete(model, string(k))
						case 1:
							rid, ok, err := tr.Get(k)
							want, wantOK := model[string(k)]
							if err != nil || ok != wantOK || rid != want {
								t.Errorf("writer %d: Get = %+v %v %v, model says %+v %v", g, rid, ok, err, want, wantOK)
								return
							}
						default:
							rid := core.RowID{Page: core.PageID(g + 1), Slot: uint16(n)}
							if err := tr.Put(k, rid); err != nil {
								t.Errorf("writer %d: Put: %v", g, err)
								return
							}
							model[string(k)] = rid
						}
					}
				}(g)
			}
			var rwg sync.WaitGroup
			for g := 0; g < readers; g++ {
				rwg.Add(1)
				go func(g int) {
					defer rwg.Done()
					for n := 0; ; n++ {
						select {
						case <-stop:
							return
						default:
						}
						if n%3 == 0 {
							if err := tr.Validate(); err != nil {
								t.Errorf("reader %d: Validate: %v", g, err)
								return
							}
							validated.Add(1)
							continue
						}
						from := rangeKey(n%writers, 0)
						it := tr.Scan(from, rangeKey(n%writers+1, 0))
						var last []byte
						for it.Next() {
							if last != nil && bytes.Compare(last, it.Key()) >= 0 {
								t.Errorf("reader %d: scan went from % 02X to % 02X", g, last, it.Key())
								return
							}
							if !bytes.HasPrefix(it.Key(), from[:4]) {
								t.Errorf("reader %d: scan of range %d returned % 02X", g, n%writers, it.Key())
								return
							}
							last = append(last[:0], it.Key()...)
						}
						if err := it.Err(); err != nil {
							t.Errorf("reader %d: scan: %v", g, err)
							return
						}
						scanned.Add(1)
					}
				}(g)
			}
			wg.Wait()
			close(stop)
			rwg.Wait()
			if t.Failed() {
				return
			}
			all := map[string]core.RowID{}
			for _, m := range models {
				for k, v := range m {
					all[k] = v
				}
			}
			s := mustValidate(t, tr)
			if s.Keys != len(all) {
				t.Fatalf("the tree holds %d keys, the writers' models %d", s.Keys, len(all))
			}
			checkModel(t, tr, all)
			if pool.Stats().Evictions == 0 || validated.Load() == 0 || scanned.Load() == 0 {
				t.Fatalf("evictions %d, validates %d, scans %d: the run did not exercise everything",
					pool.Stats().Evictions, validated.Load(), scanned.Load())
			}
			if err := pool.FlushAll(); err != nil {
				t.Fatal(err)
			}
			again, err := Open(newPool(t, src, 8, bufpool.LRU, bufpool.WriteBack), root)
			if err != nil {
				t.Fatal(err)
			}
			checkModel(t, again, all)
			t.Logf("%s: height %d, %d pages in %d frames, %d evictions, %d validates and %d scans alongside",
				mode, s.Height, s.Pages(), frames, pool.Stats().Evictions, validated.Load(), scanned.Load())
		})
	}
}

func TestConcurrentPutsIntoOneRangeKeepEveryKey(t *testing.T) {
	const goroutines = 8
	per := 400
	if testing.Short() {
		per = 100
	}
	tr, _ := newTree(t, goroutines*(MinFrames(3)+3))
	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < per; i++ {
				k := []byte(fmt.Sprintf("key-%06d", i*goroutines+g))
				if err := tr.Put(k, ridOf(i*goroutines+g)); err != nil {
					t.Error(err)
					return
				}
			}
		}(g)
	}
	wg.Wait()
	model := map[string]core.RowID{}
	for n := 0; n < per*goroutines; n++ {
		model[fmt.Sprintf("key-%06d", n)] = ridOf(n)
	}
	mustValidate(t, tr)
	checkModel(t, tr, model)
}
