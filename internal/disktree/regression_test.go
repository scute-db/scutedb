package disktree

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"testing"

	"github.com/scute-db/scutedb/internal/bufpool"
	"github.com/scute-db/scutedb/internal/core"
	"github.com/scute-db/scutedb/internal/nodepage"
	"github.com/scute-db/scutedb/internal/page"
)

func rootKeysOnDisk(t *testing.T, src *bufpool.MemSource, tr *Tree) (int, page.Page) {
	t.Helper()
	if err := tr.Pool().FlushAll(); err != nil {
		t.Fatal(err)
	}
	p, err := src.Read(tr.Root())
	if err != nil {
		t.Fatal(err)
	}
	n, err := nodepage.Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if n.Leaf() {
		return 0, p
	}
	return n.KeyCount(), p
}

func putUntilError(tr *Tree, next func() int, limit int) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v", r)
		}
	}()
	for j := 0; j < limit && err == nil; j++ {
		i := next()
		err = tr.Put(k8(i), ridOf(i))
	}
	return err
}

func TestASplitWhoseParentComesBackFromDiskAsAnOlderVersionDoesNotPanic(t *testing.T) {
	src := bufpool.NewMemSource()
	tr, _, err := Create(newPool(t, src, 2, bufpool.LRU, bufpool.WriteBack))
	if err != nil {
		t.Fatal(err)
	}
	i := 0
	var old page.Page
	for ; ; i++ {
		mustPut(t, tr, k8(i), ridOf(i))
		if n, p := rootKeysOnDisk(t, src, tr); n == 1 {
			old = p
			break
		}
	}
	for {
		i++
		mustPut(t, tr, k8(i), ridOf(i))
		if n, _ := rootKeysOnDisk(t, src, tr); n >= 3 {
			break
		}
	}
	if _, _, err := tr.Get(k8(0)); err != nil {
		t.Fatal(err)
	}
	if err := src.Write(old); err != nil {
		t.Fatal(err)
	}
	putErr := putUntilError(tr, func() int { i++; return i }, 400)
	if putErr == nil {
		t.Fatal("no split ever read the older root back")
	}
	if !errors.Is(putErr, ErrCorrupt) {
		t.Fatalf("a split under a root that went back to an older version on disk gave %v, want ErrCorrupt", putErr)
	}
	noPins(t, tr, "a split whose parent changed on disk")
}

func rewriteRoot(t *testing.T, src *bufpool.MemSource, tr *Tree, edit func(img *image)) {
	t.Helper()
	if err := tr.Pool().FlushAll(); err != nil {
		t.Fatal(err)
	}
	p, err := src.Read(tr.Root())
	if err != nil {
		t.Fatal(err)
	}
	n, err := nodepage.Load(p)
	if err != nil {
		t.Fatal(err)
	}
	img := readImage(n)
	edit(&img)
	out, err := img.render()
	if err != nil {
		t.Fatal(err)
	}
	if err := src.Write(out); err != nil {
		t.Fatal(err)
	}
}

func TestASplitRefusesAParentThatNoLongerMatchesThePathItCameDown(t *testing.T) {
	cases := []struct {
		name string
		edit func(img *image)
	}{
		{"the root's last child pointer moved", func(img *image) { img.entries[len(img.entries)-1].child = img.link }},
		{"the root is a level higher", func(img *image) { img.level++ }},
		{"the root became an empty leaf", func(img *image) { *img = image{id: img.id, leaf: true} }},
	}
	for _, c := range cases {
		src := bufpool.NewMemSource()
		tr, _, err := Create(newPool(t, src, 2, bufpool.LRU, bufpool.WriteBack))
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 400; i++ {
			mustPut(t, tr, k8(i), ridOf(i))
		}
		if h, _ := tr.Height(); h != 2 {
			t.Fatalf("%s: height %d, want 2", c.name, h)
		}
		if _, _, err := tr.Get(k8(0)); err != nil {
			t.Fatal(err)
		}
		pages := src.Len()
		rewriteRoot(t, src, tr, c.edit)
		n := 1 << 20
		putErr := putUntilError(tr, func() int { n++; return n }, 400)
		if !errors.Is(putErr, ErrCorrupt) {
			t.Fatalf("%s: a split under a parent that no longer matches gave %v, want ErrCorrupt", c.name, putErr)
		}
		if src.Len() != pages {
			t.Fatalf("%s: the refused split leaked %d pages", c.name, src.Len()-pages)
		}
		noPins(t, tr, c.name)
	}
}

type flakyPageSource struct {
	*bufpool.MemSource
	failNextWriteOf core.PageID
}

func (s *flakyPageSource) Write(p page.Page) error {
	if s.failNextWriteOf != 0 && p.ID() == s.failNextWriteOf {
		s.failNextWriteOf = 0
		return errInjected
	}
	return s.MemSource.Write(p)
}

func TestOneFailedWriteThroughDuringASplitDoesNotLeaveTheLiveTreeHalfSplit(t *testing.T) {
	src := &flakyPageSource{MemSource: bufpool.NewMemSource()}
	tr, root, err := Create(newPool(t, src, 16, bufpool.LRU, bufpool.WriteThrough))
	if err != nil {
		t.Fatal(err)
	}
	model := map[string]core.RowID{}
	for i := 0; i < 400; i++ {
		mustPut(t, tr, k8(2*i), ridOf(i))
		model[string(k8(2*i))] = ridOf(i)
	}
	if h, _ := tr.Height(); h != 2 {
		t.Fatalf("height %d, want 2", h)
	}
	src.failNextWriteOf = root
	var failed []byte
	for j := 0; j < 400 && failed == nil; j++ {
		k := k8(2*j + 1)
		err := tr.Put(k, ridOf(j))
		if err != nil && !errors.Is(err, errInjected) {
			t.Fatalf("Put gave %v", err)
		}
		if err != nil {
			failed = k
		}
		model[string(k)] = ridOf(j)
	}
	if failed == nil {
		t.Fatal("no split ever wrote the root")
	}
	noPins(t, tr, "the failed Put")
	if rid, ok, err := tr.Get(failed); err != nil || !ok || rid != model[string(failed)] {
		t.Fatalf("Get(% 02X) after its write-through failed = %+v %v %v; the change must stay in memory", failed, rid, ok, err)
	}
	if err := tr.Validate(); err != nil {
		t.Fatalf("after one failed page write the live tree no longer validates: %v", err)
	}
	var victim []byte
	for k := range model {
		if victim == nil || k > string(victim) {
			victim = []byte(k)
		}
	}
	if ok, err := tr.Delete(victim); err != nil || !ok {
		t.Fatalf("Delete(% 02X) = %v %v", victim, ok, err)
	}
	delete(model, string(victim))
	for _, e := range collect(t, tr.Scan(nil, nil)) {
		if bytes.Equal(e.key, victim) {
			t.Fatalf("Delete(% 02X) returned true, yet a full scan still returns the key", victim)
		}
	}
	checkModel(t, tr, model)
	if err := tr.Pool().FlushAll(); err != nil {
		t.Fatal(err)
	}
	again, err := Open(newPool(t, src.MemSource, 16, bufpool.LRU, bufpool.WriteBack), root)
	if err != nil {
		t.Fatal(err)
	}
	mustValidate(t, again)
	checkModel(t, again, model)
}

func failNext(src *armedSource) {
	src.live = true
	src.failAt = src.count + 1
}

func TestASplitCutShortByAFailedReadOrWriteIsUndoneOrFinishedByTheNextChange(t *testing.T) {
	undone, finished := 0, 0
	for _, frames := range []int{1, 2} {
		for _, preload := range []int{200, 400} {
			for _, writes := range []bool{false, true} {
				for at := 1; at <= 8; at++ {
					mem := bufpool.NewMemSource()
					src := &armedSource{inner: mem, failWrites: writes, failAt: at}
					tr, root, err := Create(newPool(t, src, frames, bufpool.LRU, bufpool.WriteBack))
					if err != nil {
						t.Fatal(err)
					}
					x := NewIndex(tr)
					model := map[string]core.RowID{}
					for i := 0; i < preload; i++ {
						mustPut(t, tr, k8(i*2), ridOf(i))
						model[string(k8(i*2))] = ridOf(i)
					}
					pages := mem.Len()
					src.armed = true
					var failed []byte
					var failedRID core.RowID
					for i := 0; i < preload && failed == nil; i++ {
						k := k8(i*2 + 1)
						if err := tr.Put(k, ridOf(i)); err != nil {
							if !errors.Is(err, errInjected) {
								t.Fatalf("Put gave %v", err)
							}
							failed, failedRID = k, ridOf(i)
							break
						}
						model[string(k)] = ridOf(i)
						pages = mem.Len()
					}
					src.armed = false
					if failed == nil {
						continue
					}
					noPins(t, tr, "the failed Put")
					if len(tr.pending) == 0 {
						undone++
						src.live = false
						if mem.Len() != pages {
							t.Fatalf("frames %d, preload %d, writes %v, at %d: the undone split left %d pages allocated",
								frames, preload, writes, at, mem.Len()-pages)
						}
						if _, ok, err := tr.Get(failed); ok || err != nil {
							t.Fatalf("the undone split's key is present: %v %v", ok, err)
						}
					} else {
						finished++
						refused := []struct {
							name string
							run  func() error
						}{
							{"Put", func() error { return tr.Put(k8(-1), ridOf(1)) }},
							{"Delete", func() error { _, err := tr.Delete(k8(0)); return err }},
							{"Validate", tr.Validate},
							{"Close", x.Close},
						}
						for _, op := range refused {
							failNext(src)
							if err := op.run(); !errors.Is(err, errInjected) || len(tr.pending) == 0 {
								t.Fatalf("%s while the split cannot be finished gave %v, with %d page images waiting",
									op.name, err, len(tr.pending))
							}
						}
						if _, err := tr.measure(); !errors.Is(err, ErrCorrupt) {
							t.Fatalf("a split left waiting must have changed an old page already, yet the tree checks as %v", err)
						}
						noPins(t, tr, "operations refused while a split waits")
						src.live = false
						model[string(failed)] = failedRID
					}
					s := mustValidate(t, tr)
					if s.Pages() != mem.Len() {
						t.Fatalf("frames %d, preload %d, writes %v, at %d: the tree owns %d pages, the source holds %d",
							frames, preload, writes, at, s.Pages(), mem.Len())
					}
					checkModel(t, tr, model)
					if err := x.Close(); err != nil {
						t.Fatal(err)
					}
					again, err := Open(newPool(t, mem, 4, bufpool.LRU, bufpool.WriteBack), root)
					if err != nil {
						t.Fatal(err)
					}
					mustValidate(t, again)
					checkModel(t, again, model)
				}
			}
		}
	}
	if undone == 0 || finished == 0 {
		t.Fatalf("%d splits were undone and %d finished later; the test must reach both", undone, finished)
	}
	t.Logf("%d failed splits were undone, %d were finished by the next change", undone, finished)
}

type waitingSplit struct {
	tr    *Tree
	mem   *bufpool.MemSource
	root  core.PageID
	model map[string]core.RowID
}

func cutShortSplit(t *testing.T) waitingSplit {
	t.Helper()
	for at := 1; at <= 12; at++ {
		mem := bufpool.NewMemSource()
		src := &armedSource{inner: mem, failAt: at}
		tr, root, err := Create(newPool(t, src, 1, bufpool.LRU, bufpool.WriteBack))
		if err != nil {
			t.Fatal(err)
		}
		model := map[string]core.RowID{}
		for i := 0; i < 400; i++ {
			mustPut(t, tr, k8(i*2), ridOf(i))
			model[string(k8(i*2))] = ridOf(i)
		}
		src.armed = true
		for i := 0; i < 400; i++ {
			k := k8(i*2 + 1)
			if err := tr.Put(k, ridOf(i)); err != nil {
				src.armed, src.live = false, false
				if len(tr.pending) == 0 {
					break
				}
				model[string(k)] = ridOf(i)
				return waitingSplit{tr: tr, mem: mem, root: root, model: model}
			}
			model[string(k)] = ridOf(i)
		}
	}
	t.Fatal("no failure point left a split waiting")
	return waitingSplit{}
}

func everyThird(model map[string]core.RowID) [][]byte {
	var out [][]byte
	for _, e := range expectRange(model, nil, nil) {
		if binary.BigEndian.Uint64(e.key)%3 == 0 {
			out = append(out, e.key)
		}
	}
	return out
}

func TestTheFirstChangeAfterTheDiskHealsFinishesTheWaitingSplitBeforeItsOwnWork(t *testing.T) {
	for _, first := range []string{"Put", "Delete", "Validate", "Close"} {
		w := cutShortSplit(t)
		if _, err := w.tr.measure(); !errors.Is(err, ErrCorrupt) {
			t.Fatalf("%s: the waiting split should leave the tree half split, it checks as %v", first, err)
		}
		switch first {
		case "Put":
			for _, k := range everyThird(w.model) {
				mustPut(t, w.tr, k, ridOf(9999))
				w.model[string(k)] = ridOf(9999)
			}
		case "Delete":
			for _, k := range everyThird(w.model) {
				if ok, err := w.tr.Delete(k); !ok || err != nil {
					t.Fatalf("Delete(% 02X) = %v %v", k, ok, err)
				}
				delete(w.model, string(k))
			}
		case "Validate":
			mustValidate(t, w.tr)
		case "Close":
			if err := NewIndex(w.tr).Close(); err != nil {
				t.Fatal(err)
			}
			again, err := Open(newPool(t, w.mem, 4, bufpool.LRU, bufpool.WriteBack), w.root)
			if err != nil {
				t.Fatal(err)
			}
			mustValidate(t, again)
			checkModel(t, again, w.model)
		}
		s := mustValidate(t, w.tr)
		if s.Pages() != w.mem.Len() {
			t.Fatalf("%s: the tree owns %d pages, the source holds %d", first, s.Pages(), w.mem.Len())
		}
		checkModel(t, w.tr, w.model)
	}
}
