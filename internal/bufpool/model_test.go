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

type poolChecker struct {
	t        *testing.T
	p        *Pool
	src      *MemSource
	model    map[core.PageID][]byte
	held     []*Frame
	fetches  uint64
	refused  uint64
	installs uint64
	drops    uint64
}

func (c *poolChecker) ids() []core.PageID {
	out := make([]core.PageID, 0, len(c.model))
	for id := range c.model {
		out = append(out, id)
	}
	sort.Slice(out, func(a, b int) bool { return out[a] < out[b] })
	return out
}

func (c *poolChecker) heldPins(id core.PageID) int {
	n := 0
	for _, f := range c.held {
		if f.ID() == id {
			n++
		}
	}
	return n
}

func (c *poolChecker) step(op, arg int) {
	t, p := c.t, c.p
	ids := c.ids()
	switch op % 7 {
	case 0:
		if len(c.model) >= 40 {
			return
		}
		f, err := p.NewPage(page.KindHeap)
		if errors.Is(err, ErrNoFreeFrame) {
			if p.Pinned() != p.Frames() {
				t.Fatalf("NewPage said no free frame with %d of %d pinned", p.Pinned(), p.Frames())
			}
			return
		}
		if err != nil {
			t.Fatal(err)
		}
		c.installs++
		c.model[f.ID()] = append([]byte(nil), f.Page()...)
		c.held = append(c.held, f)
	case 1, 2:
		if len(ids) == 0 {
			return
		}
		id := ids[arg%len(ids)]
		wasResident := p.Contains(id)
		f, err := p.Fetch(id)
		c.fetches++
		if errors.Is(err, ErrNoFreeFrame) {
			if wasResident || p.Pinned() != p.Frames() {
				t.Fatalf("Fetch(%d) said no free frame: resident %v, %d of %d pinned", id, wasResident, p.Pinned(), p.Frames())
			}
			c.refused++
			c.check()
			return
		}
		if err != nil {
			t.Fatal(err)
		}
		if !wasResident {
			c.installs++
		}
		c.held = append(c.held, f)
	case 3, 4:
		if len(c.held) == 0 {
			return
		}
		i := arg % len(c.held)
		f := c.held[i]
		dirty := arg%3 != 0
		if dirty {
			at := page.HeaderSize + (arg*31)%(page.Size-page.HeaderSize)
			f.Page()[at] ^= byte(arg | 1)
			c.model[f.ID()][at] = f.Page()[at]
		}
		if err := p.Unpin(f, dirty); err != nil {
			t.Fatal(err)
		}
		c.held = append(c.held[:i], c.held[i+1:]...)
	case 5:
		if arg%4 == 0 {
			if err := p.FlushAll(); err != nil {
				t.Fatal(err)
			}
			for _, f := range p.frames {
				if f.state != stFree && f.dirty {
					t.Fatalf("page %d is still dirty after FlushAll", f.id)
				}
			}
			return
		}
		if len(ids) == 0 {
			return
		}
		id := ids[arg%len(ids)]
		if err := p.Flush(id); err != nil {
			t.Fatal(err)
		}
		if f, ok := p.peek(id); ok && f.dirty {
			t.Fatalf("page %d is still dirty after Flush", id)
		}
	case 6:
		if len(ids) == 0 {
			return
		}
		id := ids[arg%len(ids)]
		resident := p.Contains(id)
		err := p.Free(id)
		if c.heldPins(id) > 0 {
			if !errors.Is(err, ErrPinned) {
				t.Fatalf("Free of pinned page %d gave %v", id, err)
			}
			return
		}
		if err != nil {
			t.Fatal(err)
		}
		if resident {
			c.drops++
		}
		delete(c.model, id)
	}
	c.check()
}

func (c *poolChecker) check() {
	t, p := c.t, c.p
	pinned := map[core.PageID]int{}
	for _, f := range c.held {
		pinned[f.ID()]++
		if !bytes.Equal(f.Page(), c.model[f.ID()]) {
			t.Fatalf("pinned page %d does not hold what was written to it", f.ID())
		}
	}
	if p.Pinned() != len(pinned) {
		t.Fatalf("pool says %d frames pinned, the test holds %d pages", p.Pinned(), len(pinned))
	}
	if p.Resident() > p.Frames() {
		t.Fatalf("%d pages resident in %d frames", p.Resident(), p.Frames())
	}
	for id, want := range c.model {
		got, err := c.src.Read(id)
		if err != nil {
			t.Fatal(err)
		}
		f, resident := p.peek(id)
		if resident {
			if f.pins != pinned[id] {
				t.Fatalf("page %d has %d pins, the test holds %d", id, f.pins, pinned[id])
			}
			if !bytes.Equal(f.buf, want) {
				t.Fatalf("resident page %d differs from the model", id)
			}
			if f.dirty {
				if p.opts.Mode == WriteThrough && f.pins == 0 {
					t.Fatalf("write-through left page %d dirty after its last unpin", id)
				}
				continue
			}
		} else if pinned[id] > 0 {
			t.Fatalf("pinned page %d is not resident", id)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("page %d is clean or evicted but the source holds stale bytes", id)
		}
	}
	s := p.Stats()
	if s.Hits+s.Misses != c.fetches {
		t.Fatalf("hits %d + misses %d != %d fetches", s.Hits, s.Misses, c.fetches)
	}
	if s.Reads+c.refused != s.Misses {
		t.Fatalf("reads %d + %d refused fetches != misses %d", s.Reads, c.refused, s.Misses)
	}
	if s.Evictions != c.installs-c.drops-uint64(p.Resident()) {
		t.Fatalf("evictions %d, but %d installs - %d drops - %d resident = %d",
			s.Evictions, c.installs, c.drops, p.Resident(), c.installs-c.drops-uint64(p.Resident()))
	}
}

func (c *poolChecker) finish() {
	for len(c.held) > 0 {
		c.step(3, 0)
	}
	if err := c.p.FlushAll(); err != nil {
		c.t.Fatal(err)
	}
	c.check()
}

func newChecker(t *testing.T, opts Options) *poolChecker {
	src := NewMemSource()
	return &poolChecker{t: t, p: mustPool(t, src, opts), src: src, model: map[core.PageID][]byte{}}
}

func TestRandomPoolOperationsMatchAModel(t *testing.T) {
	n := 20000
	if testing.Short() {
		n = 3000
	}
	for _, mode := range []Mode{WriteBack, WriteThrough} {
		for _, pol := range allPolicies() {
			for _, frames := range []int{1, 3, 8} {
				r := rand.New(rand.NewSource(int64(frames)*100 + int64(pol)*10 + int64(mode)))
				c := newChecker(t, Options{Frames: frames, Policy: pol, Mode: mode})
				for i := 0; i < n; i++ {
					op := r.Intn(7)
					if (op == 1 || op == 2) && len(c.held) > frames {
						op = 3
					}
					c.step(op, r.Intn(1<<20))
				}
				c.finish()
			}
		}
	}
}

func FuzzPoolAgreesWithAModel(f *testing.F) {
	f.Add([]byte{0, 0, 0, 1, 3, 1, 1, 3, 2, 5, 4, 6, 1})
	f.Add([]byte{0x41, 0, 7, 0, 9, 1, 4, 3, 4, 2, 3, 6, 0, 5, 0})
	f.Add([]byte{0xFF, 0, 1, 0, 2, 0, 3, 0, 4, 1, 0, 1, 1, 6, 2, 4, 9})
	f.Fuzz(func(t *testing.T, script []byte) {
		if len(script) == 0 {
			return
		}
		if len(script) > 3000 {
			script = script[:3000]
		}
		head := script[0]
		opts := Options{Frames: int(head%5) + 1, Policy: Policy(head / 5 % 3), Mode: Mode(head / 15 % 2), K: int(head/30%3) + 1}
		c := newChecker(t, opts)
		for i := 1; i+1 < len(script); i += 2 {
			c.step(int(script[i]), int(script[i+1]))
		}
		c.finish()
	})
}
