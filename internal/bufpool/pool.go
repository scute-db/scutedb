package bufpool

import (
	"errors"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"

	"github.com/scute-db/scutedb/internal/core"
	"github.com/scute-db/scutedb/internal/latch"
	"github.com/scute-db/scutedb/internal/page"
)

type Source interface {
	Read(core.PageID) (page.Page, error)
	Write(page.Page) error
	Allocate(page.Kind) (page.Page, error)
	Free(core.PageID) error
}

type Policy uint8

const (
	LRU Policy = iota
	Clock
	LRUK
)

func (p Policy) String() string {
	switch p {
	case LRU:
		return "LRU"
	case Clock:
		return "Clock"
	case LRUK:
		return "LRU-K"
	}
	return fmt.Sprintf("policy(%d)", uint8(p))
}

type Mode uint8

const (
	WriteBack Mode = iota
	WriteThrough
)

func (m Mode) String() string {
	switch m {
	case WriteBack:
		return "write-back"
	case WriteThrough:
		return "write-through"
	}
	return fmt.Sprintf("mode(%d)", uint8(m))
}

const (
	DefaultK       = 2
	MaxK           = 64
	MaxFrames      = 1 << 20
	DefaultStripes = 16
	MaxStripes     = latch.MaxStripes
)

type Options struct {
	Frames  int
	Policy  Policy
	K       int
	Mode    Mode
	Stripes int
}

var (
	ErrNoFreeFrame = errors.New("scutedb/bufpool: every frame is pinned")
	ErrNotPinned   = errors.New("scutedb/bufpool: frame is not pinned in this pool")
	ErrPinned      = errors.New("scutedb/bufpool: page is pinned")
	ErrBadOptions  = errors.New("scutedb/bufpool: bad options")
	ErrIDMismatch  = errors.New("scutedb/bufpool: page header names a different page")
	ErrShortPage   = errors.New("scutedb/bufpool: source returned a page of the wrong size")
)

type Stats struct {
	Hits      uint64
	Misses    uint64
	Evictions uint64
	Writes    uint64
	Reads     uint64
}

func (s Stats) HitRatio() float64 {
	total := s.Hits + s.Misses
	if total == 0 {
		return 0
	}
	return float64(s.Hits) / float64(total)
}

type frameState uint8

const (
	stFree frameState = iota
	stLoading
	stReady
	stTransit
)

type Frame struct {
	pool     *Pool
	index    int
	latch    sync.RWMutex
	buf      page.Page
	id       core.PageID
	state    frameState
	done     chan struct{}
	pins     int
	dirty    bool
	claim    uint64
	verified atomic.Bool
}

func (f *Frame) ID() core.PageID { return f.id }
func (f *Frame) Page() page.Page { return f.buf }
func (f *Frame) Verified() bool  { return f.verified.Load() }
func (f *Frame) MarkVerified()   { f.verified.Store(true) }

func (f *Frame) Pins() int {
	f.pool.mu.Lock()
	defer f.pool.mu.Unlock()
	return f.pins
}

func (f *Frame) Dirty() bool {
	f.pool.mu.Lock()
	defer f.pool.mu.Unlock()
	return f.dirty
}

type Pool struct {
	src    Source
	opts   Options
	frames []*Frame
	locks  *latch.Striped
	tables []map[core.PageID]*Frame

	mu   sync.Mutex
	free []int
	rep  Replacer
	seq  uint64

	onWait func(core.PageID)

	hits, misses, evictions, writes, reads atomic.Uint64
}

func New(src Source, opts Options) (*Pool, error) {
	if src == nil {
		return nil, fmt.Errorf("%w: no source", ErrBadOptions)
	}
	if opts.Frames < 1 || opts.Frames > MaxFrames {
		return nil, fmt.Errorf("%w: %d frames, need 1 to %d", ErrBadOptions, opts.Frames, MaxFrames)
	}
	if opts.K == 0 {
		opts.K = DefaultK
	}
	if opts.K < 1 || opts.K > MaxK {
		return nil, fmt.Errorf("%w: K is %d, need 1 to %d", ErrBadOptions, opts.K, MaxK)
	}
	if opts.Stripes == 0 {
		opts.Stripes = DefaultStripes
	}
	if opts.Stripes < 1 || opts.Stripes > MaxStripes {
		return nil, fmt.Errorf("%w: %d stripes, need 1 to %d", ErrBadOptions, opts.Stripes, MaxStripes)
	}
	var rep Replacer
	switch opts.Policy {
	case LRU:
		rep = NewLRU(opts.Frames)
	case Clock:
		rep = NewClock(opts.Frames)
	case LRUK:
		rep = NewLRUK(opts.Frames, opts.K)
	default:
		return nil, fmt.Errorf("%w: unknown policy %s", ErrBadOptions, opts.Policy)
	}
	if opts.Mode != WriteBack && opts.Mode != WriteThrough {
		return nil, fmt.Errorf("%w: unknown mode %s", ErrBadOptions, opts.Mode)
	}

	locks := latch.NewStriped(opts.Stripes)
	opts.Stripes = locks.Stripes()
	p := &Pool{
		src:    src,
		opts:   opts,
		frames: make([]*Frame, opts.Frames),
		locks:  locks,
		tables: make([]map[core.PageID]*Frame, locks.Stripes()),
		free:   make([]int, 0, opts.Frames),
		rep:    rep,
	}
	for i := range p.tables {
		p.tables[i] = map[core.PageID]*Frame{}
	}
	slab := make([]byte, opts.Frames*page.Size)
	frames := make([]Frame, opts.Frames)
	for i := range p.frames {
		f := &frames[i]
		f.pool, f.index = p, i
		f.buf = page.Page(slab[i*page.Size : (i+1)*page.Size : (i+1)*page.Size])
		p.frames[i] = f
	}
	for i := opts.Frames - 1; i >= 0; i-- {
		p.free = append(p.free, i)
	}
	return p, nil
}

func (p *Pool) Options() Options  { return p.opts }
func (p *Pool) HitRatio() float64 { return p.Stats().HitRatio() }
func (p *Pool) Frames() int       { return len(p.frames) }

func (p *Pool) Stats() Stats {
	return Stats{
		Hits:      p.hits.Load(),
		Misses:    p.misses.Load(),
		Evictions: p.evictions.Load(),
		Writes:    p.writes.Load(),
		Reads:     p.reads.Load(),
	}
}

func (p *Pool) ResetStats() {
	for _, c := range []*atomic.Uint64{&p.hits, &p.misses, &p.evictions, &p.writes, &p.reads} {
		c.Store(0)
	}
}

func (p *Pool) stripe(id core.PageID) (*sync.RWMutex, map[core.PageID]*Frame) {
	i := p.locks.Index(uint64(id))
	return p.locks.At(i), p.tables[i]
}

func (p *Pool) lookup(id core.PageID) (*Frame, bool) {
	mu, table := p.stripe(id)
	mu.RLock()
	defer mu.RUnlock()
	f, ok := table[id]
	return f, ok
}

func (p *Pool) Contains(id core.PageID) bool {
	_, ok := p.lookup(id)
	return ok
}

func (p *Pool) Resident() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, f := range p.frames {
		if f.state != stFree {
			n++
		}
	}
	return n
}

func (p *Pool) Pinned() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, f := range p.frames {
		if f.pins > 0 {
			n++
		}
	}
	return n
}

func (p *Pool) RLatch(f *Frame)   { f.latch.RLock() }
func (p *Pool) RUnlatch(f *Frame) { f.latch.RUnlock() }
func (p *Pool) WLatch(f *Frame)   { f.latch.Lock() }
func (p *Pool) WUnlatch(f *Frame) { f.latch.Unlock() }

func (p *Pool) wait(id core.PageID, done chan struct{}) {
	if p.onWait != nil {
		p.onWait(id)
	}
	<-done
}

func (p *Pool) Fetch(id core.PageID) (*Frame, error) {
	mu, table := p.stripe(id)
	for {
		mu.RLock()
		if f, ok := table[id]; ok {
			if f.state != stReady {
				done := f.done
				mu.RUnlock()
				p.wait(id, done)
				continue
			}
			p.mu.Lock()
			p.pinLocked(f, true)
			p.mu.Unlock()
			mu.RUnlock()
			p.hits.Add(1)
			return f, nil
		}
		mu.RUnlock()

		fr, err := p.grab()
		if err != nil {
			p.misses.Add(1)
			return nil, fmt.Errorf("scutedb/bufpool: fetch page %d: %w", id, err)
		}
		mu.Lock()
		if _, ok := table[id]; ok {
			mu.Unlock()
			p.giveBack(fr)
			continue
		}
		p.mu.Lock()
		p.installLocked(fr, id, stLoading)
		p.mu.Unlock()
		table[id] = fr
		mu.Unlock()
		p.misses.Add(1)
		return p.load(fr, id, mu, table)
	}
}

func (p *Pool) load(f *Frame, id core.PageID, mu *sync.RWMutex, table map[core.PageID]*Frame) (*Frame, error) {
	raw, err := p.src.Read(id)
	if err != nil {
		err = fmt.Errorf("scutedb/bufpool: read page %d: %w", id, err)
	} else {
		p.reads.Add(1)
		err = check(raw, id)
	}
	if err == nil {
		copy(f.buf, raw)
	}
	mu.Lock()
	p.mu.Lock()
	done := f.done
	f.done = nil
	if err != nil {
		delete(table, id)
		p.rep.Remove(f.index)
		f.pins = 0
		f.state = stFree
		p.free = append(p.free, f.index)
	} else {
		f.state = stReady
	}
	p.mu.Unlock()
	mu.Unlock()
	close(done)
	if err != nil {
		return nil, err
	}
	return f, nil
}

func (p *Pool) installLocked(f *Frame, id core.PageID, st frameState) {
	f.id = id
	f.state = st
	f.verified.Store(false)
	if st == stLoading {
		f.done = make(chan struct{})
	}
	p.pinLocked(f, true)
}

func (p *Pool) pinLocked(f *Frame, access bool) {
	if f.claim != 0 {
		f.claim = 0
		access = true
	}
	if access {
		p.rep.Access(f.index)
	}
	f.pins++
	p.rep.SetEvictable(f.index, false)
}

func (p *Pool) unpinLocked(f *Frame) {
	f.pins--
	if f.pins == 0 {
		p.rep.SetEvictable(f.index, true)
	}
}

func (p *Pool) giveBack(f *Frame) {
	p.mu.Lock()
	p.free = append(p.free, f.index)
	p.mu.Unlock()
}

func (p *Pool) NewPage(kind page.Kind) (*Frame, error) {
	fr, err := p.grab()
	if err != nil {
		return nil, fmt.Errorf("scutedb/bufpool: new page: %w", err)
	}
	raw, err := p.src.Allocate(kind)
	if err != nil {
		p.giveBack(fr)
		return nil, fmt.Errorf("scutedb/bufpool: allocate: %w", err)
	}
	if len(raw) != page.Size {
		p.giveBack(fr)
		return nil, fmt.Errorf("%w: allocated page is %d bytes, want %d", ErrShortPage, len(raw), page.Size)
	}
	id := raw.ID()
	copy(fr.buf, raw)
	mu, table := p.stripe(id)
	mu.Lock()
	p.mu.Lock()
	stale, ok := table[id]
	if ok && (stale.pins > 0 || stale.state != stReady) {
		p.mu.Unlock()
		mu.Unlock()
		p.giveBack(fr)
		return nil, fmt.Errorf("%w: the source handed out page %d while it is still in use here", ErrPinned, id)
	}
	if ok {
		p.dropLocked(stale, table)
	}
	p.installLocked(fr, id, stReady)
	p.mu.Unlock()
	table[id] = fr
	mu.Unlock()
	return fr, nil
}

func check(raw page.Page, id core.PageID) error {
	if len(raw) != page.Size {
		return fmt.Errorf("%w: page %d is %d bytes, want %d", ErrShortPage, id, len(raw), page.Size)
	}
	if raw.ID() != id {
		return fmt.Errorf("%w: asked for page %d, its header says %d", ErrIDMismatch, id, raw.ID())
	}
	return nil
}

func (p *Pool) grab() (*Frame, error) {
	for {
		p.mu.Lock()
		if n := len(p.free); n > 0 {
			i := p.free[n-1]
			p.free = p.free[:n-1]
			p.mu.Unlock()
			return p.frames[i], nil
		}
		i, ok := p.rep.Evict()
		if !ok {
			p.mu.Unlock()
			return nil, ErrNoFreeFrame
		}
		f := p.frames[i]
		p.seq++
		token, id := p.seq, f.id
		f.claim = token
		p.mu.Unlock()
		won, err := p.evict(f, id, token)
		if err != nil {
			return nil, err
		}
		if won {
			return f, nil
		}
	}
}

func (p *Pool) evict(f *Frame, id core.PageID, token uint64) (bool, error) {
	mu, table := p.stripe(id)
	mu.Lock()
	p.mu.Lock()
	if f.claim != token {
		p.mu.Unlock()
		mu.Unlock()
		return false, nil
	}
	f.claim = 0
	if !f.dirty {
		delete(table, id)
		f.state = stFree
		p.mu.Unlock()
		mu.Unlock()
		p.evictions.Add(1)
		return true, nil
	}
	done := make(chan struct{})
	f.state, f.done = stTransit, done
	p.mu.Unlock()
	mu.Unlock()

	err := p.flushFrame(f)

	mu.Lock()
	p.mu.Lock()
	f.done = nil
	if err != nil {
		f.state = stReady
		p.rep.Access(f.index)
		p.rep.SetEvictable(f.index, true)
	} else {
		delete(table, id)
		f.state = stFree
	}
	p.mu.Unlock()
	mu.Unlock()
	close(done)
	if err != nil {
		return false, fmt.Errorf("evict page %d: %w", id, err)
	}
	p.evictions.Add(1)
	return true, nil
}

func (p *Pool) flushFrame(f *Frame) error {
	f.latch.RLock()
	defer f.latch.RUnlock()
	p.mu.Lock()
	dirty, id := f.dirty, f.id
	p.mu.Unlock()
	if !dirty {
		return nil
	}
	img := append(page.Page(nil), f.buf...)
	if img.ID() != id {
		return fmt.Errorf("%w: frame holds page %d, its header now says %d", ErrIDMismatch, id, img.ID())
	}
	if err := p.src.Write(img); err != nil {
		return fmt.Errorf("scutedb/bufpool: write page %d: %w", id, err)
	}
	p.writes.Add(1)
	p.mu.Lock()
	f.dirty = false
	p.mu.Unlock()
	return nil
}

func (p *Pool) markDirty(f *Frame) {
	p.mu.Lock()
	f.dirty = true
	p.mu.Unlock()
}

func (p *Pool) Unpin(f *Frame, dirty bool) error {
	if f == nil || f.pool != p {
		return ErrNotPinned
	}
	p.mu.Lock()
	if f.state != stReady || f.pins <= 0 {
		p.mu.Unlock()
		return ErrNotPinned
	}
	if dirty {
		f.dirty = true
	}
	through := p.opts.Mode == WriteThrough && f.dirty
	if !through {
		p.unpinLocked(f)
	}
	p.mu.Unlock()
	if !through {
		return nil
	}
	err := p.flushFrame(f)
	p.mu.Lock()
	p.unpinLocked(f)
	p.mu.Unlock()
	return err
}

func (p *Pool) Flush(id core.PageID) error {
	mu, table := p.stripe(id)
	for {
		mu.RLock()
		f, ok := table[id]
		if !ok {
			mu.RUnlock()
			return nil
		}
		if f.state != stReady {
			done := f.done
			mu.RUnlock()
			p.wait(id, done)
			continue
		}
		p.mu.Lock()
		if !f.dirty {
			p.mu.Unlock()
			mu.RUnlock()
			return nil
		}
		p.pinLocked(f, false)
		p.mu.Unlock()
		mu.RUnlock()
		err := p.flushFrame(f)
		p.mu.Lock()
		p.unpinLocked(f)
		p.mu.Unlock()
		return err
	}
}

func (p *Pool) FlushAll() error {
	p.mu.Lock()
	var ids []core.PageID
	for _, f := range p.frames {
		if f.dirty {
			ids = append(ids, f.id)
		}
	}
	p.mu.Unlock()
	sort.Slice(ids, func(a, b int) bool { return ids[a] < ids[b] })
	var errs []error
	for _, id := range ids {
		if err := p.Flush(id); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (p *Pool) Free(id core.PageID) error {
	mu, table := p.stripe(id)
	for {
		mu.Lock()
		f, ok := table[id]
		if ok && f.state != stReady {
			done := f.done
			mu.Unlock()
			p.wait(id, done)
			continue
		}
		claimed := false
		if ok {
			p.mu.Lock()
			if f.pins > 0 {
				n := f.pins
				p.mu.Unlock()
				mu.Unlock()
				return fmt.Errorf("%w: cannot free page %d, it has %d pins", ErrPinned, id, n)
			}
			claimed = f.claim != 0
			f.claim = 0
			p.mu.Unlock()
		}
		err := p.src.Free(id)
		if ok {
			p.mu.Lock()
			if err == nil {
				p.dropLocked(f, table)
			} else if claimed {
				p.rep.Access(f.index)
				p.rep.SetEvictable(f.index, true)
			}
			p.mu.Unlock()
		}
		mu.Unlock()
		if err != nil {
			return fmt.Errorf("scutedb/bufpool: free page %d: %w", id, err)
		}
		return nil
	}
}

func (p *Pool) dropLocked(f *Frame, table map[core.PageID]*Frame) {
	delete(table, f.id)
	p.rep.Remove(f.index)
	f.state = stFree
	f.dirty = false
	f.claim = 0
	p.free = append(p.free, f.index)
}
