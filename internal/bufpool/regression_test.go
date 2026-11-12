package bufpool

import (
	"errors"
	"fmt"
	"math"
	"testing"
)

func newPoolWithoutPanic(src Source, opts Options) (p *Pool, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v", r)
		}
	}()
	return New(src, opts)
}

func TestAnLRUKPoolWithAnAbsurdKIsRefusedNotAPanic(t *testing.T) {
	for _, k := range []int{MaxK + 1, 1 << 30, math.MaxInt / 2, math.MaxInt} {
		_, err := newPoolWithoutPanic(NewMemSource(), Options{Frames: 1, Policy: LRUK, K: k})
		if !errors.Is(err, ErrBadOptions) {
			t.Fatalf("New with K = %d gave %v, want ErrBadOptions", k, err)
		}
	}
}

func TestAnLRUKPoolAcceptsExactlyMaxK(t *testing.T) {
	p, err := New(NewMemSource(), Options{Frames: 2, Policy: LRUK, K: MaxK})
	if err != nil {
		t.Fatalf("New with K = MaxK gave %v", err)
	}
	if p.Options().K != MaxK {
		t.Fatalf("the pool kept K = %d, want %d", p.Options().K, MaxK)
	}
}

func TestReplacersBuiltDirectlyWithAbsurdSizesAreCappedNotAPanic(t *testing.T) {
	var r Replacer
	func() {
		defer func() {
			if v := recover(); v != nil {
				t.Fatalf("NewLRUK with a huge K panicked: %v", v)
			}
		}()
		r = NewLRUK(2, math.MaxInt)
	}()
	for _, f := range []int{0, 1, 1, 0} {
		r.Access(f)
		r.SetEvictable(f, true)
	}
	if v, ok := r.Evict(); !ok || v != 0 {
		t.Fatalf("an LRU-K replacer with K capped at MaxK evicted %d %v, want frame 0 (both have fewer than K uses, 0 first)", v, ok)
	}
	for _, build := range []func(int) Replacer{NewLRU, NewClock} {
		func() {
			defer func() {
				if v := recover(); v != nil {
					t.Fatalf("a replacer for math.MaxInt frames panicked: %v", v)
				}
			}()
			r = build(math.MaxInt)
		}()
		r.Access(MaxFrames - 1)
		r.SetEvictable(MaxFrames-1, true)
		r.Access(MaxFrames)
		r.SetEvictable(MaxFrames, true)
		if r.Len() != 1 {
			t.Fatalf("a replacer capped at MaxFrames tracks %d evictable frames, want 1", r.Len())
		}
	}
}
