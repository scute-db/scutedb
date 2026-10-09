package pagestore

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/scute-db/scutedb/internal/core"
	"github.com/scute-db/scutedb/internal/fileio"
	"github.com/scute-db/scutedb/internal/page"
)

type faultsKind int

const (
	faultsWrite faultsKind = iota
	faultsTorn
	faultsSync
	faultsRead
	faultsSize
	faultsKindCount
)

const faultsSector = 512

var (
	faultsErrWrite = errors.New("faults: injected write failure")
	faultsErrTorn  = errors.New("faults: injected torn write")
	faultsErrRead  = errors.New("faults: injected read failure")
	faultsErrSize  = errors.New("faults: injected size failure")
)

func (k faultsKind) String() string {
	switch k {
	case faultsWrite:
		return "failed write"
	case faultsTorn:
		return "torn write"
	case faultsSync:
		return "failed fsync"
	case faultsRead:
		return "failed read"
	case faultsSize:
		return "failed size"
	}
	return fmt.Sprintf("fault %d", int(k))
}

func (k faultsKind) counter() faultsKind {
	if k == faultsTorn {
		return faultsWrite
	}
	return k
}

func (k faultsKind) cause() error {
	switch k {
	case faultsWrite:
		return faultsErrWrite
	case faultsTorn:
		return faultsErrTorn
	case faultsSync:
		return errSyncFailed
	case faultsRead:
		return faultsErrRead
	}
	return faultsErrSize
}

type faultsFile struct {
	mf          *memFile
	seen        [faultsKindCount]int
	kind        faultsKind
	at          int
	keep        func(int) bool
	tear        int
	fired       bool
	firedAtMeta bool
	metaBefore  bool
	metaWritten bool
}

var _ fileio.File = (*faultsFile)(nil)

func faultsWrap(mf *memFile) *faultsFile { return &faultsFile{mf: mf} }

func (f *faultsFile) reset() { *f = faultsFile{mf: f.mf} }

func (f *faultsFile) arm(kind faultsKind, at int, keep func(int) bool, tear int) {
	f.reset()
	f.kind, f.at, f.keep, f.tear = kind, at, keep, tear
}

func (f *faultsFile) trip(kind faultsKind) bool {
	f.seen[kind]++
	if f.at == 0 || f.fired || f.kind.counter() != kind || f.seen[kind] != f.at {
		return false
	}
	f.fired = true
	return true
}

func faultsTearAt(n, sel int) int {
	sectors := n / faultsSector
	if sectors < 2 {
		return 0
	}
	if sel < 0 {
		return (sectors - 1) * faultsSector
	}
	return (1 + sel%(sectors-1)) * faultsSector
}

func (f *faultsFile) landed(off int64) {
	if off < page.Size {
		f.metaWritten = true
	}
}

func (f *faultsFile) WriteAt(p []byte, off int64) (int, error) {
	if f.trip(faultsWrite) {
		f.metaBefore, f.firedAtMeta = f.metaWritten, off < page.Size
		if f.kind == faultsWrite {
			return 0, faultsErrWrite
		}
		n := faultsTearAt(len(p), f.tear)
		if n > 0 {
			if _, err := f.mf.WriteAt(p[:n], off); err != nil {
				return 0, err
			}
			f.landed(off)
		}
		return n, faultsErrTorn
	}
	n, err := f.mf.WriteAt(p, off)
	if err == nil {
		f.landed(off)
	}
	return n, err
}

func (f *faultsFile) Sync() error {
	if !f.trip(faultsSync) {
		return f.mf.Sync()
	}
	f.mf.failAtSync, f.mf.failKeep = f.mf.syncs+1, f.keep
	err := f.mf.Sync()
	f.mf.failAtSync, f.mf.failKeep = 0, nil
	return err
}

func (f *faultsFile) ReadAt(p []byte, off int64) (int, error) {
	if f.trip(faultsRead) {
		n, _ := f.mf.ReadAt(p[:len(p)/2], off)
		return n, faultsErrRead
	}
	return f.mf.ReadAt(p, off)
}

func (f *faultsFile) Size() (int64, error) {
	if f.trip(faultsSize) {
		return 0, faultsErrSize
	}
	return f.mf.Size()
}

func (f *faultsFile) Close() error { return f.mf.Close() }

func faultsClone(m *memFile) *memFile {
	return &memFile{
		durable: append([]byte(nil), m.durable...),
		live:    append([]byte(nil), m.live...),
		log:     append([]memWrite(nil), m.log...),
		syncs:   m.syncs,
		closed:  m.closed,
	}
}

func faultsSubset(p byte) func(int) bool {
	return func(i int) bool { return (uint(p)>>(uint(i)%8))&1 == 1 }
}

type faultsGen struct {
	inUse map[core.PageID]uint64
	root  core.PageID
}

func faultsEmpty() faultsGen { return faultsGen{inUse: map[core.PageID]uint64{}} }

func (g faultsGen) copy() faultsGen {
	c := faultsGen{inUse: make(map[core.PageID]uint64, len(g.inUse)), root: g.root}
	for id, tag := range g.inUse {
		c.inUse[id] = tag
	}
	return c
}

func (g faultsGen) ids() []core.PageID {
	ids := make([]core.PageID, 0, len(g.inUse))
	for id := range g.inUse {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(a, b int) bool { return ids[a] < ids[b] })
	return ids
}

type faultsModel struct {
	live faultsGen
	gen  uint32
	gens map[uint32]faultsGen
	tag  uint64
}

func faultsNewModel(gen uint32) *faultsModel {
	return &faultsModel{live: faultsEmpty(), gen: gen, gens: map[uint32]faultsGen{gen: faultsEmpty()}}
}

func (m *faultsModel) durable() faultsGen { return m.gens[m.gen] }

func (m *faultsModel) commit(gen uint32) {
	m.gen = gen
	m.gens[gen] = m.live.copy()
}

func (m *faultsModel) revert() { m.live = m.gens[m.gen].copy() }

func (m *faultsModel) clone() *faultsModel {
	c := &faultsModel{live: m.live.copy(), gen: m.gen, tag: m.tag, gens: make(map[uint32]faultsGen, len(m.gens))}
	for g, v := range m.gens {
		c.gens[g] = v.copy()
	}
	return c
}

func faultsGens(allowed map[uint32]faultsGen) []uint32 {
	var out []uint32
	for g := range allowed {
		out = append(out, g)
	}
	sort.Slice(out, func(a, b int) bool { return out[a] < out[b] })
	return out
}

type faultsSnap struct {
	durable Meta
	meta    Meta
	free    []core.PageID
	pending []core.PageID
	lists   []core.PageID
	root    core.PageID
	dirty   bool
}

func faultsSnapOf(s *Store) faultsSnap {
	return faultsSnap{
		durable: s.Durable(),
		meta:    s.Meta(),
		free:    s.FreePages(),
		pending: s.PendingPages(),
		lists:   s.ListPages(),
		root:    s.Root(),
		dirty:   s.Dirty(),
	}
}

func faultsSameIDs(a, b []core.PageID) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func faultsDiff(was, now faultsSnap) string {
	var out []string
	if was.durable != now.durable {
		out = append(out, fmt.Sprintf("Durable() went from %+v to %+v", was.durable, now.durable))
	}
	if was.meta != now.meta {
		out = append(out, fmt.Sprintf("Meta() went from %+v to %+v", was.meta, now.meta))
	}
	if !faultsSameIDs(was.free, now.free) {
		out = append(out, fmt.Sprintf("FreePages() went from %v to %v", was.free, now.free))
	}
	if !faultsSameIDs(was.pending, now.pending) {
		out = append(out, fmt.Sprintf("PendingPages() went from %v to %v", was.pending, now.pending))
	}
	if !faultsSameIDs(was.lists, now.lists) {
		out = append(out, fmt.Sprintf("ListPages() went from %v to %v", was.lists, now.lists))
	}
	if was.root != now.root {
		out = append(out, fmt.Sprintf("Root() went from %d to %d", was.root, now.root))
	}
	if was.dirty != now.dirty {
		out = append(out, fmt.Sprintf("Dirty() went from %v to %v", was.dirty, now.dirty))
	}
	return strings.Join(out, "; ")
}

func faultsCheck(t *testing.T, where string, s *Store, want faultsGen, tags bool) {
	t.Helper()
	if err := s.Verify(); err != nil {
		t.Fatalf("%s: Verify rejected a reachable state: %v", where, err)
	}
	if s.Root() != want.root {
		t.Fatalf("%s: the root is page %d, want %d", where, s.Root(), want.root)
	}
	next := s.Meta().NextPage
	owner := make(map[core.PageID]string, int(next))
	claim := func(ids []core.PageID, as string) {
		for _, id := range ids {
			if id == MetaPage || uint32(id) >= uint32(next) {
				t.Fatalf("%s: page %d is %s, but only pages 1..%d are handed out", where, id, as, next-1)
			}
			if prev, dup := owner[id]; dup {
				t.Fatalf("%s: page %d is both %s and %s", where, id, prev, as)
			}
			owner[id] = as
		}
	}
	ids := want.ids()
	claim(ids, "in use")
	claim(s.FreePages(), "free")
	claim(s.PendingPages(), "pending")
	claim(s.ListPages(), "a free-list page")
	for id := core.PageID(1); id < next; id++ {
		if _, ok := owner[id]; !ok {
			t.Fatalf("%s: page %d was handed out and is now in no state at all, so it leaked", where, id)
		}
	}
	if !tags {
		return
	}
	for _, id := range ids {
		p, err := s.Read(id)
		if err != nil {
			t.Fatalf("%s: reading in-use page %d: %v", where, id, err)
		}
		if got := binary.BigEndian.Uint64(p[page.HeaderSize:]); p.ID() != id || got != want.inUse[id] {
			t.Fatalf("%s: page %d says it is page %d and holds tag %d, want tag %d",
				where, id, p.ID(), got, want.inUse[id])
		}
	}
}

func faultsJudge(t *testing.T, where string, kind faultsKind, fired bool, err error) {
	t.Helper()
	if !fired {
		t.Fatalf("%s: the fault never fired, so the operation did different I/O than its unfaulted run", where)
	}
	if err == nil {
		t.Fatalf("%s: the operation succeeded, so the injected %s was swallowed", where, kind)
	}
	if !errors.Is(err, kind.cause()) {
		t.Fatalf("%s: got %v, which does not carry the injected %q", where, err, kind.cause())
	}
	for _, wrong := range []error{ErrCorrupt, ErrNotScuteDB, ErrBadVersion, ErrBadPageSize, ErrFull, core.ErrShortPage} {
		if errors.Is(err, wrong) {
			t.Fatalf("%s: an I/O failure was reported as %q: %v", where, wrong, err)
		}
	}
}

func faultsRules(t *testing.T, where string, f faultsFile, err error, store bool) {
	t.Helper()
	faultsJudge(t, where, f.kind, f.fired, err)
	if !store {
		return
	}
	poisoned := errors.Is(err, ErrPoisoned)
	if f.kind == faultsSync && !poisoned {
		t.Fatalf("%s: an fsync failed and the store was not poisoned: %v", where, err)
	}
	if (f.kind == faultsWrite || f.kind == faultsTorn) && !f.metaBefore && !f.firedAtMeta && poisoned {
		t.Fatalf("%s: a write failed before page 0 was touched, so nothing durable changed, "+
			"yet the store was poisoned: %v", where, err)
	}
}

func faultsCheckPoisoned(t *testing.T, where string, s *Store, cause error, closed bool) {
	t.Helper()
	calls := []struct {
		name string
		call func() error
	}{
		{"Allocate", func() error { _, err := s.Allocate(page.KindHeap); return err }},
		{"Free", func() error { return s.Free(1) }},
		{"SetRoot", func() error { return s.SetRoot(NoPage) }},
		{"Write", func() error { return s.Write(page.New(1, page.KindHeap)) }},
		{"Read", func() error { _, err := s.Read(1); return err }},
		{"Verify", s.Verify},
		{"DescribeMeta", func() error { _, err := s.DescribeMeta(); return err }},
		{"Checkpoint", s.Checkpoint},
		{"Sync", s.Sync},
	}
	if !closed {
		calls = append(calls, []struct {
			name string
			call func() error
		}{
			{"Close", s.Close},
			{"Allocate after Close", func() error { _, err := s.Allocate(page.KindHeap); return err }},
			{"Checkpoint after Close", s.Checkpoint},
		}...)
	}
	for _, c := range calls {
		err := c.call()
		if !errors.Is(err, ErrPoisoned) || !errors.Is(err, cause) {
			t.Fatalf("%s: after the store was poisoned, %s gave %v; want ErrPoisoned still carrying %q",
				where, c.name, err, cause)
		}
	}
}

type faultsOp struct {
	name     string
	commits  bool
	closes   bool
	noWrites bool
	targets  func(m *faultsModel) []core.PageID
	do       func(t *testing.T, s *Store, m *faultsModel, pick byte) error
}

func faultsFreeTargets(m *faultsModel) []core.PageID {
	var out []core.PageID
	for _, id := range m.live.ids() {
		if id != m.live.root {
			out = append(out, id)
		}
	}
	return out
}

func faultsRootTargets(m *faultsModel) []core.PageID {
	return append([]core.PageID{NoPage}, m.live.ids()...)
}

func faultsWriteTargets(m *faultsModel) []core.PageID {
	durable := m.durable().inUse
	var fresh []core.PageID
	for _, id := range m.live.ids() {
		if _, ok := durable[id]; !ok {
			fresh = append(fresh, id)
		}
	}
	if len(fresh) > 0 {
		return fresh
	}
	return m.live.ids()
}

func faultsReadTargets(m *faultsModel) []core.PageID { return m.live.ids() }

func faultsPick(ids []core.PageID, pick byte) (core.PageID, bool) {
	if len(ids) == 0 {
		return NoPage, false
	}
	return ids[int(pick)%len(ids)], true
}

var faultsOps = []faultsOp{
	{name: "Allocate", do: func(t *testing.T, s *Store, m *faultsModel, pick byte) error {
		t.Helper()
		p, err := s.Allocate(page.KindHeap)
		if err != nil {
			return err
		}
		id := p.ID()
		if _, ok := m.live.inUse[id]; ok {
			t.Fatalf("Allocate handed out page %d, which is still in use", id)
		}
		if _, ok := m.durable().inUse[id]; ok {
			t.Fatalf("Allocate handed out page %d, which checkpoint %d still references", id, m.gen)
		}
		m.live.inUse[id] = 0
		return nil
	}},
	{name: "Free", noWrites: true, targets: faultsFreeTargets, do: func(t *testing.T, s *Store, m *faultsModel, pick byte) error {
		id, ok := faultsPick(faultsFreeTargets(m), pick)
		if !ok {
			return nil
		}
		if err := s.Free(id); err != nil {
			return err
		}
		delete(m.live.inUse, id)
		return nil
	}},
	{name: "SetRoot", targets: faultsRootTargets, do: func(t *testing.T, s *Store, m *faultsModel, pick byte) error {
		id, _ := faultsPick(faultsRootTargets(m), pick)
		if err := s.SetRoot(id); err != nil {
			return err
		}
		m.live.root = id
		return nil
	}},
	{name: "Write", targets: faultsWriteTargets, do: func(t *testing.T, s *Store, m *faultsModel, pick byte) error {
		id, ok := faultsPick(faultsWriteTargets(m), pick)
		if !ok {
			return nil
		}
		tag := m.live.inUse[id]
		if _, durable := m.durable().inUse[id]; !durable {
			m.tag++
			tag = m.tag
		}
		p := page.New(id, page.KindHeap)
		binary.BigEndian.PutUint64(p[page.HeaderSize:], tag)
		if err := s.Write(p); err != nil {
			return err
		}
		m.live.inUse[id] = tag
		return nil
	}},
	{name: "Read", targets: faultsReadTargets, do: func(t *testing.T, s *Store, m *faultsModel, pick byte) error {
		t.Helper()
		id, ok := faultsPick(faultsReadTargets(m), pick)
		if !ok {
			return nil
		}
		p, err := s.Read(id)
		if err != nil {
			return err
		}
		if got := binary.BigEndian.Uint64(p[page.HeaderSize:]); got != m.live.inUse[id] {
			t.Fatalf("Read(%d) returned tag %d, want %d", id, got, m.live.inUse[id])
		}
		return nil
	}},
	{name: "Verify", do: func(t *testing.T, s *Store, m *faultsModel, pick byte) error { return s.Verify() }},
	{name: "DescribeMeta", do: func(t *testing.T, s *Store, m *faultsModel, pick byte) error {
		_, err := s.DescribeMeta()
		return err
	}},
	{name: "Checkpoint", commits: true, do: func(t *testing.T, s *Store, m *faultsModel, pick byte) error {
		if err := s.Checkpoint(); err != nil {
			return err
		}
		m.commit(s.Durable().Checkpoint)
		return nil
	}},
	{name: "Sync", do: func(t *testing.T, s *Store, m *faultsModel, pick byte) error { return s.Sync() }},
	{name: "Close", commits: true, closes: true, do: func(t *testing.T, s *Store, m *faultsModel, pick byte) error {
		if err := s.Close(); err != nil {
			return err
		}
		m.commit(s.Durable().Checkpoint)
		return nil
	}},
}

func faultsOpNamed(t *testing.T, name string) faultsOp {
	t.Helper()
	for _, op := range faultsOps {
		if op.name == name {
			return op
		}
	}
	t.Fatalf("no operation named %s", name)
	return faultsOp{}
}

type faultsBench struct {
	t  *testing.T
	mf *memFile
	ff *faultsFile
	s  *Store
	m  *faultsModel
}

func faultsFresh(t *testing.T) *faultsBench {
	t.Helper()
	mf := newMemFile()
	ff := faultsWrap(mf)
	s, err := create(page.NewFile(ff))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	return &faultsBench{t: t, mf: mf, ff: ff, s: s, m: faultsNewModel(s.Durable().Checkpoint)}
}

func (b *faultsBench) alloc() core.PageID {
	b.t.Helper()
	p, err := b.s.Allocate(page.KindHeap)
	if err != nil {
		b.t.Fatalf("Allocate: %v", err)
	}
	b.m.tag++
	binary.BigEndian.PutUint64(p[page.HeaderSize:], b.m.tag)
	if err := b.s.Write(p); err != nil {
		b.t.Fatalf("Write: %v", err)
	}
	b.m.live.inUse[p.ID()] = b.m.tag
	return p.ID()
}

func (b *faultsBench) free(id core.PageID) {
	b.t.Helper()
	if err := b.s.Free(id); err != nil {
		b.t.Fatalf("Free(%d): %v", id, err)
	}
	delete(b.m.live.inUse, id)
}

func (b *faultsBench) setRoot(id core.PageID) {
	b.t.Helper()
	if err := b.s.SetRoot(id); err != nil {
		b.t.Fatalf("SetRoot(%d): %v", id, err)
	}
	b.m.live.root = id
}

func (b *faultsBench) checkpoint() {
	b.t.Helper()
	if err := b.s.Checkpoint(); err != nil {
		b.t.Fatalf("Checkpoint: %v", err)
	}
	b.m.commit(b.s.Durable().Checkpoint)
}

func (b *faultsBench) close() {
	b.t.Helper()
	if err := b.s.Close(); err != nil {
		b.t.Fatalf("Close: %v", err)
	}
	b.m.commit(b.s.Durable().Checkpoint)
	b.mf.restart()
}

func faultsBusy(t *testing.T) *faultsBench {
	t.Helper()
	b := faultsFresh(t)
	var ids []core.PageID
	for i := 0; i < 8; i++ {
		ids = append(ids, b.alloc())
	}
	b.setRoot(ids[0])
	b.checkpoint()
	for _, id := range ids[2:5] {
		b.free(id)
	}
	b.checkpoint()
	b.alloc()
	b.free(ids[5])
	b.setRoot(ids[1])
	b.alloc()
	return b
}

func faultsClean(t *testing.T) *faultsBench {
	t.Helper()
	b := faultsBusy(t)
	b.checkpoint()
	return b
}

func faultsEdge(t *testing.T) *faultsBench {
	t.Helper()
	b := faultsFresh(t)
	var ids []core.PageID
	for uint32(b.s.Meta().NextPage) < b.s.Meta().FilePages {
		ids = append(ids, b.alloc())
	}
	b.setRoot(ids[0])
	b.checkpoint()
	b.free(ids[1])
	b.free(ids[2])
	return b
}

func faultsLongImage(t *testing.T) (*memFile, *faultsModel) {
	t.Helper()
	b := faultsFresh(t)
	var ids []core.PageID
	for i := 0; i < FreeListCapacity+4; i++ {
		ids = append(ids, b.alloc())
	}
	b.setRoot(ids[0])
	b.checkpoint()
	for _, id := range ids[1:] {
		b.free(id)
	}
	b.checkpoint()
	if len(b.s.ListPages()) < 2 {
		t.Fatalf("expected a free list of at least two pages, got %v", b.s.ListPages())
	}
	b.close()
	return b.mf, b.m
}

func faultsFromImage(t *testing.T, img *memFile, m *faultsModel) *faultsBench {
	t.Helper()
	mf := faultsClone(img)
	ff := faultsWrap(mf)
	s, _, err := openFile(ff)
	if err != nil {
		t.Fatalf("opening the prepared image: %v", err)
	}
	return &faultsBench{t: t, mf: mf, ff: ff, s: s, m: m.clone()}
}

type faultsCrash struct {
	name  string
	apply func(*memFile)
}

var faultsCrashes = []faultsCrash{
	{"process crash", func(m *memFile) { m.restart() }},
	{"process crash that evicts every clean page", func(m *memFile) {
		m.restartEvicting(func(int) bool { return true })
	}},
	{"process crash that evicts odd pages", func(m *memFile) {
		m.restartEvicting(func(b int) bool { return b%2 == 1 })
	}},
	{"power loss that keeps nothing", func(m *memFile) { m.keepNothing() }},
	{"power loss that keeps every write", func(m *memFile) { m.powerLoss(func(int) bool { return true }) }},
	{"power loss that keeps alternate writes", func(m *memFile) {
		m.powerLoss(func(i int) bool { return i%2 == 0 })
	}},
}

var faultsFewCrashes = []faultsCrash{faultsCrashes[0], faultsCrashes[2], faultsCrashes[5]}

func faultsRecover(t *testing.T, where string, mf *memFile, allowed map[uint32]faultsGen, keepDrain bool) {
	t.Helper()
	back, _, err := openFile(faultsWrap(mf))
	if err != nil {
		t.Fatalf("%s: the file no longer opens: %v", where, err)
	}
	g := back.Durable().Checkpoint
	want, ok := allowed[g]
	if !ok {
		back.Discard()
		t.Fatalf("%s: recovered checkpoint %d, want one of %v", where, g, faultsGens(allowed))
	}
	faultsCheck(t, where, back, want, true)
	for n := 0; n < 40 && len(back.FreePages()) > 0; n++ {
		p, err := back.Allocate(page.KindHeap)
		if err != nil {
			t.Fatalf("%s: allocating a free page after recovery: %v", where, err)
		}
		if _, ok := want.inUse[p.ID()]; ok {
			t.Fatalf("%s: page %d was handed out although recovered checkpoint %d still uses it", where, p.ID(), g)
		}
		binary.BigEndian.PutUint64(p[page.HeaderSize:], 0xDEADBEEFDEADBEEF)
		if err := back.Write(p); err != nil {
			t.Fatalf("%s: overwriting a reused page: %v", where, err)
		}
	}
	back.Discard()
	mf.powerLoss(func(int) bool { return keepDrain })
	again, _, err := openFile(faultsWrap(mf))
	if err != nil {
		t.Fatalf("%s: after reusing free pages and losing power, the file no longer opens: %v", where, err)
	}
	defer again.Discard()
	if got := again.Durable().Checkpoint; got != g {
		t.Fatalf("%s: Open recovered checkpoint %d, but a power loss afterwards went back to %d; "+
			"Open handed out pages on the strength of a meta page that was not durable", where, g, got)
	}
	faultsCheck(t, where+", after a later power loss", again, want, true)
}

func faultsCrashAll(t *testing.T, where string, img *memFile, allowed map[uint32]faultsGen, crashes []faultsCrash) {
	t.Helper()
	for i, c := range crashes {
		mf := img
		if i < len(crashes)-1 {
			mf = faultsClone(img)
		}
		c.apply(mf)
		faultsRecover(t, where+", then a "+c.name, mf, allowed, i%2 == 0)
	}
}

type faultsVariant struct {
	name string
	keep func(int) bool
	tear int
}

func faultsWhere(subject string, kind faultsKind, at int, v faultsVariant) string {
	where := fmt.Sprintf("%s with a %s on call %d", subject, kind, at)
	if v.name != "" {
		where += " " + v.name
	}
	return where
}

func faultsVariants(kind faultsKind) []faultsVariant {
	switch kind {
	case faultsTorn:
		return []faultsVariant{
			{"keeping the first sector", nil, 0},
			{"keeping all but the last sector", nil, -1},
		}
	case faultsSync:
		return []faultsVariant{
			{"dropping every write", func(int) bool { return false }, 0},
			{"keeping every write", func(int) bool { return true }, 0},
			{"keeping alternate writes", func(i int) bool { return i%2 == 0 }, 0},
		}
	}
	return []faultsVariant{{}}
}

func faultsPoint(t *testing.T, build func(*testing.T) *faultsBench, op faultsOp, kind faultsKind, at int,
	v faultsVariant, want faultsSnap, crashes []faultsCrash) {
	t.Helper()
	b := build(t)
	where := faultsWhere(op.name, kind, at, v)
	g := b.s.Durable().Checkpoint
	before := faultsSnapOf(b.s)
	live := b.m.live.copy()
	allowed := map[uint32]faultsGen{g: b.m.durable()}

	b.ff.arm(kind, at, v.keep, v.tear)
	err := op.do(t, b.s, b.m, 0)
	flags := *b.ff
	b.ff.reset()
	faultsRules(t, where, flags, err, true)
	if op.commits && flags.metaWritten {
		allowed[g+1] = live
	}
	img := faultsClone(b.mf)

	if errors.Is(err, ErrPoisoned) {
		faultsCheckPoisoned(t, where, b.s, kind.cause(), op.closes)
	} else {
		if diff := faultsDiff(before, faultsSnapOf(b.s)); diff != "" {
			t.Fatalf("%s: a failure that did not poison changed the store: %s", where, diff)
		}
		if op.closes {
			if err := b.s.Close(); err != nil {
				t.Fatalf("%s: a second Close gave %v", where, err)
			}
		} else {
			faultsCheck(t, where+", before retrying", b.s, b.m.live, false)
			if err := op.do(t, b.s, b.m, 0); err != nil {
				t.Fatalf("%s: retrying after a failure that did not poison: %v", where, err)
			}
			if diff := faultsDiff(want, faultsSnapOf(b.s)); diff != "" {
				t.Fatalf("%s: the retry did not end where an unfaulted run ends: %s", where, diff)
			}
			faultsCheck(t, where+", after retrying", b.s, b.m.live, true)
		}
	}
	b.s.Discard()
	faultsCrashAll(t, where, img, allowed, crashes)
}

func faultsMatrix(t *testing.T, build func(*testing.T) *faultsBench, op faultsOp, crashes []faultsCrash) int {
	t.Helper()
	dry := build(t)
	if op.targets != nil && len(op.targets(dry.m)) == 0 {
		dry.s.Discard()
		return 0
	}
	dry.ff.reset()
	if err := op.do(t, dry.s, dry.m, 0); err != nil {
		t.Fatalf("%s with no fault injected: %v", op.name, err)
	}
	seen := dry.ff.seen
	want := faultsSnapOf(dry.s)
	dry.s.Discard()
	if op.noWrites && seen[faultsWrite] != 0 {
		t.Fatalf("%s wrote to the file %d times; a freed page must stay untouched until a checkpoint "+
			"stops referencing it", op.name, seen[faultsWrite])
	}
	points := 0
	for kind := faultsWrite; kind < faultsKindCount; kind++ {
		for at := 1; at <= seen[kind.counter()]; at++ {
			for _, v := range faultsVariants(kind) {
				faultsPoint(t, build, op, kind, at, v, want, crashes)
				points++
			}
		}
	}
	return points
}

func TestTheFaultFileFailsExactlyTheCallItIsArmedFor(t *testing.T) {
	a := bytes.Repeat([]byte{0xAA}, page.Size)
	b := bytes.Repeat([]byte{0xBB}, page.Size)

	ff := faultsWrap(newMemFile())
	ff.arm(faultsWrite, 2, nil, 0)
	for i, want := range []error{nil, faultsErrWrite, nil} {
		n, err := ff.WriteAt(a, int64(i)*page.Size)
		if !errors.Is(err, want) || (want == nil) != (n == page.Size) || (want != nil && n != 0) {
			t.Fatalf("write %d gave %d, %v; want only the second write to fail, writing nothing", i+1, n, err)
		}
	}
	if size, _ := ff.Size(); size != 3*page.Size {
		t.Fatalf("after a failed middle write the file is %d bytes, want %d", size, 3*page.Size)
	}
	if !bytes.Equal(ff.mf.live[page.Size:2*page.Size], make([]byte, page.Size)) {
		t.Fatal("a failed write changed the bytes it was asked to write")
	}

	for _, c := range []struct {
		tear, want int
	}{{0, faultsSector}, {-1, page.Size - faultsSector}, {3, 4 * faultsSector}} {
		ff := faultsWrap(newMemFile())
		ff.arm(faultsTorn, 1, nil, c.tear)
		n, err := ff.WriteAt(a, 0)
		if n != c.want || !errors.Is(err, faultsErrTorn) {
			t.Fatalf("torn write with tear %d gave %d, %v; want %d bytes and the torn error", c.tear, n, err, c.want)
		}
		if !bytes.Equal(ff.mf.live, a[:c.want]) || !ff.metaWritten || !ff.firedAtMeta {
			t.Fatalf("torn write with tear %d left %d bytes in the cache, want a %d-byte prefix", c.tear, len(ff.mf.live), c.want)
		}
	}

	ff = faultsWrap(newMemFile())
	if _, err := ff.WriteAt(a, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := ff.WriteAt(b, page.Size); err != nil {
		t.Fatal(err)
	}
	ff.arm(faultsSync, 1, func(i int) bool { return i == 0 }, 0)
	if err := ff.Sync(); !errors.Is(err, errSyncFailed) {
		t.Fatalf("an armed fsync gave %v, want the injected failure", err)
	}
	ff.reset()
	if err := ff.Sync(); err != nil {
		t.Fatalf("an fsync after the failure gave %v", err)
	}
	if !bytes.Equal(ff.mf.durable, a) || !bytes.Equal(ff.mf.live[page.Size:], b) {
		t.Fatalf("after a failed fsync that kept only the first write, the disk holds %d bytes; "+
			"the dropped write must stay readable in the cache and never reach the disk", len(ff.mf.durable))
	}

	ff.arm(faultsRead, 1, nil, 0)
	buf := make([]byte, page.Size)
	if n, err := ff.ReadAt(buf, 0); n != page.Size/2 || !errors.Is(err, faultsErrRead) {
		t.Fatalf("an armed read gave %d, %v; want half a page and the injected failure", n, err)
	}
	if n, err := ff.ReadAt(buf, 0); n != page.Size || err != nil {
		t.Fatalf("the read after the failure gave %d, %v", n, err)
	}

	ff.arm(faultsSize, 2, nil, 0)
	if _, err := ff.Size(); err != nil {
		t.Fatalf("the first size call failed: %v", err)
	}
	if _, err := ff.Size(); !errors.Is(err, faultsErrSize) {
		t.Fatalf("the second size call gave %v, want the injected failure", err)
	}
	if ff.seen[faultsSize] != 2 || ff.seen[faultsRead] != 0 {
		t.Fatalf("counted %v calls since arming", ff.seen)
	}
}

func TestEveryFailurePointInCreateLeavesAFileThatOpensAtTheFirstGeneration(t *testing.T) {
	dry := faultsWrap(newMemFile())
	if _, err := create(page.NewFile(dry)); err != nil {
		t.Fatal(err)
	}
	seen := dry.seen
	if seen[faultsWrite] == 0 || seen[faultsSync] < 2 {
		t.Fatalf("create wrote %d times and synced %d times; it must write the meta page and fsync before and after",
			seen[faultsWrite], seen[faultsSync])
	}
	first := map[uint32]faultsGen{1: faultsEmpty()}
	points := 0
	for kind := faultsWrite; kind < faultsKindCount; kind++ {
		for at := 1; at <= seen[kind.counter()]; at++ {
			for _, v := range faultsVariants(kind) {
				where := faultsWhere("create", kind, at, v)
				mf := newMemFile()
				ff := faultsWrap(mf)
				ff.arm(kind, at, v.keep, v.tear)
				s, err := create(page.NewFile(ff))
				flags := *ff
				ff.reset()
				if s != nil {
					s.Discard()
				}
				faultsRules(t, where, flags, err, true)
				img := faultsClone(mf)
				if !errors.Is(err, ErrPoisoned) {
					again, err := create(page.NewFile(ff))
					if err != nil {
						t.Fatalf("%s: retrying create after a failure that did not poison: %v", where, err)
					}
					if g := again.Durable().Checkpoint; g != 1 {
						t.Fatalf("%s: the retried create is at checkpoint %d, want 1", where, g)
					}
					faultsCheck(t, where+", after retrying", again, faultsEmpty(), true)
					again.Discard()
				}
				faultsCrashAll(t, where, img, first, faultsCrashes)
				points++
			}
		}
	}
	t.Logf("exercised %d failure points inside create", points)
}

func TestEveryFailurePointInAStoreOperationEitherPoisonsOrLeavesTheStoreAsItWas(t *testing.T) {
	setups := []struct {
		name  string
		build func(*testing.T) *faultsBench
	}{
		{"fresh", faultsFresh},
		{"busy", faultsBusy},
		{"clean", faultsClean},
		{"at the end of a chunk", faultsEdge},
	}
	total := 0
	for _, su := range setups {
		for _, op := range faultsOps {
			su, op := su, op
			t.Run(su.name+"/"+op.name, func(t *testing.T) {
				n := faultsMatrix(t, su.build, op, faultsCrashes)
				total += n
				t.Logf("exercised %d failure points", n)
			})
		}
	}
	t.Logf("exercised %d failure points across every store operation", total)
	if total < 100 {
		t.Fatalf("only %d failure points were found; the operations stopped doing the I/O this test expects", total)
	}
}

func TestEveryFailurePointInACheckpointThatWritesTwoFreeListPagesIsRecoverable(t *testing.T) {
	img, model := faultsLongImage(t)
	build := func(t *testing.T) *faultsBench {
		t.Helper()
		b := faultsFromImage(t, img, model)
		a := b.alloc()
		b.alloc()
		b.free(a)
		return b
	}
	total := 0
	for _, name := range []string{"Checkpoint", "Allocate"} {
		op := faultsOpNamed(t, name)
		t.Run(name, func(t *testing.T) {
			n := faultsMatrix(t, build, op, faultsFewCrashes)
			total += n
			t.Logf("exercised %d failure points", n)
		})
	}
	t.Logf("exercised %d failure points with a two-page free list", total)
}

func TestEveryFailurePointInOpenLeavesTheFileRecoverable(t *testing.T) {
	images := []struct {
		name    string
		build   func(*testing.T) (*memFile, map[uint32]faultsGen)
		crashes []faultsCrash
	}{
		{"a cleanly closed store", func(t *testing.T) (*memFile, map[uint32]faultsGen) {
			b := faultsBusy(t)
			b.close()
			return b.mf, map[uint32]faultsGen{b.m.gen: b.m.durable()}
		}, nil},
		{"a store whose last checkpoint lost its final fsync", func(t *testing.T) (*memFile, map[uint32]faultsGen) {
			b := faultsBusy(t)
			g, live := b.m.gen, b.m.live.copy()
			b.ff.arm(faultsSync, 3, nil, 0)
			err := b.s.Checkpoint()
			meta := b.ff.metaWritten
			b.ff.reset()
			if !errors.Is(err, ErrPoisoned) || !meta {
				t.Fatalf("a checkpoint whose final fsync failed gave %v with the meta written %v", err, meta)
			}
			b.s.Discard()
			b.mf.restart()
			return b.mf, map[uint32]faultsGen{g: b.m.durable(), g + 1: live}
		}, nil},
		{"a crashed store with unsynced pages in the cache", func(t *testing.T) (*memFile, map[uint32]faultsGen) {
			b := faultsBusy(t)
			b.s.Discard()
			b.mf.restart()
			return b.mf, map[uint32]faultsGen{b.m.gen: b.m.durable()}
		}, nil},
		{"an all-zero file an interrupted create left", func(t *testing.T) (*memFile, map[uint32]faultsGen) {
			mf := newMemFile()
			if _, err := mf.WriteAt(make([]byte, ChunkPages*page.Size), 0); err != nil {
				t.Fatal(err)
			}
			return mf, map[uint32]faultsGen{1: faultsEmpty()}
		}, nil},
		{"an empty file", func(t *testing.T) (*memFile, map[uint32]faultsGen) {
			return newMemFile(), map[uint32]faultsGen{1: faultsEmpty()}
		}, nil},
		{"a store whose free list spans two pages", func(t *testing.T) (*memFile, map[uint32]faultsGen) {
			img, m := faultsLongImage(t)
			return img, map[uint32]faultsGen{m.gen: m.durable()}
		}, faultsFewCrashes},
	}
	total := 0
	for _, im := range images {
		im := im
		t.Run(im.name, func(t *testing.T) {
			img, allowed := im.build(t)
			crashes := im.crashes
			if crashes == nil {
				crashes = faultsCrashes
			}
			dry := faultsWrap(faultsClone(img))
			s, _, err := openFile(dry)
			if err != nil {
				t.Fatalf("opening with no fault injected: %v", err)
			}
			if _, ok := allowed[s.Durable().Checkpoint]; !ok {
				t.Fatalf("opened at checkpoint %d, want one of %v", s.Durable().Checkpoint, faultsGens(allowed))
			}
			s.Discard()
			seen := dry.seen
			points := 0
			for kind := faultsWrite; kind < faultsKindCount; kind++ {
				for at := 1; at <= seen[kind.counter()]; at++ {
					for _, v := range faultsVariants(kind) {
						where := faultsWhere("Open", kind, at, v)
						mf := faultsClone(img)
						ff := faultsWrap(mf)
						ff.arm(kind, at, v.keep, v.tear)
						s, _, err := openFile(ff)
						flags := *ff
						ff.reset()
						if s != nil {
							s.Discard()
						}
						faultsRules(t, where, flags, err, false)
						faultsCrashAll(t, where, mf, allowed, crashes)
						points++
					}
				}
			}
			total += points
			t.Logf("exercised %d failure points", points)
		})
	}
	t.Logf("exercised %d failure points inside Open", total)
}

type faultsTape struct {
	b []byte
	i int
}

func (r *faultsTape) more() bool { return r.i < len(r.b) }

func (r *faultsTape) next() byte {
	if r.i >= len(r.b) {
		return 0
	}
	c := r.b[r.i]
	r.i++
	return c
}

func faultsSeed(seed uint64, n int) []byte {
	out := make([]byte, n)
	x := seed*0x9E3779B97F4A7C15 + 1
	for i := range out {
		x ^= x << 13
		x ^= x >> 7
		x ^= x << 17
		out[i] = byte(x >> 24)
	}
	return out
}

func faultsCrashBy(mf *memFile, mode, p byte) {
	switch mode % 3 {
	case 0:
		mf.restart()
	case 1:
		mf.restartEvicting(faultsSubset(p))
	default:
		mf.powerLoss(faultsSubset(p))
	}
}

func faultsDrive(t *testing.T, script []byte) int {
	if len(script) > 300 {
		script = script[:300]
	}
	r := &faultsTape{b: script}
	mf := newMemFile()
	ff := faultsWrap(mf)
	s, err := create(page.NewFile(ff))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer func() { s.Discard() }()
	m := faultsNewModel(s.Durable().Checkpoint)
	fired := 0

	reopen := func(where string, allowed map[uint32]faultsGen) {
		t.Helper()
		var back *Store
		if r.next()&3 == 3 {
			kind, at, p := faultsKind(r.next()%byte(faultsKindCount)), 1+int(r.next()%4), r.next()
			ff.arm(kind, at, faultsSubset(p), int(p))
			got, _, err := openFile(ff)
			flags := *ff
			ff.reset()
			switch {
			case flags.fired:
				fired++
				if got != nil {
					got.Discard()
				}
				faultsRules(t, where+", opening", flags, err, false)
				mf.restart()
			case err != nil:
				t.Fatalf("%s: opening failed with no fault injected: %v", where, err)
			default:
				back = got
			}
		}
		if back == nil {
			got, _, err := openFile(ff)
			if err != nil {
				t.Fatalf("%s: the file no longer opens: %v", where, err)
			}
			back = got
		}
		s = back
		g := s.Durable().Checkpoint
		want, ok := allowed[g]
		if !ok {
			t.Fatalf("%s: recovered checkpoint %d, want one of %v", where, g, faultsGens(allowed))
		}
		if g == m.gen {
			m.revert()
		} else {
			m.live = want.copy()
			m.commit(g)
		}
	}

	for step := 0; r.more(); step++ {
		where := fmt.Sprintf("step %d", step)
		code := r.next()
		if code&0xC0 == 0xC0 {
			kind, at, p := faultsKind(r.next()%byte(faultsKindCount)), 1+int(r.next()%3), r.next()
			ff.arm(kind, at, faultsSubset(p), int(p))
		} else {
			ff.reset()
		}
		c := int(code&0x3F) % (len(faultsOps) + 3)
		if c >= len(faultsOps) {
			ff.reset()
			s.Discard()
			faultsCrashBy(mf, byte(c-len(faultsOps)), r.next())
			reopen(where+", after a crash", map[uint32]faultsGen{m.gen: m.durable()})
			faultsCheck(t, where, s, m.live, true)
			continue
		}
		o := faultsOps[c]
		where += ", " + o.name
		before := faultsSnapOf(s)
		live := m.live.copy()
		pick := r.next()
		err := o.do(t, s, m, pick)
		flags := *ff
		ff.reset()
		if flags.fired {
			fired++
			faultsRules(t, where, flags, err, true)
		} else if err != nil {
			t.Fatalf("%s: failed with no fault injected: %v", where, err)
		}
		switch {
		case err != nil && errors.Is(err, ErrPoisoned):
			faultsCheckPoisoned(t, where, s, flags.kind.cause(), o.closes)
			allowed := map[uint32]faultsGen{m.gen: m.durable()}
			if o.commits && flags.metaWritten {
				allowed[m.gen+1] = live
			}
			s.Discard()
			mode := r.next()
			faultsCrashBy(mf, mode, r.next())
			reopen(where+", recovering from the poisoned store", allowed)
		case err != nil:
			if diff := faultsDiff(before, faultsSnapOf(s)); diff != "" {
				t.Fatalf("%s: a failure that did not poison changed the store: %s", where, diff)
			}
			if o.closes {
				mf.restart()
				reopen(where+", reopening after a failed Close", map[uint32]faultsGen{m.gen: m.durable()})
			} else if err := o.do(t, s, m, pick); err != nil {
				t.Fatalf("%s: retrying after a failure that did not poison: %v", where, err)
			}
		case o.closes:
			mf.restart()
			reopen(where+", reopening after Close", map[uint32]faultsGen{m.gen: m.durable()})
		}
		faultsCheck(t, where, s, m.live, true)
		if g := s.Durable().Checkpoint; g != m.gen {
			t.Fatalf("%s: the store is at checkpoint %d, the model at %d", where, g, m.gen)
		}
	}
	return fired
}

func FuzzRandomFaultsNeverBreakRecovery(f *testing.F) {
	f.Add([]byte{0, 0, 0, 0, 0, 0, 0, 0, 3, 0, 3, 1, 2, 1, 1, 1, 7, 0, 0, 0,
		0xC7, 2, 2, 0, 0, 0, 0, 0, 5, 0, 12, 0x55, 0})
	f.Add([]byte{0, 0, 0, 0, 0, 0, 2, 1, 1, 1, 1, 0, 7, 0,
		0xC0, 0, 0, 0, 0, 0, 0, 0xC7, 1, 0, 0, 0, 4, 0, 0xC7, 0, 1, 0, 0, 7, 0, 11, 0x0F, 3, 2, 1, 0})
	f.Add([]byte{0, 0, 0, 0, 1, 0, 7, 0, 0xC5, 3, 0, 0, 0, 0xC5, 4, 0, 0, 0,
		0xC4, 3, 1, 0, 0, 9, 0, 3, 3, 0, 0, 0, 0xC9, 2, 2, 0xAA, 0, 2, 0x33, 0})
	f.Add([]byte{0, 0, 2, 1, 7, 0, 1, 0, 0xC9, 1, 1, 0, 0, 1, 0xF0, 0,
		0xC8, 2, 0, 0x0F, 0, 1, 0x0F, 3, 3, 0, 0, 7, 0, 10, 0, 3, 4, 1, 0})
	f.Add([]byte("\xcd0000A0000Z0\xd621"))
	f.Add([]byte("Z0000A0\xc9"))
	f.Add(append(make([]byte, 2*(ChunkPages-1)), 0xC0, 0, 0, 0, 0, 4, 0))
	f.Add(append(make([]byte, 2*(ChunkPages-1)), 1, 0, 0xC7, 1, 0, 0, 0, 0xC7, 1, 0, 0xFF, 0, 4, 0))
	for seed := uint64(1); seed <= 16; seed++ {
		f.Add(faultsSeed(seed, 200))
	}
	f.Fuzz(func(t *testing.T, script []byte) {
		faultsDrive(t, script)
	})
}
