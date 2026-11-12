package bufpool

import (
	"bytes"
	"errors"
	"math/rand"
	"sort"
	"testing"

	"github.com/scute-db/scutedb/internal/core"
	"github.com/scute-db/scutedb/internal/page"
)

type mtrChecker struct {
	t       *testing.T
	p       *Pool
	src     *MemSource
	model   map[core.PageID][]byte
	work    map[core.PageID][]byte
	m       *MTR
	frames  map[core.PageID]*Frame
	latched map[core.PageID]latchMode
	fresh   map[core.PageID]bool
	frees   []core.PageID
	commits int
	aborts  int
}

func (c *mtrChecker) sorted(m map[core.PageID][]byte) []core.PageID {
	var out []core.PageID
	for id := range m {
		out = append(out, id)
	}
	sort.Slice(out, func(a, b int) bool { return out[a] < out[b] })
	return out
}

func (c *mtrChecker) heldIDs() []core.PageID {
	var out []core.PageID
	for id := range c.frames {
		out = append(out, id)
	}
	sort.Slice(out, func(a, b int) bool { return out[a] < out[b] })
	return out
}

func (c *mtrChecker) begin() {
	c.m = c.p.Begin()
	c.work = map[core.PageID][]byte{}
	for id, b := range c.model {
		c.work[id] = append([]byte(nil), b...)
	}
	c.frames, c.latched, c.fresh, c.frees = map[core.PageID]*Frame{}, map[core.PageID]latchMode{}, map[core.PageID]bool{}, nil
}

func (c *mtrChecker) full(err error) bool {
	return errors.Is(err, ErrNoFreeFrame) && c.p.Pinned() == c.p.Frames()
}

func (c *mtrChecker) step(op, arg int) {
	t := c.t
	if c.m == nil {
		c.begin()
	}
	held := c.heldIDs()
	switch op % 10 {
	case 0, 1:
		ids := c.sorted(c.work)
		if len(ids) == 0 {
			return
		}
		id := ids[arg%len(ids)]
		f, err := c.m.Fetch(id)
		if c.full(err) {
			return
		}
		if err != nil {
			t.Fatalf("m.Fetch(%d): %v", id, err)
		}
		c.frames[id] = f
	case 2:
		if len(c.work) >= 24 {
			return
		}
		f, err := c.m.NewPage(page.KindHeap)
		if c.full(err) {
			return
		}
		if err != nil {
			t.Fatal(err)
		}
		c.frames[f.ID()] = f
		c.fresh[f.ID()] = true
		c.work[f.ID()] = append([]byte(nil), f.Page()...)
	case 3, 4:
		if len(held) == 0 {
			return
		}
		id := held[arg%len(held)]
		want := exclusive
		var err error
		if op%10 == 4 {
			want = shared
			err = c.m.RLatch(c.frames[id])
		} else {
			err = c.m.WLatch(c.frames[id])
		}
		switch {
		case c.latched[id] == shared && want == exclusive:
			if !errors.Is(err, ErrUpgrade) {
				t.Fatalf("WLatch over a read latch gave %v", err)
			}
		case err != nil:
			t.Fatal(err)
		case c.latched[id] == 0:
			c.latched[id] = want
		}
	case 5, 6:
		for _, id := range held {
			if c.latched[id] == exclusive && (arg%len(held)) == 0 {
				at := page.HeaderSize + (arg*13)%(page.Size-page.HeaderSize)
				c.frames[id].Page()[at] ^= byte(arg | 1)
				c.work[id][at] = c.frames[id].Page()[at]
				break
			}
			arg++
		}
	case 7:
		if len(held) == 0 {
			return
		}
		id := held[arg%len(held)]
		changed := !bytes.Equal(c.work[id], c.model[id]) || c.fresh[id]
		err := c.m.Release(c.frames[id])
		if c.latched[id] == exclusive && !bytes.Equal(c.work[id], c.beforeOf(id)) {
			if !errors.Is(err, ErrModified) {
				t.Fatalf("Release of changed page %d gave %v", id, err)
			}
			return
		}
		if err != nil {
			t.Fatalf("Release(%d): %v (changed %v)", id, err, changed)
		}
		delete(c.frames, id)
		delete(c.latched, id)
	case 8:
		if arg%3 == 0 {
			ids := c.sorted(c.work)
			if len(ids) == 0 {
				return
			}
			id := ids[arg%len(ids)]
			for _, x := range c.frees {
				if x == id {
					return
				}
			}
			c.frees = append(c.frees, id)
			if err := c.m.Free(id); err != nil {
				t.Fatal(err)
			}
			return
		}
		c.finish(arg%2 == 0)
	case 9:
		if arg%5 == 0 {
			if err := c.p.FlushAll(); err != nil {
				t.Fatal(err)
			}
		}
	}
	c.check()
}

func (c *mtrChecker) beforeOf(id core.PageID) []byte {
	h := c.m.byID[id]
	if h == nil || h.before == nil {
		return c.work[id]
	}
	return h.before
}

func (c *mtrChecker) finish(commit bool) {
	t := c.t
	var err error
	if commit {
		modified := map[core.PageID]bool{}
		for _, id := range c.m.Modified() {
			modified[id] = true
		}
		err = c.m.Commit()
		c.model = c.work
		for _, id := range c.frees {
			delete(c.model, id)
		}
		for id, f := range c.frames {
			if _, live := c.model[id]; !live || !modified[id] {
				continue
			}
			if c.p.opts.Mode == WriteBack && !f.Dirty() {
				t.Fatalf("page %d changed in a committed MTR but is clean", id)
			}
			if got, _ := c.src.Read(id); c.p.opts.Mode == WriteThrough && !bytes.Equal(got, c.model[id]) {
				t.Fatalf("write-through did not write page %d at commit", id)
			}
		}
		c.commits++
	} else {
		err = c.m.Abort()
		c.aborts++
	}
	if err != nil {
		t.Fatalf("finish (commit %v): %v", commit, err)
	}
	if c.p.Pinned() != 0 {
		t.Fatalf("%d frames pinned after the MTR ended", c.p.Pinned())
	}
	c.m = nil
	if c.src.Len() != len(c.model) {
		t.Fatalf("the source holds %d pages, the model %d", c.src.Len(), len(c.model))
	}
	for id, want := range c.model {
		f, err := c.p.Fetch(id)
		if c.full(err) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		got := bytes.Equal(f.Page(), want)
		if err := c.p.Unpin(f, false); err != nil {
			t.Fatal(err)
		}
		if !got {
			t.Fatalf("page %d does not hold its committed bytes", id)
		}
	}
}

func (c *mtrChecker) check() {
	t := c.t
	if c.m == nil {
		return
	}
	for id, f := range c.frames {
		if !bytes.Equal(f.Page(), c.work[id]) {
			t.Fatalf("held page %d differs from the MTR's view", id)
		}
	}
	if c.m.Held() != len(c.frames) || c.p.Pinned() != len(c.frames) {
		t.Fatalf("the MTR holds %d pages and %d frames are pinned, the test holds %d", c.m.Held(), c.p.Pinned(), len(c.frames))
	}
}

func newMTRChecker(t *testing.T, frames int, mode Mode, seed int) *mtrChecker {
	src := NewMemSource()
	c := &mtrChecker{t: t, p: mustPool(t, src, Options{Frames: frames, Mode: mode, Policy: Policy(seed % 3)}), src: src, model: map[core.PageID][]byte{}}
	for i := 0; i < 6; i++ {
		pg, err := src.Allocate(page.KindHeap)
		if err != nil {
			t.Fatal(err)
		}
		pg[page.HeaderSize] = byte(i)
		if err := src.Write(pg); err != nil {
			t.Fatal(err)
		}
		c.model[pg.ID()] = pg
	}
	return c
}

func (c *mtrChecker) end() {
	if c.m != nil {
		c.finish(true)
	}
	if err := c.p.FlushAll(); err != nil {
		c.t.Fatal(err)
	}
	for id, want := range c.model {
		got, err := c.src.Read(id)
		if err != nil {
			c.t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			c.t.Fatalf("after FlushAll the source holds stale bytes for page %d", id)
		}
	}
}

func TestRandomMiniTransactionsMatchAModel(t *testing.T) {
	steps := 6000
	if testing.Short() {
		steps = 1500
	}
	for seed := 0; seed < 6; seed++ {
		r := rand.New(rand.NewSource(int64(seed)))
		c := newMTRChecker(t, 2+seed, Mode(seed%2), seed)
		for i := 0; i < steps; i++ {
			c.step(r.Intn(10), r.Intn(1<<20))
		}
		c.end()
		if c.commits == 0 || c.aborts == 0 {
			t.Fatalf("seed %d: %d commits, %d aborts", seed, c.commits, c.aborts)
		}
	}
}

func FuzzMiniTransactionsAgreeWithAModel(f *testing.F) {
	f.Add([]byte{3, 0, 1, 0, 3, 5, 0, 8, 2, 2, 0, 3, 1, 5, 0, 8, 1})
	f.Add([]byte{0x11, 2, 0, 3, 1, 5, 0, 7, 0, 8, 3, 9, 0})
	f.Fuzz(func(t *testing.T, script []byte) {
		if len(script) < 2 || len(script) > 4000 {
			return
		}
		c := newMTRChecker(t, int(script[0]%6)+1, Mode(script[0]/6%2), int(script[0]/12))
		for i := 1; i+1 < len(script); i += 2 {
			c.step(int(script[i]), int(script[i+1]))
		}
		c.end()
	})
}
