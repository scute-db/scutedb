package bufpool

import (
	"errors"
	"fmt"
	"sort"

	"github.com/scute-db/scutedb/internal/core"
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
	DefaultK  = 2
	MaxK      = 64
	MaxFrames = 1 << 20
)

type Options struct {
	Frames int
	Policy Policy
	K      int
	Mode   Mode
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

type Frame struct {
	pool     *Pool
	index    int
	id       core.PageID
	buf      page.Page
	pins     int
	dirty    bool
	used     bool
	verified bool
}

func (f *Frame) ID() core.PageID { return f.id }
func (f *Frame) Page() page.Page { return f.buf }
func (f *Frame) Pins() int       { return f.pins }
func (f *Frame) Dirty() bool     { return f.dirty }
func (f *Frame) Verified() bool  { return f.verified }
func (f *Frame) MarkVerified()   { f.verified = true }

type Pool struct {
	src    Source
	opts   Options
	frames []*Frame
	table  map[core.PageID]int
	free   []int
	rep    Replacer
	stats  Stats
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

	p := &Pool{
		src:    src,
		opts:   opts,
		frames: make([]*Frame, opts.Frames),
		table:  make(map[core.PageID]int, opts.Frames),
		free:   make([]int, 0, opts.Frames),
		rep:    rep,
	}
	slab := make([]byte, opts.Frames*page.Size)
	for i := range p.frames {
		p.frames[i] = &Frame{pool: p, index: i, buf: page.Page(slab[i*page.Size : (i+1)*page.Size : (i+1)*page.Size])}
	}
	for i := opts.Frames - 1; i >= 0; i-- {
		p.free = append(p.free, i)
	}
	return p, nil
}

func (p *Pool) Options() Options  { return p.opts }
func (p *Pool) Stats() Stats      { return p.stats }
func (p *Pool) ResetStats()       { p.stats = Stats{} }
func (p *Pool) HitRatio() float64 { return p.stats.HitRatio() }
func (p *Pool) Frames() int       { return len(p.frames) }
func (p *Pool) Resident() int     { return len(p.table) }

func (p *Pool) Contains(id core.PageID) bool {
	_, ok := p.table[id]
	return ok
}

func (p *Pool) Pinned() int {
	n := 0
	for _, f := range p.frames {
		if f.pins > 0 {
			n++
		}
	}
	return n
}

func (p *Pool) Fetch(id core.PageID) (*Frame, error) {
	if i, ok := p.table[id]; ok {
		f := p.frames[i]
		p.pin(f)
		p.stats.Hits++
		return f, nil
	}
	p.stats.Misses++
	i, err := p.grab()
	if err != nil {
		return nil, fmt.Errorf("scutedb/bufpool: fetch page %d: %w", id, err)
	}
	raw, err := p.src.Read(id)
	if err != nil {
		p.free = append(p.free, i)
		return nil, fmt.Errorf("scutedb/bufpool: read page %d: %w", id, err)
	}
	p.stats.Reads++
	if err := check(raw, id); err != nil {
		p.free = append(p.free, i)
		return nil, err
	}
	return p.install(i, id, raw), nil
}

func (p *Pool) NewPage(kind page.Kind) (*Frame, error) {
	i, err := p.grab()
	if err != nil {
		return nil, fmt.Errorf("scutedb/bufpool: new page: %w", err)
	}
	raw, err := p.src.Allocate(kind)
	if err != nil {
		p.free = append(p.free, i)
		return nil, fmt.Errorf("scutedb/bufpool: allocate: %w", err)
	}
	if len(raw) != page.Size {
		p.free = append(p.free, i)
		return nil, fmt.Errorf("%w: allocated page is %d bytes, want %d", ErrShortPage, len(raw), page.Size)
	}
	id := raw.ID()
	if old, ok := p.table[id]; ok {
		stale := p.frames[old]
		if stale.pins > 0 {
			p.free = append(p.free, i)
			return nil, fmt.Errorf("%w: the source handed out page %d while it is still pinned here", ErrPinned, id)
		}
		p.drop(stale)
	}
	return p.install(i, id, raw), nil
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

func (p *Pool) install(i int, id core.PageID, raw page.Page) *Frame {
	f := p.frames[i]
	copy(f.buf, raw)
	f.id = id
	f.used = true
	f.dirty = false
	f.verified = false
	f.pins = 0
	p.table[id] = i
	p.pin(f)
	return f
}

func (p *Pool) pin(f *Frame) {
	f.pins++
	p.rep.Access(f.index)
	p.rep.SetEvictable(f.index, false)
}

func (p *Pool) grab() (int, error) {
	if n := len(p.free); n > 0 {
		i := p.free[n-1]
		p.free = p.free[:n-1]
		return i, nil
	}
	i, ok := p.rep.Evict()
	if !ok {
		return 0, ErrNoFreeFrame
	}
	f := p.frames[i]
	if f.dirty {
		if err := p.write(f); err != nil {
			p.rep.Access(i)
			p.rep.SetEvictable(i, true)
			return 0, fmt.Errorf("evict page %d: %w", f.id, err)
		}
	}
	delete(p.table, f.id)
	f.used = false
	f.verified = false
	p.stats.Evictions++
	return i, nil
}

func (p *Pool) write(f *Frame) error {
	if f.buf.ID() != f.id {
		return fmt.Errorf("%w: frame holds page %d, its header now says %d", ErrIDMismatch, f.id, f.buf.ID())
	}
	if err := p.src.Write(f.buf); err != nil {
		return fmt.Errorf("scutedb/bufpool: write page %d: %w", f.id, err)
	}
	p.stats.Writes++
	f.dirty = false
	return nil
}

func (p *Pool) mine(f *Frame) bool {
	if f == nil || f.pool != p || !f.used {
		return false
	}
	i, ok := p.table[f.id]
	return ok && i == f.index
}

func (p *Pool) Unpin(f *Frame, dirty bool) error {
	if !p.mine(f) || f.pins <= 0 {
		return ErrNotPinned
	}
	if dirty {
		f.dirty = true
	}
	f.pins--
	if f.pins == 0 {
		p.rep.SetEvictable(f.index, true)
	}
	if p.opts.Mode == WriteThrough && f.dirty {
		return p.write(f)
	}
	return nil
}

func (p *Pool) Flush(id core.PageID) error {
	i, ok := p.table[id]
	if !ok {
		return nil
	}
	f := p.frames[i]
	if !f.dirty {
		return nil
	}
	return p.write(f)
}

func (p *Pool) FlushAll() error {
	var dirty []*Frame
	for _, f := range p.frames {
		if f.used && f.dirty {
			dirty = append(dirty, f)
		}
	}
	sort.Slice(dirty, func(a, b int) bool { return dirty[a].id < dirty[b].id })
	var errs []error
	for _, f := range dirty {
		if err := p.write(f); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (p *Pool) Free(id core.PageID) error {
	if i, ok := p.table[id]; ok && p.frames[i].pins > 0 {
		return fmt.Errorf("%w: cannot free page %d, it has %d pins", ErrPinned, id, p.frames[i].pins)
	}
	if err := p.src.Free(id); err != nil {
		return fmt.Errorf("scutedb/bufpool: free page %d: %w", id, err)
	}
	if i, ok := p.table[id]; ok {
		p.drop(p.frames[i])
	}
	return nil
}

func (p *Pool) drop(f *Frame) {
	delete(p.table, f.id)
	p.rep.Remove(f.index)
	f.used = false
	f.dirty = false
	f.verified = false
	f.pins = 0
	p.free = append(p.free, f.index)
}
