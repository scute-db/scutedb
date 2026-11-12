package disktree

import (
	"math/rand"
	"path/filepath"
	"testing"

	"github.com/scute-db/scutedb/internal/bufpool"
	"github.com/scute-db/scutedb/internal/core"
	"github.com/scute-db/scutedb/internal/pagestore"
)

func TestATreeSurvivesFlushCheckpointCloseAndReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tree.db")
	store, err := pagestore.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	pool := newPool(t, store, 8, bufpool.LRUK, bufpool.WriteBack)
	tr, root, err := Create(pool)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetRoot(root); err != nil {
		t.Fatal(err)
	}

	r := rand.New(rand.NewSource(5))
	model := map[string]core.RowID{}
	rounds := []struct {
		pol  bufpool.Policy
		mode bufpool.Mode
		puts int
		dels int
	}{
		{bufpool.LRUK, bufpool.WriteBack, 8000, 1000},
		{bufpool.LRU, bufpool.WriteThrough, 3000, 4000},
		{bufpool.Clock, bufpool.WriteBack, 5000, 500},
	}
	for round, c := range rounds {
		if round > 0 {
			store, err = pagestore.Open(path)
			if err != nil {
				t.Fatalf("round %d: reopen: %v", round, err)
			}
			if store.Root() != root {
				t.Fatalf("round %d: the store's root is %d, the tree was created at %d", round, store.Root(), root)
			}
			pool = newPool(t, store, 8, c.pol, c.mode)
			if tr, err = Open(pool, store.Root()); err != nil {
				t.Fatalf("round %d: open tree: %v", round, err)
			}
			checkModel(t, tr, model)
			mustValidate(t, tr)
		}
		for i := 0; i < c.puts; i++ {
			k := make([]byte, 1+r.Intn(60))
			r.Read(k)
			rid := core.RowID{Page: core.PageID(r.Uint32()), Slot: uint16(i)}
			mustPut(t, tr, k, rid)
			model[string(k)] = rid
		}
		deleted := 0
		for k := range model {
			if deleted == c.dels {
				break
			}
			if ok, err := tr.Delete([]byte(k)); !ok || err != nil {
				t.Fatalf("Delete: %v %v", ok, err)
			}
			delete(model, k)
			deleted++
		}
		mustValidate(t, tr)
		if err := pool.FlushAll(); err != nil {
			t.Fatal(err)
		}
		if err := store.Checkpoint(); err != nil {
			t.Fatal(err)
		}
		if err := store.Verify(); err != nil {
			t.Fatal(err)
		}
		if err := store.Close(); err != nil {
			t.Fatal(err)
		}
	}

	store, err = pagestore.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	tr, err = Open(newPool(t, store, 4, bufpool.LRU, bufpool.WriteBack), store.Root())
	if err != nil {
		t.Fatal(err)
	}
	checkModel(t, tr, model)
	s := mustValidate(t, tr)
	if s.Keys != len(model) || tr.Root() != root {
		t.Fatalf("final tree: %d keys (model %d), root %d (created %d)", s.Keys, len(model), tr.Root(), root)
	}
}
