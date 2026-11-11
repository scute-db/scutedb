package disktree

import (
	"fmt"
	"math/rand"
	"testing"

	"github.com/scute-db/scutedb/internal/bufpool"
	"github.com/scute-db/scutedb/internal/core"
)

type treeDriver struct {
	t     testing.TB
	tr    *Tree
	model map[string]core.RowID
	keys  []string
}

func (d *treeDriver) remember(k string) {
	if _, ok := d.model[k]; !ok {
		d.keys = append(d.keys, k)
	}
}

func (d *treeDriver) pick(r func(int) int, fresh func() []byte) []byte {
	if len(d.keys) > 0 && r(3) != 0 {
		return []byte(d.keys[r(len(d.keys))])
	}
	return fresh()
}

func (d *treeDriver) do(op int, key, bound []byte, rid core.RowID) {
	t, tr := d.t, d.tr
	switch op % 6 {
	case 0, 1:
		if err := tr.Put(key, rid); err != nil {
			t.Fatalf("Put(% 02X): %v", key, err)
		}
		d.remember(string(key))
		d.model[string(key)] = rid
	case 2:
		ok, err := tr.Delete(key)
		_, want := d.model[string(key)]
		if err != nil || ok != want {
			t.Fatalf("Delete(% 02X) = %v %v, model says %v", key, ok, err, want)
		}
		delete(d.model, string(key))
	case 3, 4:
		d.get(key)
	case 5:
		if op >= 12 {
			d.get(key)
			break
		}
		from, to := key, bound
		if op == 5 {
			from = nil
		}
		got := collect(t, tr.Scan(from, to))
		if err := sameKVs(got, expectRange(d.model, from, to)); err != nil {
			t.Fatalf("Scan(% 02X, % 02X): %v", from, to, err)
		}
	}
	noPins(t, tr, fmt.Sprintf("op %d", op%6))
}

func (d *treeDriver) get(key []byte) {
	rid, ok, err := d.tr.Get(key)
	want, wantOK := d.model[string(key)]
	if err != nil || ok != wantOK || (ok && rid != want) {
		d.t.Fatalf("Get(% 02X) = %+v %v %v, model says %+v %v", key, rid, ok, err, want, wantOK)
	}
}

func TestRandomOperationsMatchAMapThroughATinyPool(t *testing.T) {
	steps := 12000
	if testing.Short() {
		steps = 3000
	}
	configs := []struct {
		pol    bufpool.Policy
		mode   bufpool.Mode
		frames int
		maxKey int
	}{
		{bufpool.LRU, bufpool.WriteBack, 8, 40},
		{bufpool.Clock, bufpool.WriteBack, 8, 300},
		{bufpool.LRUK, bufpool.WriteThrough, 8, 1000},
		{bufpool.LRUK, bufpool.WriteBack, 5, 12},
	}
	for ci, c := range configs {
		t.Run(fmt.Sprintf("%s-%s-%dframes-keys%d", c.pol, c.mode, c.frames, c.maxKey), func(t *testing.T) {
			r := rand.New(rand.NewSource(int64(ci) + 99))
			src := bufpool.NewMemSource()
			pool := newPool(t, src, c.frames, c.pol, c.mode)
			tr, root, err := Create(pool)
			if err != nil {
				t.Fatal(err)
			}
			d := &treeDriver{t: t, tr: tr, model: map[string]core.RowID{}}
			fresh := func() []byte {
				n := 1 + r.Intn(c.maxKey)
				if r.Intn(4) == 0 {
					n = 1 + r.Intn(4)
				}
				k := make([]byte, n)
				r.Read(k)
				return k
			}
			for i := 0; i < steps; i++ {
				op := r.Intn(36)
				if i < steps/3 && op%6 == 2 {
					op = 0
				}
				key := d.pick(r.Intn, fresh)
				d.do(op, key, fresh(), core.RowID{Page: core.PageID(r.Uint32()), Slot: uint16(i)})
				if i%2000 == 0 {
					mustValidate(t, tr)
				}
			}
			s := mustValidate(t, tr)
			if s.Keys != len(d.model) {
				t.Fatalf("tree holds %d keys, model holds %d", s.Keys, len(d.model))
			}
			checkModel(t, tr, d.model)
			st := pool.Stats()
			if st.Evictions == 0 {
				t.Fatal("the pool never evicted; the test is not exercising eviction")
			}
			t.Logf("height %d, %d pages, %d keys, %d evictions, hit ratio %.3f", s.Height, s.Pages(), s.Keys, st.Evictions, st.HitRatio())

			if err := pool.FlushAll(); err != nil {
				t.Fatal(err)
			}
			again, err := Open(newPool(t, src, 3, bufpool.Clock, bufpool.WriteBack), root)
			if err != nil {
				t.Fatal(err)
			}
			checkModel(t, again, d.model)
		})
	}
}
