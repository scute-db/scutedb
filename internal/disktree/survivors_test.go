package disktree

import (
	"bytes"
	"testing"

	"github.com/scute-db/scutedb/internal/bufpool"
	"github.com/scute-db/scutedb/internal/core"
	"github.com/scute-db/scutedb/internal/nodepage"
)

func verifiedNow(t *testing.T, p *bufpool.Pool, id core.PageID) bool {
	t.Helper()
	f, err := p.Fetch(id)
	if err != nil {
		t.Fatal(err)
	}
	v := f.Verified()
	if err := p.Unpin(f, false); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestThePageCreateWritesIsTrustedWithoutCheckingItAgain(t *testing.T) {
	tr, _ := newTree(t, 2)
	if !verifiedNow(t, tr.Pool(), tr.Root()) {
		t.Fatal("the root Create formatted is not marked verified")
	}
}

func TestAPageIsCheckedOnceAfterItIsReadAndThenTrusted(t *testing.T) {
	src := bufpool.NewMemSource()
	tr, root, err := Create(newPool(t, src, 8, bufpool.LRU, bufpool.WriteBack))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 600; i++ {
		mustPut(t, tr, k8(i), ridOf(i))
	}
	if err := tr.Pool().FlushAll(); err != nil {
		t.Fatal(err)
	}
	for _, check := range []string{"Get", "Validate"} {
		again, err := Open(newPool(t, src, 8, bufpool.LRU, bufpool.WriteBack), root)
		if err != nil {
			t.Fatal(err)
		}
		leaf := leafOf(t, again, k8(599))
		if check == "Validate" {
			again, err = Open(newPool(t, src, 8, bufpool.LRU, bufpool.WriteBack), root)
			if err != nil {
				t.Fatal(err)
			}
			mustValidate(t, again)
		}
		if !verifiedNow(t, again.Pool(), leaf) {
			t.Fatalf("after %s read the leaf from the source it is not marked verified", check)
		}
	}
	again, err := Open(newPool(t, src, 8, bufpool.LRU, bufpool.WriteBack), root)
	if err != nil {
		t.Fatal(err)
	}
	leaf := leafOf(t, again, k8(599))
	f, err := again.Pool().Fetch(leaf)
	if err != nil {
		t.Fatal(err)
	}
	n, err := nodepage.Load(f.Page())
	if err != nil {
		t.Fatal(err)
	}
	n.SetFreeEnd(n.FreeEnd() - 1)
	if err := n.Validate(); err == nil {
		t.Fatal("the damaged node still validates")
	}
	if err := again.Pool().Unpin(f, false); err != nil {
		t.Fatal(err)
	}
	if rid, ok, err := again.Get(k8(599)); err != nil || !ok || rid != ridOf(599) {
		t.Fatalf("Get through a page already checked since it was read gave %+v %v %v; a cache hit must not check it again", rid, ok, err)
	}
}

func TestAPutThatFitsTouchesOnlyItsPathAndAllocatesNothing(t *testing.T) {
	tr, src := newTree(t, 8)
	for i := 0; i < 600; i += 2 {
		mustPut(t, tr, k8(i), ridOf(i))
	}
	h, err := tr.Height()
	if err != nil || h != 2 {
		t.Fatalf("height %d %v, want 2", h, err)
	}
	pages := src.Len()
	before := fetches(tr)
	mustPut(t, tr, k8(1), ridOf(1))
	if got := fetches(tr) - before; got != uint64(h+1) {
		t.Fatalf("a Put that fits fetched %d pages, want %d: the path once, then the leaf again to change it", got, h+1)
	}
	if src.Len() != pages {
		t.Fatal("a Put that fits allocated a page")
	}
}

func TestAHalfThatExactlyFillsAPageIsAllowed(t *testing.T) {
	full := nodepage.EntryArea - nodepage.SlotSize - nodepage.LeafSuffix
	if m := chooseSplit(image{leaf: true, entries: entries(full, 10)}); m != 1 {
		t.Fatalf("a left half of exactly %d bytes was refused: split at %d", nodepage.EntryArea, m)
	}
	if m := chooseSplit(image{leaf: true, entries: entries(10, full)}); m != 1 {
		t.Fatalf("a right half of exactly %d bytes was refused: split at %d", nodepage.EntryArea, m)
	}
	if m := chooseSplit(image{leaf: true, entries: entries(full+1, 10)}); m != -1 {
		t.Fatalf("a left half one byte over a page was allowed: split at %d", m)
	}
	if !bytes.Equal(entries(1)[0].key, []byte{'a'}) {
		t.Fatal("entries helper changed")
	}
}
