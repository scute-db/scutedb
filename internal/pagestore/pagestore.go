package pagestore

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"github.com/scute-db/scutedb/internal/core"
	"github.com/scute-db/scutedb/internal/fileio"
	"github.com/scute-db/scutedb/internal/page"
)

const (
	Version    = 1
	MetaPage   = core.PageID(0)
	NoPage     = core.PageID(0)
	ChunkPages = 16
	MaxPages   = uint32(0xFFFFFFFF)
)

var Magic = [8]byte{'S', 'C', 'U', 'T', 'E', 'D', 'B', 0x00}

const (
	offMagic      = page.HeaderSize
	offVersion    = offMagic + 8
	offPageSize   = offVersion + 4
	offRoot       = offPageSize + 4
	offFreeList   = offRoot + 4
	offFreeCount  = offFreeList + 4
	offNextPage   = offFreeCount + 4
	offFilePages  = offNextPage + 4
	offCheckpoint = offFilePages + 4
	MetaSize      = offCheckpoint + 4 - page.HeaderSize

	offFreeListNext  = page.HeaderSize
	FreeListHeader   = page.HeaderSize + 4
	FreeListCapacity = (page.Size - FreeListHeader) / 4
)

var (
	ErrNotScuteDB   = errors.New("scutedb/pagestore: not a scutedb file")
	ErrBadVersion   = errors.New("scutedb/pagestore: unsupported format version")
	ErrBadPageSize  = errors.New("scutedb/pagestore: page size does not match this build")
	ErrBadPageID    = errors.New("scutedb/pagestore: page id out of range")
	ErrFreeMeta     = errors.New("scutedb/pagestore: the meta page cannot be freed")
	ErrFreeRoot     = errors.New("scutedb/pagestore: the root cannot be freed while it is the root")
	ErrDoubleFree   = errors.New("scutedb/pagestore: page is already free")
	ErrNotAllocated = errors.New("scutedb/pagestore: page is not allocated")
	ErrCorrupt      = errors.New("scutedb/pagestore: structure is inconsistent")
	ErrClosed       = errors.New("scutedb/pagestore: store is closed")
	ErrPoisoned     = errors.New("scutedb/pagestore: an fsync failed; reopen the file to recover")
	ErrFull         = errors.New("scutedb/pagestore: no page ids left")
)

type Meta struct {
	Version      uint32
	PageSize     uint32
	Root         core.PageID
	FreeListPage core.PageID
	FreeCount    uint32
	NextPage     core.PageID
	FilePages    uint32
	Checkpoint   uint32
}

type Stats struct {
	Grown       int
	Allocated   int
	Reused      int
	Freed       int
	Checkpoints int
}

type pageState uint8

const (
	stFree pageState = iota + 1
	stPending
	stFreeList
)

func (st pageState) String() string {
	switch st {
	case stFree:
		return "free"
	case stPending:
		return "pending"
	case stFreeList:
		return "a free-list page"
	}
	return "unknown"
}

type Store struct {
	pf      *page.File
	durable Meta

	root      core.PageID
	nextPage  core.PageID
	filePages uint32

	free     []core.PageID
	pending  []core.PageID
	listPage []core.PageID
	status   map[core.PageID]pageState

	dirty    bool
	closed   bool
	poisoned error
	stats    Stats
}

func Create(path string) (*Store, error) {
	f, err := fileio.Open(path)
	if err != nil {
		return nil, err
	}
	blank, err := isBlank(f)
	if err != nil {
		f.Close()
		return nil, err
	}
	if !blank {
		ours := hasMagic(f)
		f.Close()
		if ours {
			return nil, fmt.Errorf("scutedb/pagestore: %s already holds a scutedb store; use Open", path)
		}
		return nil, fmt.Errorf("scutedb/pagestore: %s already holds data that is not a scutedb store", path)
	}
	s, err := create(page.NewFile(f))
	if err != nil {
		f.Close()
		return nil, err
	}
	if err := fileio.SyncParent(path); err != nil {
		s.Discard()
		return nil, err
	}
	return s, nil
}

func hasMagic(f fileio.File) bool {
	var b [8]byte
	n, _ := f.ReadAt(b[:], offMagic)
	return n == len(b) && bytes.Equal(b[:], Magic[:])
}

func Open(path string) (*Store, error) {
	f, err := fileio.OpenExisting(path)
	if err != nil {
		return nil, err
	}
	s, _, err := openFile(f)
	if err != nil {
		f.Close()
		return nil, err
	}
	if err := fileio.SyncParent(path); err != nil {
		s.Discard()
		return nil, err
	}
	return s, nil
}

func openFile(f fileio.File) (*Store, bool, error) {
	blank, err := isBlank(f)
	if err != nil {
		return nil, false, err
	}
	pf := page.NewFile(f)
	if blank {
		s, err := create(pf)
		return s, true, err
	}
	size, err := f.Size()
	if err != nil {
		return nil, false, err
	}
	if size < page.Size {
		if hasMagic(f) {
			return nil, false, fmt.Errorf("%w: a scutedb file truncated to %d bytes", ErrCorrupt, size)
		}
		return nil, false, fmt.Errorf("%w: file is %d bytes, smaller than one page", ErrNotScuteDB, size)
	}
	s, err := load(pf)
	return s, false, err
}

func isBlank(f fileio.File) (bool, error) {
	size, err := f.Size()
	if err != nil {
		return false, err
	}
	if size == 0 {
		return true, nil
	}
	if size > int64(ChunkPages)*page.Size {
		return false, nil
	}
	buf := make([]byte, size)
	n, err := f.ReadAt(buf, 0)
	if int64(n) != size {
		if err == nil {
			err = io.ErrUnexpectedEOF
		}
		return false, err
	}
	for _, b := range buf {
		if b != 0 {
			return false, nil
		}
	}
	return true, nil
}

func create(pf *page.File) (*Store, error) {
	if err := pf.Grow(0, ChunkPages); err != nil {
		return nil, err
	}
	s := &Store{
		pf:        pf,
		root:      NoPage,
		nextPage:  1,
		filePages: ChunkPages,
		status:    map[core.PageID]pageState{},
	}
	s.stats.Grown++
	if err := s.Checkpoint(); err != nil {
		return nil, err
	}
	return s, nil
}

func load(pf *page.File) (*Store, error) {
	raw, err := pf.Read(MetaPage)
	if err != nil {
		return nil, err
	}
	m, err := decodeMeta(raw)
	if err != nil {
		return nil, err
	}
	onDisk, err := pf.PageCount()
	if err != nil {
		return nil, err
	}
	if err := checkMeta(m, onDisk); err != nil {
		return nil, err
	}
	entries, lists, err := readFreeList(pf, m)
	if err != nil {
		return nil, err
	}
	if err := checkFreeSet(m, entries, lists); err != nil {
		return nil, err
	}

	s := &Store{
		pf:        pf,
		durable:   m,
		root:      m.Root,
		nextPage:  m.NextPage,
		filePages: m.FilePages,
		free:      entries,
		listPage:  lists,
		status:    make(map[core.PageID]pageState, len(entries)+len(lists)),
	}
	for _, id := range entries {
		s.status[id] = stFree
	}
	for _, id := range lists {
		s.status[id] = stFreeList
	}
	if err := s.verifyLive(onDisk, entries, lists); err != nil {
		return nil, err
	}

	if err := pf.Write(encodeMeta(m)); err != nil {
		return nil, err
	}
	if err := pf.Sync(); err != nil {
		return nil, err
	}
	return s, nil
}

func decodeMeta(p page.Page) (Meta, error) {
	var m Meta
	if !bytes.Equal(p[offMagic:offMagic+8], Magic[:]) {
		return m, fmt.Errorf("%w: bytes 16..23 are % 02X, want % 02X",
			ErrNotScuteDB, p[offMagic:offMagic+8], Magic)
	}
	m.Version = binary.BigEndian.Uint32(p[offVersion:])
	if m.Version != Version {
		return m, fmt.Errorf("%w: file is version %d, this build reads version %d",
			ErrBadVersion, m.Version, Version)
	}
	m.PageSize = binary.BigEndian.Uint32(p[offPageSize:])
	if m.PageSize != page.Size {
		return m, fmt.Errorf("%w: file uses %d-byte pages, this build uses %d",
			ErrBadPageSize, m.PageSize, page.Size)
	}
	if p.Kind() != page.KindMeta {
		return m, fmt.Errorf("%w: page 0 is kind %s, want meta", ErrCorrupt, p.Kind())
	}
	if p.ID() != MetaPage {
		return m, fmt.Errorf("%w: page 0 says it is page %d", ErrCorrupt, p.ID())
	}
	m.Root = core.PageID(binary.BigEndian.Uint32(p[offRoot:]))
	m.FreeListPage = core.PageID(binary.BigEndian.Uint32(p[offFreeList:]))
	m.FreeCount = binary.BigEndian.Uint32(p[offFreeCount:])
	m.NextPage = core.PageID(binary.BigEndian.Uint32(p[offNextPage:]))
	m.FilePages = binary.BigEndian.Uint32(p[offFilePages:])
	m.Checkpoint = binary.BigEndian.Uint32(p[offCheckpoint:])
	return m, nil
}

func encodeMeta(m Meta) page.Page {
	p := page.New(MetaPage, page.KindMeta)
	copy(p[offMagic:], Magic[:])
	binary.BigEndian.PutUint32(p[offVersion:], m.Version)
	binary.BigEndian.PutUint32(p[offPageSize:], m.PageSize)
	binary.BigEndian.PutUint32(p[offRoot:], uint32(m.Root))
	binary.BigEndian.PutUint32(p[offFreeList:], uint32(m.FreeListPage))
	binary.BigEndian.PutUint32(p[offFreeCount:], m.FreeCount)
	binary.BigEndian.PutUint32(p[offNextPage:], uint32(m.NextPage))
	binary.BigEndian.PutUint32(p[offFilePages:], m.FilePages)
	binary.BigEndian.PutUint32(p[offCheckpoint:], m.Checkpoint)
	p.SetFreeStart(uint16(page.HeaderSize + MetaSize))
	return p
}

func checkMeta(m Meta, pagesOnDisk uint32) error {
	if m.NextPage == 0 {
		return fmt.Errorf("%w: next page is 0, which is the meta page", ErrCorrupt)
	}
	if uint32(m.NextPage) > m.FilePages {
		return fmt.Errorf("%w: %d pages handed out but the meta says the file holds %d",
			ErrCorrupt, m.NextPage, m.FilePages)
	}
	if pagesOnDisk < m.FilePages {
		return fmt.Errorf("%w: meta says the file holds %d pages, it actually holds %d",
			ErrCorrupt, m.FilePages, pagesOnDisk)
	}
	if m.Root != NoPage && uint32(m.Root) >= uint32(m.NextPage) {
		return fmt.Errorf("%w: root is page %d, only %d handed out", ErrCorrupt, m.Root, m.NextPage)
	}
	return nil
}

func readFreeList(pf *page.File, m Meta) ([]core.PageID, []core.PageID, error) {
	var entries, lists []core.PageID
	seen := map[core.PageID]bool{}
	for id := m.FreeListPage; id != NoPage; {
		if uint32(id) >= uint32(m.NextPage) {
			return nil, nil, fmt.Errorf("%w: free list reaches page %d, only %d handed out",
				ErrCorrupt, id, m.NextPage)
		}
		if seen[id] {
			return nil, nil, fmt.Errorf("%w: free-list page %d is reached twice, so the chain loops",
				ErrCorrupt, id)
		}
		seen[id] = true
		p, err := pf.Read(id)
		if err != nil {
			return nil, nil, err
		}
		if p.Kind() != page.KindFreeList {
			return nil, nil, fmt.Errorf("%w: page %d is in the free-list chain but its kind is %s",
				ErrCorrupt, id, p.Kind())
		}
		if p.ID() != id {
			return nil, nil, fmt.Errorf("%w: free-list page read from slot %d says it is page %d",
				ErrCorrupt, id, p.ID())
		}
		n := int(p.ItemCount())
		if n > FreeListCapacity {
			return nil, nil, fmt.Errorf("%w: free-list page %d claims %d entries, capacity is %d",
				ErrCorrupt, id, n, FreeListCapacity)
		}
		for i := 0; i < n; i++ {
			entries = append(entries, core.PageID(binary.BigEndian.Uint32(p[FreeListHeader+4*i:])))
		}
		lists = append(lists, id)
		id = core.PageID(binary.BigEndian.Uint32(p[offFreeListNext:]))
	}
	if uint32(len(entries)) != m.FreeCount {
		return nil, nil, fmt.Errorf("%w: free list holds %d entries, meta says %d",
			ErrCorrupt, len(entries), m.FreeCount)
	}
	return entries, lists, nil
}

func checkFreeSet(m Meta, entries, lists []core.PageID) error {
	seen := make(map[core.PageID]string, len(entries)+len(lists))
	for _, id := range lists {
		if id == m.Root && m.Root != NoPage {
			return fmt.Errorf("%w: the root page %d is also a free-list page", ErrCorrupt, id)
		}
		seen[id] = "a free-list page"
	}
	for _, id := range entries {
		if id == MetaPage || uint32(id) >= uint32(m.NextPage) {
			return fmt.Errorf("%w: free list names page %d, outside 1..%d", ErrCorrupt, id, m.NextPage-1)
		}
		if what, dup := seen[id]; dup {
			return fmt.Errorf("%w: page %d is on the free list and is also %s", ErrCorrupt, id, what)
		}
		if id == m.Root {
			return fmt.Errorf("%w: the root page %d is on the free list", ErrCorrupt, id)
		}
		seen[id] = "already on the free list"
	}
	return nil
}

func pagesFor(entries int) int {
	return (entries + FreeListCapacity - 1) / FreeListCapacity
}

func growTo(filePages uint32) uint32 {
	g := uint64(filePages) + ChunkPages
	if g > uint64(MaxPages) {
		g = uint64(MaxPages)
	}
	return uint32(g)
}

type mustPoison struct{ err error }

func (e mustPoison) Error() string { return e.err.Error() }

func (s *Store) poison(err error) error {
	if s.poisoned == nil {
		s.poisoned = err
	}
	return fmt.Errorf("%w: %w", ErrPoisoned, err)
}

func (s *Store) usable() error {
	if s.poisoned != nil {
		return fmt.Errorf("%w: %w", ErrPoisoned, s.poisoned)
	}
	if s.closed {
		return ErrClosed
	}
	return nil
}

func (s *Store) Durable() Meta { return s.durable }
func (s *Store) Stats() Stats  { return s.stats }
func (s *Store) Dirty() bool   { return s.dirty }

func (s *Store) Root() core.PageID { return s.root }

func (s *Store) Meta() Meta {
	m := s.durable
	m.Root = s.root
	m.NextPage = s.nextPage
	m.FilePages = s.filePages
	m.FreeCount = uint32(len(s.free))
	return m
}

func (s *Store) FreePages() []core.PageID    { return append([]core.PageID(nil), s.free...) }
func (s *Store) PendingPages() []core.PageID { return append([]core.PageID(nil), s.pending...) }
func (s *Store) ListPages() []core.PageID    { return append([]core.PageID(nil), s.listPage...) }

func (s *Store) inRange(id core.PageID) error {
	if id == MetaPage || uint32(id) >= uint32(s.nextPage) {
		return fmt.Errorf("%w: page %d, pages 1..%d are handed out", ErrBadPageID, id, s.nextPage-1)
	}
	return nil
}

func (s *Store) owned(id core.PageID) error {
	if err := s.inRange(id); err != nil {
		return err
	}
	if st, ok := s.status[id]; ok {
		return fmt.Errorf("%w: page %d is %s", ErrNotAllocated, id, st)
	}
	return nil
}

func (s *Store) SetRoot(id core.PageID) error {
	if err := s.usable(); err != nil {
		return err
	}
	if id != NoPage {
		if err := s.owned(id); err != nil {
			return err
		}
	}
	s.root = id
	s.dirty = true
	return nil
}

func (s *Store) Read(id core.PageID) (page.Page, error) {
	if err := s.usable(); err != nil {
		return nil, err
	}
	if err := s.owned(id); err != nil {
		return nil, err
	}
	return s.pf.Read(id)
}

func (s *Store) Write(p page.Page) error {
	if err := s.usable(); err != nil {
		return err
	}
	if err := s.owned(p.ID()); err != nil {
		return err
	}
	if err := s.pf.Write(p); err != nil {
		return err
	}
	s.dirty = true
	return nil
}

func (s *Store) Allocate(kind page.Kind) (page.Page, error) {
	if err := s.usable(); err != nil {
		return nil, err
	}

	reused := len(s.free) > 0
	filePages, grown := s.filePages, s.stats.Grown
	var id core.PageID
	if reused {
		id = s.free[len(s.free)-1]
		s.free = s.free[:len(s.free)-1]
		delete(s.status, id)
	} else {
		id = s.nextPage
		if uint32(id) >= MaxPages {
			return nil, ErrFull
		}
		if uint32(id) >= s.filePages {
			grown := growTo(s.filePages)
			if grown <= uint32(id) {
				return nil, ErrFull
			}
			if err := s.pf.Grow(s.filePages, grown); err != nil {
				return nil, err
			}
			s.filePages = grown
			s.stats.Grown++
		}
		s.nextPage = id + 1
	}

	fresh := page.New(id, kind)
	if err := s.pf.Write(fresh); err != nil {
		if reused {
			s.free = append(s.free, id)
			s.status[id] = stFree
		} else {
			s.nextPage = id
			s.filePages = filePages
			s.stats.Grown = grown
		}
		return nil, err
	}
	if reused {
		s.stats.Reused++
	} else {
		s.stats.Allocated++
	}
	s.dirty = true
	return fresh, nil
}

func (s *Store) Free(id core.PageID) error {
	if err := s.usable(); err != nil {
		return err
	}
	if id == MetaPage {
		return ErrFreeMeta
	}
	if err := s.inRange(id); err != nil {
		return err
	}
	if st, ok := s.status[id]; ok {
		if st == stFreeList {
			return fmt.Errorf("%w: page %d is %s", ErrNotAllocated, id, st)
		}
		return fmt.Errorf("%w: page %d is already %s", ErrDoubleFree, id, st)
	}
	if id == s.root {
		return fmt.Errorf("%w: page %d", ErrFreeRoot, id)
	}
	s.pending = append(s.pending, id)
	s.status[id] = stPending
	s.dirty = true
	s.stats.Freed++
	return nil
}

func (s *Store) Sync() error {
	if err := s.usable(); err != nil {
		return err
	}
	if err := s.pf.Sync(); err != nil {
		return s.poison(err)
	}
	return nil
}

func (s *Store) Checkpoint() error {
	if err := s.usable(); err != nil {
		return err
	}
	if err := s.checkpoint(); err != nil {
		var fatal mustPoison
		if errors.As(err, &fatal) {
			return s.poison(fatal.err)
		}
		return err
	}
	return nil
}

func (s *Store) checkpoint() error {
	if err := s.pf.Sync(); err != nil {
		return mustPoison{err}
	}

	free := append([]core.PageID(nil), s.free...)
	nextPage, filePages := s.nextPage, s.filePages
	grew := 0

	count := func() int { return len(free) + len(s.pending) + len(s.listPage) }
	var lists []core.PageID
	for pagesFor(count()) > len(lists) {
		var id core.PageID
		if n := len(free); n > 0 {
			id = free[n-1]
			free = free[:n-1]
		} else {
			id = nextPage
			if uint32(id) >= MaxPages {
				return ErrFull
			}
			if uint32(id) >= filePages {
				grown := growTo(filePages)
				if grown <= uint32(id) {
					return ErrFull
				}
				if err := s.pf.Grow(filePages, grown); err != nil {
					return err
				}
				filePages = grown
				grew++
			}
			nextPage = id + 1
		}
		lists = append(lists, id)
	}

	entries := make([]core.PageID, 0, count())
	entries = append(entries, s.listPage...)
	entries = append(entries, free...)
	entries = append(entries, s.pending...)

	for i, id := range lists {
		p := page.New(id, page.KindFreeList)
		next := NoPage
		if i+1 < len(lists) {
			next = lists[i+1]
		}
		binary.BigEndian.PutUint32(p[offFreeListNext:], uint32(next))
		lo := i * FreeListCapacity
		hi := lo + FreeListCapacity
		if hi > len(entries) {
			hi = len(entries)
		}
		for j, e := range entries[lo:hi] {
			binary.BigEndian.PutUint32(p[FreeListHeader+4*j:], uint32(e))
		}
		p.SetItemCount(uint16(hi - lo))
		p.SetFreeStart(uint16(FreeListHeader + 4*(hi-lo)))
		if err := s.pf.Write(p); err != nil {
			return err
		}
	}
	if len(lists) > 0 {
		if err := s.pf.Sync(); err != nil {
			return mustPoison{err}
		}
	}

	head := NoPage
	if len(lists) > 0 {
		head = lists[0]
	}
	next := Meta{
		Version:      Version,
		PageSize:     page.Size,
		Root:         s.root,
		FreeListPage: head,
		FreeCount:    uint32(len(entries)),
		NextPage:     nextPage,
		FilePages:    filePages,
		Checkpoint:   s.durable.Checkpoint + 1,
	}
	if err := s.pf.Write(encodeMeta(next)); err != nil {
		return mustPoison{err}
	}
	if err := s.pf.Sync(); err != nil {
		return mustPoison{err}
	}

	s.durable = next
	s.nextPage = nextPage
	s.filePages = filePages
	s.free = entries
	s.pending = nil
	s.listPage = lists
	s.status = make(map[core.PageID]pageState, len(entries)+len(lists))
	for _, id := range entries {
		s.status[id] = stFree
	}
	for _, id := range lists {
		s.status[id] = stFreeList
	}
	s.dirty = false
	s.stats.Grown += grew
	s.stats.Checkpoints++
	return nil
}

func (s *Store) Close() error {
	if s.closed {
		return nil
	}
	var err error
	if s.poisoned == nil && s.dirty {
		err = s.Checkpoint()
	}
	s.closed = true
	cerr := s.pf.Close()
	if s.poisoned != nil {
		return fmt.Errorf("%w: %w", ErrPoisoned, s.poisoned)
	}
	if err != nil {
		return err
	}
	return cerr
}

func (s *Store) Discard() error {
	if s.closed {
		return nil
	}
	s.closed = true
	return s.pf.Close()
}

func (s *Store) Verify() error {
	if err := s.usable(); err != nil {
		return err
	}

	raw, err := s.pf.Read(MetaPage)
	if err != nil {
		return err
	}
	m, err := decodeMeta(raw)
	if err != nil {
		return err
	}
	if m != s.durable {
		return fmt.Errorf("%w: meta page on disk %+v differs from the last checkpoint %+v",
			ErrCorrupt, m, s.durable)
	}
	onDisk, err := s.pf.PageCount()
	if err != nil {
		return err
	}
	if err := checkMeta(m, onDisk); err != nil {
		return err
	}
	entries, lists, err := readFreeList(s.pf, m)
	if err != nil {
		return err
	}
	if err := checkFreeSet(m, entries, lists); err != nil {
		return err
	}
	return s.verifyLive(onDisk, entries, lists)
}

func (s *Store) verifyLive(pagesOnDisk uint32, entries, lists []core.PageID) error {
	if s.nextPage < s.durable.NextPage {
		return fmt.Errorf("%w: next page went backwards, %d after a checkpoint at %d",
			ErrCorrupt, s.nextPage, s.durable.NextPage)
	}
	if uint32(s.nextPage) > s.filePages || s.filePages > pagesOnDisk {
		return fmt.Errorf("%w: %d pages handed out, %d claimed, %d on disk",
			ErrCorrupt, s.nextPage, s.filePages, pagesOnDisk)
	}
	if s.root != NoPage {
		if err := s.inRange(s.root); err != nil {
			return fmt.Errorf("%w: root: %v", ErrCorrupt, err)
		}
		if st, ok := s.status[s.root]; ok {
			return fmt.Errorf("%w: the root page %d is %s", ErrCorrupt, s.root, st)
		}
	}

	if len(s.listPage) != len(lists) {
		return fmt.Errorf("%w: %d free-list pages in memory, %d on disk", ErrCorrupt, len(s.listPage), len(lists))
	}
	for i := range lists {
		if s.listPage[i] != lists[i] {
			return fmt.Errorf("%w: free-list pages are %v in memory, %v on disk", ErrCorrupt, s.listPage, lists)
		}
	}

	seen := make(map[core.PageID]pageState, len(s.status))
	record := func(ids []core.PageID, want pageState) error {
		for _, id := range ids {
			if err := s.inRange(id); err != nil {
				return fmt.Errorf("%w: a %s page: %v", ErrCorrupt, want, err)
			}
			if prev, dup := seen[id]; dup {
				return fmt.Errorf("%w: page %d is listed as %s and again as %s", ErrCorrupt, id, prev, want)
			}
			seen[id] = want
			if got := s.status[id]; got != want {
				return fmt.Errorf("%w: page %d is listed as %s but marked %s", ErrCorrupt, id, want, got)
			}
		}
		return nil
	}
	if err := record(s.free, stFree); err != nil {
		return err
	}
	if err := record(s.pending, stPending); err != nil {
		return err
	}
	if err := record(s.listPage, stFreeList); err != nil {
		return err
	}
	if len(seen) != len(s.status) {
		for id, st := range s.status {
			if _, ok := seen[id]; !ok {
				return fmt.Errorf("%w: page %d is marked %s but is on no list", ErrCorrupt, id, st)
			}
		}
	}

	durableFree := make(map[core.PageID]bool, len(entries))
	for _, id := range entries {
		durableFree[id] = true
	}
	for _, id := range s.free {
		if !durableFree[id] {
			return fmt.Errorf("%w: page %d is reusable now but was not free at the last checkpoint, "+
				"so overwriting it could damage the durable state", ErrCorrupt, id)
		}
	}
	return nil
}
