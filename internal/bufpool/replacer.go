package bufpool

type Replacer interface {
	Access(frame int)
	SetEvictable(frame int, evictable bool)
	Evict() (int, bool)
	Remove(frame int)
	Len() int
}

type tracking struct {
	tracked   []bool
	evictable []bool
	count     int
}

func newTracking(frames int) tracking {
	if frames < 0 {
		frames = 0
	}
	if frames > MaxFrames {
		frames = MaxFrames
	}
	return tracking{tracked: make([]bool, frames), evictable: make([]bool, frames)}
}

func (t *tracking) valid(f int) bool { return f >= 0 && f < len(t.tracked) }

func (t *tracking) track(f int) bool {
	if t.tracked[f] {
		return false
	}
	t.tracked[f] = true
	t.evictable[f] = false
	return true
}

func (t *tracking) setEvictable(f int, on bool) {
	if !t.valid(f) || !t.tracked[f] || t.evictable[f] == on {
		return
	}
	t.evictable[f] = on
	if on {
		t.count++
	} else {
		t.count--
	}
}

func (t *tracking) untrack(f int) {
	if t.evictable[f] {
		t.count--
	}
	t.tracked[f] = false
	t.evictable[f] = false
}

func (t *tracking) Len() int { return t.count }

type lru struct {
	tracking
	prev, next []int
	head, tail int
}

func NewLRU(frames int) Replacer {
	r := &lru{tracking: newTracking(frames), head: -1, tail: -1}
	r.prev = make([]int, len(r.tracked))
	r.next = make([]int, len(r.tracked))
	return r
}

func (r *lru) unlink(f int) {
	if r.prev[f] >= 0 {
		r.next[r.prev[f]] = r.next[f]
	} else {
		r.head = r.next[f]
	}
	if r.next[f] >= 0 {
		r.prev[r.next[f]] = r.prev[f]
	} else {
		r.tail = r.prev[f]
	}
}

func (r *lru) pushBack(f int) {
	r.prev[f] = r.tail
	r.next[f] = -1
	if r.tail >= 0 {
		r.next[r.tail] = f
	} else {
		r.head = f
	}
	r.tail = f
}

func (r *lru) Access(f int) {
	if !r.valid(f) {
		return
	}
	if !r.track(f) {
		r.unlink(f)
	}
	r.pushBack(f)
}

func (r *lru) SetEvictable(f int, on bool) { r.setEvictable(f, on) }

func (r *lru) Evict() (int, bool) {
	for f := r.head; f >= 0; f = r.next[f] {
		if r.evictable[f] {
			r.unlink(f)
			r.untrack(f)
			return f, true
		}
	}
	return 0, false
}

func (r *lru) Remove(f int) {
	if !r.valid(f) || !r.tracked[f] {
		return
	}
	r.unlink(f)
	r.untrack(f)
}

type clock struct {
	tracking
	ref  []bool
	hand int
}

func NewClock(frames int) Replacer {
	r := &clock{tracking: newTracking(frames)}
	r.ref = make([]bool, len(r.tracked))
	return r
}

func (r *clock) Access(f int) {
	if !r.valid(f) {
		return
	}
	r.track(f)
	r.ref[f] = true
}

func (r *clock) SetEvictable(f int, on bool) { r.setEvictable(f, on) }

func (r *clock) Evict() (int, bool) {
	if r.count == 0 {
		return 0, false
	}
	n := len(r.tracked)
	for step := 0; step < 2*n+1; step++ {
		f := r.hand
		r.hand = (r.hand + 1) % n
		if !r.tracked[f] || !r.evictable[f] {
			continue
		}
		if r.ref[f] {
			r.ref[f] = false
			continue
		}
		r.untrack(f)
		return f, true
	}
	return 0, false
}

func (r *clock) Remove(f int) {
	if !r.valid(f) || !r.tracked[f] {
		return
	}
	r.untrack(f)
	r.ref[f] = false
}

type lruK struct {
	tracking
	k    int
	now  uint64
	hist [][]uint64
	seen []int
}

func NewLRUK(frames, k int) Replacer {
	if k < 1 {
		k = 1
	}
	if k > MaxK {
		k = MaxK
	}
	r := &lruK{tracking: newTracking(frames), k: k}
	r.hist = make([][]uint64, len(r.tracked))
	r.seen = make([]int, len(r.tracked))
	for i := range r.hist {
		r.hist[i] = make([]uint64, k)
	}
	return r
}

func (r *lruK) Access(f int) {
	if !r.valid(f) {
		return
	}
	if r.track(f) {
		r.seen[f] = 0
	}
	r.now++
	r.hist[f][r.seen[f]%r.k] = r.now
	r.seen[f]++
}

func (r *lruK) SetEvictable(f int, on bool) { r.setEvictable(f, on) }

func (r *lruK) oldest(f int) uint64 {
	if r.seen[f] < r.k {
		return r.hist[f][0]
	}
	return r.hist[f][r.seen[f]%r.k]
}

func (r *lruK) Evict() (int, bool) {
	victim, found := -1, false
	victimInf := false
	var victimTime uint64
	for f := range r.tracked {
		if !r.tracked[f] || !r.evictable[f] {
			continue
		}
		inf := r.seen[f] < r.k
		t := r.oldest(f)
		better := !found ||
			(inf && !victimInf) ||
			(inf == victimInf && t < victimTime)
		if better {
			victim, found, victimInf, victimTime = f, true, inf, t
		}
	}
	if !found {
		return 0, false
	}
	r.untrack(victim)
	r.seen[victim] = 0
	return victim, true
}

func (r *lruK) Remove(f int) {
	if !r.valid(f) || !r.tracked[f] {
		return
	}
	r.untrack(f)
	r.seen[f] = 0
}
