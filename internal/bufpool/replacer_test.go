package bufpool

import (
	"math/rand"
	"reflect"
	"testing"
)

func accessAll(r Replacer, frames ...int) {
	for _, f := range frames {
		r.Access(f)
		r.SetEvictable(f, true)
	}
}

func drain(r Replacer) []int {
	var out []int
	for {
		f, ok := r.Evict()
		if !ok {
			return out
		}
		out = append(out, f)
	}
}

func TestLRUEvictsTheLeastRecentlyUsedFrameFirst(t *testing.T) {
	r := NewLRU(5)
	accessAll(r, 0, 1, 2, 3, 4)
	r.Access(0)
	r.Access(2)
	if got, want := drain(r), []int{1, 3, 4, 0, 2}; !reflect.DeepEqual(got, want) {
		t.Fatalf("eviction order %v, want %v", got, want)
	}
	if r.Len() != 0 {
		t.Fatalf("Len is %d after draining", r.Len())
	}
}

func TestLRUSkipsPinnedFramesAndEvictsThemOnceUnpinned(t *testing.T) {
	r := NewLRU(4)
	accessAll(r, 0, 1, 2, 3)
	r.SetEvictable(0, false)
	r.SetEvictable(2, false)
	if r.Len() != 2 {
		t.Fatalf("Len is %d with two of four pinned, want 2", r.Len())
	}
	if got, want := drain(r), []int{1, 3}; !reflect.DeepEqual(got, want) {
		t.Fatalf("eviction order %v, want %v", got, want)
	}
	r.SetEvictable(2, true)
	r.SetEvictable(0, true)
	if got, want := drain(r), []int{0, 2}; !reflect.DeepEqual(got, want) {
		t.Fatalf("after unpinning, eviction order %v, want %v (recency, not unpin order)", got, want)
	}
}

func TestClockGivesEveryReferencedFrameASecondChance(t *testing.T) {
	r := NewClock(4)
	accessAll(r, 0, 1, 2, 3)
	f, ok := r.Evict()
	if !ok || f != 0 {
		t.Fatalf("first victim %d, %v; want 0: the hand clears all four bits, wraps, and takes 0", f, ok)
	}
	r.Access(2)
	if got, want := drain(r), []int{1, 3, 2}; !reflect.DeepEqual(got, want) {
		t.Fatalf("eviction order %v, want %v: 2 was touched, so 3 goes before it", got, want)
	}
}

func TestClockHandResumesWhereItStopped(t *testing.T) {
	r := NewClock(5)
	accessAll(r, 0, 1, 2, 3, 4)
	if f, _ := r.Evict(); f != 0 {
		t.Fatalf("first victim %d, want 0", f)
	}
	r.Access(0)
	r.SetEvictable(0, true)
	r.Access(1)
	if got, want := drain(r), []int{2, 3, 4, 1, 0}; !reflect.DeepEqual(got, want) {
		t.Fatalf("eviction order %v, want %v: the hand spends 1's bit first, then comes back to 0 last", got, want)
	}
}

func TestClockSkipsPinnedFrames(t *testing.T) {
	r := NewClock(3)
	accessAll(r, 0, 1, 2)
	r.SetEvictable(1, false)
	if got, want := drain(r), []int{0, 2}; !reflect.DeepEqual(got, want) {
		t.Fatalf("eviction order %v, want %v", got, want)
	}
	if _, ok := r.Evict(); ok {
		t.Fatal("Evict found a victim while the only frame left is pinned")
	}
	r.SetEvictable(1, true)
	if got, want := drain(r), []int{1}; !reflect.DeepEqual(got, want) {
		t.Fatalf("eviction order %v, want %v", got, want)
	}
}

func TestLRUKEvictsFramesWithFewerThanKAccessesFirstByEarliestAccess(t *testing.T) {
	r := NewLRUK(8, 2)
	accessAll(r, 1, 2, 3, 4, 5, 6)
	r.Access(1)
	if got, want := []int{evictOne(t, r), evictOne(t, r), evictOne(t, r)}, []int{2, 3, 4}; !reflect.DeepEqual(got, want) {
		t.Fatalf("first three victims %v, want %v", got, want)
	}
	r.Access(5)
	if got, want := drain(r), []int{6, 1, 5}; !reflect.DeepEqual(got, want) {
		t.Fatalf("eviction order %v, want %v: 6 has one access so it goes first, then the larger K-distance", got, want)
	}
}

func TestLRUKUsesTheKthMostRecentAccessNotTheLatest(t *testing.T) {
	r := NewLRUK(4, 2)
	accessAll(r, 0)
	r.Access(0)
	accessAll(r, 1)
	r.Access(1)
	r.Access(0)
	if got, want := drain(r), []int{0, 1}; !reflect.DeepEqual(got, want) {
		t.Fatalf("eviction order %v, want %v: frame 0's second-latest access (t2) is older than frame 1's (t3)", got, want)
	}
}

func TestLRUKWithKEqualOneIsLRU(t *testing.T) {
	a, b := NewLRUK(6, 1), NewLRU(6)
	r := rand.New(rand.NewSource(3))
	for step := 0; step < 5000; step++ {
		f := r.Intn(6)
		switch r.Intn(4) {
		case 0, 1:
			a.Access(f)
			b.Access(f)
			a.SetEvictable(f, true)
			b.SetEvictable(f, true)
		case 2:
			x, okx := a.Evict()
			y, oky := b.Evict()
			if x != y || okx != oky {
				t.Fatalf("step %d: LRU-1 evicted %d %v, LRU evicted %d %v", step, x, okx, y, oky)
			}
		case 3:
			on := r.Intn(2) == 0
			a.SetEvictable(f, on)
			b.SetEvictable(f, on)
		}
	}
	if NewLRUK(2, 0).(*lruK).k != 1 {
		t.Fatal("NewLRUK with k 0 should fall back to k 1")
	}
}

func evictOne(t *testing.T, r Replacer) int {
	t.Helper()
	f, ok := r.Evict()
	if !ok {
		t.Fatal("Evict found nothing")
	}
	return f
}

func TestRemoveForgetsAFrameEvenWhenPinned(t *testing.T) {
	for name, r := range map[string]Replacer{"lru": NewLRU(3), "clock": NewClock(3), "lruk": NewLRUK(3, 2)} {
		accessAll(r, 0, 1, 2)
		r.SetEvictable(1, false)
		r.Remove(1)
		r.Remove(0)
		r.Remove(0)
		if r.Len() != 1 {
			t.Fatalf("%s: Len is %d after removing two, want 1", name, r.Len())
		}
		if got := drain(r); !reflect.DeepEqual(got, []int{2}) {
			t.Fatalf("%s: drained %v, want [2]", name, got)
		}
		r.SetEvictable(1, true)
		if _, ok := r.Evict(); ok {
			t.Fatalf("%s: SetEvictable revived a removed frame", name)
		}
	}
}

func TestReplacersIgnoreFramesOutOfRange(t *testing.T) {
	for name, r := range map[string]Replacer{"lru": NewLRU(2), "clock": NewClock(2), "lruk": NewLRUK(2, 2)} {
		for _, f := range []int{-1, 2, 99} {
			r.Access(f)
			r.SetEvictable(f, true)
			r.Remove(f)
		}
		if r.Len() != 0 {
			t.Fatalf("%s: out-of-range frames were counted", name)
		}
		if _, ok := r.Evict(); ok {
			t.Fatalf("%s: Evict returned a frame that was never valid", name)
		}
	}
	for name, r := range map[string]Replacer{"lru": NewLRU(-3), "clock": NewClock(0), "lruk": NewLRUK(-1, 2)} {
		r.Access(0)
		if _, ok := r.Evict(); ok || r.Len() != 0 {
			t.Fatalf("%s: a replacer with no frames evicted something", name)
		}
	}
}

type refReplacer struct {
	k         int
	tracked   map[int]bool
	evictable map[int]bool
	log       map[int][]int
	now       int
}

func newRef(k int) *refReplacer {
	return &refReplacer{k: k, tracked: map[int]bool{}, evictable: map[int]bool{}, log: map[int][]int{}}
}

func (r *refReplacer) Access(f int) {
	if !r.tracked[f] {
		r.tracked[f] = true
		r.evictable[f] = false
		r.log[f] = nil
	}
	r.now++
	r.log[f] = append(r.log[f], r.now)
}

func (r *refReplacer) SetEvictable(f int, on bool) {
	if r.tracked[f] {
		r.evictable[f] = on
	}
}

func (r *refReplacer) Remove(f int) {
	delete(r.tracked, f)
	delete(r.evictable, f)
	delete(r.log, f)
}

func (r *refReplacer) Len() int {
	n := 0
	for f := range r.tracked {
		if r.evictable[f] {
			n++
		}
	}
	return n
}

func (r *refReplacer) Evict() (int, bool) {
	best, bestInf, bestTime := -1, false, 0
	for f := range r.tracked {
		if !r.evictable[f] {
			continue
		}
		l := r.log[f]
		inf := len(l) < r.k
		var at int
		if inf {
			at = l[0]
		} else {
			at = l[len(l)-r.k]
		}
		if best < 0 || (inf && !bestInf) || (inf == bestInf && at < bestTime) {
			best, bestInf, bestTime = f, inf, at
		}
	}
	if best < 0 {
		return 0, false
	}
	r.Remove(best)
	return best, true
}

func TestLRUAndLRUKMatchAReferenceThatKeepsEveryAccess(t *testing.T) {
	cases := []struct {
		name string
		k    int
		mk   func(int) Replacer
	}{
		{"lru", 1, func(n int) Replacer { return NewLRU(n) }},
		{"lru-2", 2, func(n int) Replacer { return NewLRUK(n, 2) }},
		{"lru-3", 3, func(n int) Replacer { return NewLRUK(n, 3) }},
	}
	for _, c := range cases {
		r := rand.New(rand.NewSource(int64(7 + c.k)))
		const frames = 9
		got, ref := c.mk(frames), newRef(c.k)
		for step := 0; step < 30000; step++ {
			f := r.Intn(frames)
			switch op := r.Intn(10); {
			case op < 4:
				got.Access(f)
				ref.Access(f)
			case op < 7:
				on := r.Intn(3) != 0
				got.SetEvictable(f, on)
				ref.SetEvictable(f, on)
			case op < 9:
				x, okx := got.Evict()
				y, oky := ref.Evict()
				if x != y || okx != oky {
					t.Fatalf("%s step %d: evicted %d %v, reference evicted %d %v", c.name, step, x, okx, y, oky)
				}
			default:
				got.Remove(f)
				ref.Remove(f)
			}
			if got.Len() != ref.Len() {
				t.Fatalf("%s step %d: Len %d, reference %d", c.name, step, got.Len(), ref.Len())
			}
		}
	}
}

func TestClockNeverEvictsAPinnedOrUnknownFrame(t *testing.T) {
	r := rand.New(rand.NewSource(11))
	const frames = 7
	c := NewClock(frames)
	tracked := map[int]bool{}
	evictable := map[int]bool{}
	for step := 0; step < 30000; step++ {
		f := r.Intn(frames)
		switch op := r.Intn(10); {
		case op < 4:
			c.Access(f)
			if !tracked[f] {
				tracked[f] = true
				evictable[f] = false
			}
		case op < 7:
			on := r.Intn(2) == 0
			c.SetEvictable(f, on)
			if tracked[f] {
				evictable[f] = on
			}
		case op < 9:
			v, ok := c.Evict()
			want := 0
			for g := range tracked {
				if evictable[g] {
					want++
				}
			}
			if ok != (want > 0) {
				t.Fatalf("step %d: Evict said %v with %d evictable frames", step, ok, want)
			}
			if ok {
				if !tracked[v] || !evictable[v] {
					t.Fatalf("step %d: clock evicted frame %d which is pinned or unknown", step, v)
				}
				delete(tracked, v)
				delete(evictable, v)
			}
		default:
			c.Remove(f)
			delete(tracked, f)
			delete(evictable, f)
		}
		want := 0
		for g := range tracked {
			if evictable[g] {
				want++
			}
		}
		if c.Len() != want {
			t.Fatalf("step %d: Len %d, want %d", step, c.Len(), want)
		}
	}
}
