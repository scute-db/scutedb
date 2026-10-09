package pagestore

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"testing"

	"github.com/scute-db/scutedb/internal/core"
	"github.com/scute-db/scutedb/internal/page"
)

const (
	oraclePageSize = 4096
	oracleListCap  = (oraclePageSize - 20) / 4
	oracleMaxOps   = 120
	oracleMaxPages = 3000
	oracleOpCount  = 19
)

const (
	oracleInUse byte = iota
	oracleFree
	oracleList
)

var oracleMagic = []byte{'S', 'C', 'U', 'T', 'E', 'D', 'B', 0}

var oracleErrWrite = errors.New("oracle: injected write failure")

type oracleImage struct {
	meta    Meta
	chain   []core.PageID
	entries []core.PageID
	state   []byte
}

func oracleU32(b []byte, at int) uint32 { return binary.BigEndian.Uint32(b[at : at+4]) }

func oracleStateName(st byte) string {
	switch st {
	case oracleFree:
		return "free"
	case oracleList:
		return "holding the free list"
	}
	return "in use"
}

func oracleDecode(img []byte) (*oracleImage, error) {
	if len(img) < oraclePageSize {
		return nil, fmt.Errorf("the image is %d bytes, less than one page", len(img))
	}
	if id := oracleU32(img, 0); id != 0 {
		return nil, fmt.Errorf("page 0 says it is page %d", id)
	}
	if img[4] != 1 {
		return nil, fmt.Errorf("page 0 has kind %d, want 1", img[4])
	}
	if !bytes.Equal(img[16:24], oracleMagic) {
		return nil, fmt.Errorf("bytes 16..23 are % 02X, not the magic", img[16:24])
	}
	m := Meta{
		Version:      oracleU32(img, 24),
		PageSize:     oracleU32(img, 28),
		Root:         core.PageID(oracleU32(img, 32)),
		FreeListPage: core.PageID(oracleU32(img, 36)),
		FreeCount:    oracleU32(img, 40),
		NextPage:     core.PageID(oracleU32(img, 44)),
		FilePages:    oracleU32(img, 48),
		Checkpoint:   oracleU32(img, 52),
	}
	switch {
	case m.Version != 1:
		return nil, fmt.Errorf("version %d", m.Version)
	case m.PageSize != oraclePageSize:
		return nil, fmt.Errorf("page size %d", m.PageSize)
	case m.NextPage == 0:
		return nil, errors.New("next page is 0, the meta page")
	case uint32(m.NextPage) > m.FilePages:
		return nil, fmt.Errorf("next page %d is past the %d pages the meta claims", m.NextPage, m.FilePages)
	case uint64(m.FilePages)*oraclePageSize > uint64(len(img)):
		return nil, fmt.Errorf("the meta claims %d pages, the image holds %d bytes", m.FilePages, len(img))
	case m.Root != 0 && m.Root >= m.NextPage:
		return nil, fmt.Errorf("root %d was never handed out, next page is %d", m.Root, m.NextPage)
	}
	d := &oracleImage{meta: m, state: make([]byte, m.NextPage)}
	for id := m.FreeListPage; id != 0; {
		if id >= m.NextPage {
			return nil, fmt.Errorf("the chain reaches page %d, next page is %d", id, m.NextPage)
		}
		if d.state[id] == oracleList {
			return nil, fmt.Errorf("the chain reaches page %d twice", id)
		}
		p := img[int(id)*oraclePageSize : (int(id)+1)*oraclePageSize]
		if oracleU32(p, 0) != uint32(id) || p[4] != 5 {
			return nil, fmt.Errorf("chain page %d says it is page %d of kind %d", id, oracleU32(p, 0), p[4])
		}
		n := int(binary.BigEndian.Uint16(p[6:8]))
		if n > oracleListCap {
			return nil, fmt.Errorf("chain page %d claims %d entries", id, n)
		}
		d.state[id] = oracleList
		d.chain = append(d.chain, id)
		for i := 0; i < n; i++ {
			d.entries = append(d.entries, core.PageID(oracleU32(p, 20+4*i)))
		}
		id = core.PageID(oracleU32(p, 16))
	}
	if uint32(len(d.entries)) != m.FreeCount {
		return nil, fmt.Errorf("the chain holds %d entries, the meta says %d", len(d.entries), m.FreeCount)
	}
	for _, e := range d.entries {
		switch {
		case e == 0 || e >= m.NextPage:
			return nil, fmt.Errorf("the chain names page %d, outside 1..%d", e, m.NextPage-1)
		case d.state[e] != oracleInUse:
			return nil, fmt.Errorf("the chain names page %d, which is already %s", e, oracleStateName(d.state[e]))
		case e == m.Root:
			return nil, fmt.Errorf("the chain names the root %d", e)
		}
		d.state[e] = oracleFree
	}
	if m.Root != 0 && d.state[m.Root] == oracleList {
		return nil, fmt.Errorf("the root %d holds the free list", m.Root)
	}
	return d, nil
}

type oracleDisk struct {
	mf          *memFile
	writes      int
	failWrite   int
	failedAt    int64
	failedLen   int
	metaWritten bool
	finalFailed bool
	guard       bool
	cached      *oracleImage
	cachedErr   error
	violation   error
}

func (d *oracleDisk) ReadAt(p []byte, off int64) (int, error) { return d.mf.ReadAt(p, off) }

func (d *oracleDisk) WriteAt(p []byte, off int64) (int, error) {
	d.writes++
	if d.failWrite != 0 && d.writes == d.failWrite {
		d.failWrite = 0
		d.failedAt, d.failedLen = off, len(p)
		return 0, oracleErrWrite
	}
	if d.guard {
		d.checkWrite(off, len(p))
	}
	n, err := d.mf.WriteAt(p, off)
	if err == nil && off < oraclePageSize {
		d.metaWritten = true
	}
	return n, err
}

func (d *oracleDisk) Sync() error {
	err := d.mf.Sync()
	if err != nil {
		d.finalFailed = d.metaWritten
	}
	d.metaWritten = false
	d.forget()
	return err
}

func (d *oracleDisk) Size() (int64, error) { return d.mf.Size() }
func (d *oracleDisk) Close() error         { return d.mf.Close() }
func (d *oracleDisk) forget()              { d.cached, d.cachedErr = nil, nil }

func (d *oracleDisk) image() (*oracleImage, error) {
	if d.cached == nil && d.cachedErr == nil {
		d.cached, d.cachedErr = oracleDecode(d.mf.durable)
	}
	return d.cached, d.cachedErr
}

func (d *oracleDisk) checkWrite(off int64, n int) {
	if d.violation != nil || n == 0 {
		return
	}
	img, err := d.image()
	if err != nil {
		d.violation = fmt.Errorf("the store wrote %d bytes at offset %d while the durable image did not decode: %v", n, off, err)
		return
	}
	for id := off / oraclePageSize; id <= (off+int64(n)-1)/oraclePageSize; id++ {
		if id == 0 || id >= int64(len(img.state)) {
			continue
		}
		if st := img.state[id]; st != oracleFree {
			d.violation = fmt.Errorf("the store wrote page %d while the durable image, generation %d, says it is %s",
				id, img.meta.Checkpoint, oracleStateName(st))
			return
		}
	}
}

func oracleSubset(seed byte) func(int) bool {
	switch seed % 4 {
	case 0:
		return func(int) bool { return false }
	case 1:
		return func(int) bool { return true }
	}
	return func(i int) bool {
		x := uint32(i)*0x9E3779B1 ^ uint32(seed)*0x85EBCA77
		x ^= x >> 13
		return (x>>5)&1 == 1
	}
}

func oracleKeys[V any](m map[core.PageID]V) []core.PageID {
	ids := make([]core.PageID, 0, len(m))
	for id := range m {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(a, b int) bool { return ids[a] < ids[b] })
	return ids
}

func oracleSameOrder(a, b []core.PageID) bool {
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

func oracleShort(ids []core.PageID) string {
	if len(ids) <= 12 {
		return fmt.Sprint(ids)
	}
	return fmt.Sprintf("%v ... %v (%d pages)", ids[:6], ids[len(ids)-6:], len(ids))
}

func oracleFirstDiff(a, b []byte) int {
	for i := range a {
		if i >= len(b) || a[i] != b[i] {
			return i
		}
	}
	return len(a)
}

func oracleByte(ops []byte, i int) byte {
	if i < len(ops) {
		return ops[i]
	}
	return 0
}

type oracleSnap struct {
	gen   uint32
	root  core.PageID
	pages map[core.PageID][]byte
}

type oracleView struct {
	meta    Meta
	durable Meta
	root    core.PageID
	free    []core.PageID
	pending []core.PageID
	lists   []core.PageID
	dirty   bool
	stats   Stats
}

type oracleRun struct {
	t         *testing.T
	disk      *oracleDisk
	s         *Store
	step      int
	op        byte
	pages     map[core.PageID][]byte
	fresh     map[core.PageID]bool
	pending   map[core.PageID]bool
	root      core.PageID
	dirty     bool
	committed oracleSnap
	candidate *oracleSnap
	stamp     uint64
}

func oracleStart(t *testing.T) *oracleRun {
	t.Helper()
	disk := &oracleDisk{mf: newMemFile(), failedAt: -1}
	s, err := create(page.NewFile(disk))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	disk.guard = true
	r := &oracleRun{t: t, disk: disk, s: s}
	r.adopt(oracleSnap{gen: r.image().meta.Checkpoint, pages: map[core.PageID][]byte{}})
	r.checkDurable(r.committed)
	return r
}

func (r *oracleRun) fatalf(format string, args ...any) {
	r.t.Helper()
	r.t.Fatalf("step %d (op %d): %s", r.step, r.op, fmt.Sprintf(format, args...))
}

func (r *oracleRun) image() *oracleImage {
	r.t.Helper()
	img, err := r.disk.image()
	if err != nil {
		r.fatalf("the durable image does not decode: %v", err)
	}
	return img
}

func (r *oracleRun) checkGuard() {
	r.t.Helper()
	if r.disk.violation != nil {
		r.fatalf("%v", r.disk.violation)
	}
}

func (r *oracleRun) snap(gen uint32) oracleSnap {
	pages := make(map[core.PageID][]byte, len(r.pages))
	for id, b := range r.pages {
		pages[id] = b
	}
	return oracleSnap{gen: gen, root: r.root, pages: pages}
}

func (r *oracleRun) adopt(s oracleSnap) {
	r.committed = s
	r.candidate = nil
	r.pages = make(map[core.PageID][]byte, len(s.pages))
	for id, b := range s.pages {
		r.pages[id] = b
	}
	r.root = s.root
	r.fresh = map[core.PageID]bool{}
	r.pending = map[core.PageID]bool{}
	r.dirty = false
}

func (r *oracleRun) observe() oracleView {
	return oracleView{
		meta:    r.s.Meta(),
		durable: r.s.Durable(),
		root:    r.s.Root(),
		free:    r.s.FreePages(),
		pending: r.s.PendingPages(),
		lists:   r.s.ListPages(),
		dirty:   r.s.Dirty(),
		stats:   r.s.Stats(),
	}
}

func (r *oracleRun) checkDurable(want oracleSnap) {
	r.t.Helper()
	r.checkGuard()
	img, err := oracleDecode(r.disk.mf.durable)
	if err != nil {
		r.fatalf("the durable image does not decode: %v", err)
	}
	if got := r.s.Durable(); got != img.meta {
		r.fatalf("the store says its durable meta is %+v, the disk holds %+v", got, img.meta)
	}
	if img.meta.Checkpoint != want.gen || img.meta.Root != want.root {
		r.fatalf("the disk holds generation %d with root %d, want generation %d with root %d",
			img.meta.Checkpoint, img.meta.Root, want.gen, want.root)
	}
	for id := range want.pages {
		if id >= img.meta.NextPage {
			r.fatalf("page %d is in use at generation %d but the disk says only pages below %d were handed out",
				id, want.gen, img.meta.NextPage)
		}
	}
	for id := core.PageID(1); id < img.meta.NextPage; id++ {
		_, inUse := want.pages[id]
		st := img.state[id]
		if inUse && st != oracleInUse {
			r.fatalf("page %d is in use at generation %d but the disk says it is %s", id, want.gen, oracleStateName(st))
		}
		if !inUse && st == oracleInUse {
			r.fatalf("page %d was handed out but at generation %d it is not in use, not free and not holding the list",
				id, want.gen)
		}
	}
	for id, b := range want.pages {
		at := int(id) * oraclePageSize
		if got := r.disk.mf.durable[at : at+oraclePageSize]; !bytes.Equal(got, b) {
			r.fatalf("page %d on disk differs from what was written for generation %d, first at byte %d",
				id, want.gen, oracleFirstDiff(got, b))
		}
	}
	if got := r.s.Meta(); got != img.meta {
		r.fatalf("the store is clean but its Meta() is %+v while the disk holds %+v", got, img.meta)
	}
	if got := r.s.FreePages(); !oracleSameOrder(oracleSorted(got), oracleSorted(img.entries)) {
		r.fatalf("the store reports free pages %s, the durable chain holds %s", oracleShort(got), oracleShort(img.entries))
	}
	if got := r.s.ListPages(); !oracleSameOrder(got, img.chain) {
		r.fatalf("the store reports free-list pages %v, the durable chain is %v", got, img.chain)
	}
	if got := r.s.PendingPages(); len(got) != 0 {
		r.fatalf("pages %s are still pending at a clean point", oracleShort(got))
	}
	if r.s.Dirty() {
		r.fatalf("the store is dirty at a clean point")
	}
}

func (r *oracleRun) checkLive() {
	r.t.Helper()
	r.checkGuard()
	if err := r.s.Verify(); err != nil {
		r.fatalf("Verify: %v", err)
	}
	if got := r.s.Root(); got != r.root {
		r.fatalf("the root is %d, want %d", got, r.root)
	}
	img := r.image()
	if got := r.s.Durable(); got != img.meta {
		r.fatalf("the store says its durable meta is %+v, the disk holds %+v", got, img.meta)
	}
	if img.meta.Checkpoint != r.committed.gen {
		r.fatalf("the disk holds generation %d without a checkpoint committing it, want %d",
			img.meta.Checkpoint, r.committed.gen)
	}
	lists := r.s.ListPages()
	if !oracleSameOrder(lists, img.chain) {
		r.fatalf("the store reports free-list pages %v, the durable chain is %v", lists, img.chain)
	}
	next := r.s.Meta().NextPage
	names := [...]string{"", "in use", "free", "pending", "holding the free list"}
	owner := make([]byte, next)
	claim := func(id core.PageID, as byte) {
		if id == 0 || id >= next {
			r.fatalf("page %d is %s but only pages 1..%d are handed out", id, names[as], next-1)
		}
		if owner[id] != 0 {
			r.fatalf("page %d is %s and also %s", id, names[owner[id]], names[as])
		}
		owner[id] = as
	}
	for id := range r.pages {
		claim(id, 1)
	}
	for _, id := range r.s.FreePages() {
		claim(id, 2)
		if int(id) >= len(img.state) {
			r.fatalf("page %d is reusable but the durable image never handed it out", id)
		}
		if img.state[id] != oracleFree {
			r.fatalf("page %d is reusable but the durable image says it is %s", id, oracleStateName(img.state[id]))
		}
	}
	pending := r.s.PendingPages()
	if len(pending) != len(r.pending) {
		r.fatalf("the store holds %d pending pages, %d were freed since the last checkpoint", len(pending), len(r.pending))
	}
	for _, id := range pending {
		if !r.pending[id] {
			r.fatalf("page %d is pending but was not freed since the last checkpoint", id)
		}
		claim(id, 3)
	}
	for _, id := range lists {
		claim(id, 4)
	}
	for id := core.PageID(1); id < next; id++ {
		if owner[id] == 0 {
			r.fatalf("page %d was handed out and is now in no state at all", id)
		}
	}
}

func (r *oracleRun) checkPoisoned() {
	r.t.Helper()
	writes, syncs := r.disk.writes, r.disk.mf.syncs
	if _, err := r.s.Allocate(page.KindHeap); !errors.Is(err, ErrPoisoned) {
		r.fatalf("Allocate on a poisoned store gave %v", err)
	}
	if err := r.s.Free(1); !errors.Is(err, ErrPoisoned) {
		r.fatalf("Free on a poisoned store gave %v", err)
	}
	if err := r.s.Checkpoint(); !errors.Is(err, ErrPoisoned) {
		r.fatalf("Checkpoint on a poisoned store gave %v", err)
	}
	if err := r.s.Close(); !errors.Is(err, ErrPoisoned) {
		r.fatalf("Close of a poisoned store gave %v", err)
	}
	if r.disk.writes != writes || r.disk.mf.syncs != syncs {
		r.fatalf("a poisoned store went on to write %d times and fsync %d times",
			r.disk.writes-writes, r.disk.mf.syncs-syncs)
	}
}

func (r *oracleRun) stampPage(p page.Page) {
	r.stamp++
	for _, at := range []int{page.HeaderSize, page.Size / 2, page.Size - 8} {
		binary.BigEndian.PutUint64(p[at:], r.stamp*0x9E3779B97F4A7C15^uint64(at))
	}
}

func (r *oracleRun) handedOut(img *oracleImage, p page.Page, kind page.Kind) {
	r.t.Helper()
	r.checkGuard()
	id := p.ID()
	if id == MetaPage {
		r.fatalf("Allocate handed out the meta page")
	}
	if _, ok := r.pages[id]; ok {
		r.fatalf("Allocate handed out page %d while it is in use", id)
	}
	if r.pending[id] {
		r.fatalf("Allocate handed out page %d while it is pending until the next checkpoint", id)
	}
	if int(id) < len(img.state) && img.state[id] != oracleFree {
		r.fatalf("Allocate handed out page %d while the durable image, generation %d, says it is %s",
			id, img.meta.Checkpoint, oracleStateName(img.state[id]))
	}
	if len(p) != page.Size || p.Kind() != kind {
		r.fatalf("Allocate(%s) returned a %d-byte page of kind %s", kind, len(p), p.Kind())
	}
}

func (r *oracleRun) allocate(kind page.Kind, write bool) core.PageID {
	r.t.Helper()
	img := r.image()
	p, err := r.s.Allocate(kind)
	if err != nil {
		r.fatalf("Allocate: %v", err)
	}
	r.handedOut(img, p, kind)
	if write {
		r.stampPage(p)
		if err := r.s.Write(p); err != nil {
			r.fatalf("Write(%d): %v", p.ID(), err)
		}
	}
	r.pages[p.ID()] = append([]byte(nil), p...)
	r.fresh[p.ID()] = true
	r.dirty = true
	return p.ID()
}

func (r *oracleRun) rewrite(a byte) {
	r.t.Helper()
	ids := oracleKeys(r.fresh)
	if len(ids) == 0 {
		return
	}
	id := ids[int(a)%len(ids)]
	p := page.Page(append([]byte(nil), r.pages[id]...))
	r.stampPage(p)
	if err := r.s.Write(p); err != nil {
		r.fatalf("rewriting page %d: %v", id, err)
	}
	r.pages[id] = p
	r.dirty = true
}

func (r *oracleRun) freeID(id core.PageID) {
	r.t.Helper()
	if id == r.root {
		before := r.observe()
		if err := r.s.Free(id); !errors.Is(err, ErrFreeRoot) {
			r.fatalf("freeing the root %d gave %v, want ErrFreeRoot", id, err)
		}
		if after := r.observe(); !reflect.DeepEqual(before, after) {
			r.fatalf("a refused Free changed the store from %+v to %+v", before, after)
		}
		return
	}
	if err := r.s.Free(id); err != nil {
		r.fatalf("Free(%d): %v", id, err)
	}
	delete(r.pages, id)
	delete(r.fresh, id)
	r.pending[id] = true
	r.dirty = true
}

func (r *oracleRun) free(a byte) {
	r.t.Helper()
	ids := oracleKeys(r.pages)
	if len(ids) == 0 {
		return
	}
	r.freeID(ids[int(a)%len(ids)])
}

func (r *oracleRun) refuse(a, b byte) {
	r.t.Helper()
	var id core.PageID
	var freeWant, useWant error
	switch a % 5 {
	case 0:
		id, freeWant, useWant = MetaPage, ErrFreeMeta, ErrBadPageID
	case 1:
		ids := oracleKeys(r.pending)
		if len(ids) == 0 {
			return
		}
		id, freeWant, useWant = ids[int(b)%len(ids)], ErrDoubleFree, ErrNotAllocated
	case 2:
		ids := r.s.FreePages()
		if len(ids) == 0 {
			return
		}
		id, freeWant, useWant = ids[int(b)%len(ids)], ErrDoubleFree, ErrNotAllocated
	case 3:
		ids := r.s.ListPages()
		if len(ids) == 0 {
			return
		}
		id, freeWant, useWant = ids[int(b)%len(ids)], ErrNotAllocated, ErrNotAllocated
	case 4:
		id, freeWant, useWant = r.s.Meta().NextPage+core.PageID(b%3), ErrBadPageID, ErrBadPageID
	}
	before := r.observe()
	writes := r.disk.writes
	var err, want error
	switch (b >> 2) % 4 {
	case 0:
		err, want = r.s.Free(id), freeWant
	case 1:
		if id == NoPage {
			return
		}
		err, want = r.s.SetRoot(id), useWant
	case 2:
		_, err = r.s.Read(id)
		want = useWant
	case 3:
		err, want = r.s.Write(page.New(id, page.KindHeap)), useWant
	}
	if !errors.Is(err, want) {
		r.fatalf("a call on page %d, which the store does not own, gave %v, want %v", id, err, want)
	}
	if after := r.observe(); !reflect.DeepEqual(before, after) {
		r.fatalf("a refused call on page %d changed the store from %+v to %+v", id, before, after)
	}
	if r.disk.writes != writes {
		r.fatalf("a refused call on page %d wrote to the file", id)
	}
}

func (r *oracleRun) setRootTo(id core.PageID) {
	r.t.Helper()
	if err := r.s.SetRoot(id); err != nil {
		r.fatalf("SetRoot(%d): %v", id, err)
	}
	r.root = id
	r.dirty = true
}

func (r *oracleRun) setRoot(a byte) {
	r.t.Helper()
	ids := oracleKeys(r.pages)
	if len(ids) == 0 || a%5 == 0 {
		r.setRootTo(NoPage)
		return
	}
	r.setRootTo(ids[int(a)%len(ids)])
}

func (r *oracleRun) checkpoint() {
	r.t.Helper()
	want := r.snap(r.committed.gen + 1)
	if err := r.s.Checkpoint(); err != nil {
		r.fatalf("Checkpoint: %v", err)
	}
	r.adopt(want)
	r.checkDurable(want)
}

func (r *oracleRun) closeAndReopen(times int) {
	r.t.Helper()
	for j := 0; j < times; j++ {
		gen := r.committed.gen
		if err := r.s.Close(); err != nil {
			r.fatalf("Close: %v", err)
		}
		img, err := oracleDecode(r.disk.mf.durable)
		if err != nil {
			r.fatalf("after a clean Close the durable image does not decode: %v", err)
		}
		if last := r.s.Meta(); last != img.meta {
			r.fatalf("after a clean Close the disk holds %+v but the store's last Meta() was %+v", img.meta, last)
		}
		got := img.meta.Checkpoint
		if r.dirty && got != gen+1 {
			r.fatalf("Close left generation %d on disk although work was pending since generation %d", got, gen)
		}
		if !r.dirty && got != gen && got != gen+1 {
			r.fatalf("Close of a store with nothing pending left generation %d on disk after %d", got, gen)
		}
		want := r.snap(got)
		r.adopt(want)
		r.checkDurable(want)
		r.disk.mf.closed = false
		r.reopen()
	}
}

func (r *oracleRun) crash(how, seed byte) {
	r.s.Discard()
	switch how % 3 {
	case 0:
		r.disk.mf.restart()
	case 1:
		r.disk.mf.restartEvicting(oracleSubset(seed))
	case 2:
		r.disk.mf.powerLoss(oracleSubset(seed))
		r.disk.metaWritten = false
	}
	r.disk.forget()
	r.disk.failWrite = 0
}

func (r *oracleRun) reopen() {
	r.t.Helper()
	s, created, err := openFile(r.disk)
	if err != nil {
		r.fatalf("reopening: %v", err)
	}
	if created {
		r.fatalf("reopening an existing store created a fresh one")
	}
	r.s = s
	r.checkGuard()
	got := s.Durable().Checkpoint
	var want oracleSnap
	switch {
	case got == r.committed.gen:
		want = r.committed
	case r.candidate != nil && got == r.candidate.gen:
		want = *r.candidate
	case r.candidate != nil:
		r.fatalf("recovered generation %d; only %d, the last durable checkpoint, or %d, the one whose final fsync failed, are possible",
			got, r.committed.gen, r.candidate.gen)
	default:
		r.fatalf("recovered generation %d, want the last durable checkpoint %d", got, r.committed.gen)
	}
	r.adopt(want)
	r.checkDurable(want)
	for id, b := range want.pages {
		p, err := s.Read(id)
		if err != nil {
			r.fatalf("reading page %d of recovered generation %d: %v", id, want.gen, err)
		}
		if !bytes.Equal(p, b) {
			r.fatalf("page %d of recovered generation %d reads back differently, first at byte %d",
				id, want.gen, oracleFirstDiff(p, b))
		}
	}
}

func (r *oracleRun) failCheckpointSync(n int, keep func(int) bool) bool {
	r.t.Helper()
	want := r.snap(r.committed.gen + 1)
	mf := r.disk.mf
	r.disk.finalFailed = false
	mf.failAtSync, mf.failKeep = mf.syncs+n, keep
	err := r.s.Checkpoint()
	mf.failAtSync, mf.failKeep = 0, nil
	if err == nil {
		r.adopt(want)
		r.checkDurable(want)
		return false
	}
	if !errors.Is(err, ErrPoisoned) || !errors.Is(err, errSyncFailed) {
		r.fatalf("a checkpoint whose fsync failed gave %v, want ErrPoisoned keeping the fsync error", err)
	}
	if r.disk.finalFailed {
		r.candidate = &want
	}
	r.checkPoisoned()
	return true
}

func (r *oracleRun) fsyncFailure(a, b byte) {
	r.t.Helper()
	if !r.failCheckpointSync(1+int(a%3), oracleSubset(b)) {
		return
	}
	r.crash(a>>2, b>>2)
	r.reopen()
	if a&0x80 != 0 {
		r.crash(a>>4, b>>4)
		r.reopen()
	}
}

func (r *oracleRun) failCheckpointWrite(k int, how, seed byte, retry bool) (int64, int) {
	r.t.Helper()
	before := r.observe()
	want := r.snap(r.committed.gen + 1)
	r.disk.failedAt, r.disk.failedLen = -1, 0
	r.disk.failWrite = r.disk.writes + k
	err := r.s.Checkpoint()
	r.disk.failWrite = 0
	at, n := r.disk.failedAt, r.disk.failedLen
	switch {
	case at == -1:
		if err != nil {
			r.fatalf("Checkpoint: %v", err)
		}
		r.adopt(want)
		r.checkDurable(want)
	case err == nil:
		r.fatalf("Checkpoint succeeded although its write at offset %d failed", at)
	case !errors.Is(err, oracleErrWrite):
		r.fatalf("a checkpoint whose write failed gave %v, which does not keep the cause", err)
	case at < oraclePageSize:
		if !errors.Is(err, ErrPoisoned) {
			r.fatalf("a checkpoint whose meta write failed gave %v, want ErrPoisoned", err)
		}
		r.checkPoisoned()
		r.crash(how, seed)
		r.reopen()
	default:
		if errors.Is(err, ErrPoisoned) {
			r.fatalf("a write at offset %d failed before the meta page was touched, yet the store is poisoned: %v", at, err)
		}
		if after := r.observe(); !reflect.DeepEqual(before, after) {
			r.fatalf("a checkpoint whose write at offset %d failed changed the store from %+v to %+v", at, before, after)
		}
		r.checkLive()
		if retry {
			r.checkpoint()
		} else {
			r.crash(how, seed)
			r.reopen()
		}
	}
	return at, n
}

func (r *oracleRun) allocateFailure(a, b byte) {
	r.t.Helper()
	img := r.image()
	before := r.observe()
	r.disk.failedAt = -1
	r.disk.failWrite = r.disk.writes + 1 + int(a%2)
	p, err := r.s.Allocate(page.KindHeap)
	r.disk.failWrite = 0
	if err == nil {
		if r.disk.failedAt != -1 {
			r.fatalf("Allocate succeeded although its write at offset %d failed", r.disk.failedAt)
		}
		r.handedOut(img, p, page.KindHeap)
		r.pages[p.ID()] = append([]byte(nil), p...)
		r.fresh[p.ID()] = true
		r.dirty = true
		return
	}
	if !errors.Is(err, oracleErrWrite) || errors.Is(err, ErrPoisoned) {
		r.fatalf("an Allocate whose write failed gave %v, want the plain write error", err)
	}
	after := r.observe()
	if after.meta.NextPage != before.meta.NextPage || after.root != before.root || after.durable != before.durable ||
		after.dirty != before.dirty || !reflect.DeepEqual(after.free, before.free) ||
		!reflect.DeepEqual(after.pending, before.pending) || !reflect.DeepEqual(after.lists, before.lists) {
		r.fatalf("a failed Allocate changed the store from %+v to %+v", before, after)
	}
	r.crash(b, b>>2)
	r.reopen()
}

func (r *oracleRun) writeFailure(a byte) {
	r.t.Helper()
	ids := oracleKeys(r.fresh)
	if len(ids) == 0 {
		return
	}
	p := page.Page(append([]byte(nil), r.pages[ids[int(a)%len(ids)]]...))
	r.stampPage(p)
	r.disk.failWrite = r.disk.writes + 1
	err := r.s.Write(p)
	r.disk.failWrite = 0
	if !errors.Is(err, oracleErrWrite) || errors.Is(err, ErrPoisoned) {
		r.fatalf("a Write whose write failed gave %v, want the plain write error", err)
	}
}

func (r *oracleRun) storeSync(a, b byte) {
	r.t.Helper()
	mf := r.disk.mf
	if a%2 == 0 {
		if err := r.s.Sync(); err != nil {
			r.fatalf("Sync: %v", err)
		}
		return
	}
	mf.failAtSync, mf.failKeep = mf.syncs+1, oracleSubset(b)
	err := r.s.Sync()
	mf.failAtSync, mf.failKeep = 0, nil
	if !errors.Is(err, ErrPoisoned) || !errors.Is(err, errSyncFailed) {
		r.fatalf("a failed Sync gave %v, want ErrPoisoned keeping the fsync error", err)
	}
	r.checkPoisoned()
	r.crash(a>>1, b>>2)
	r.reopen()
}

func (r *oracleRun) reopenFailure(a, b byte) {
	r.t.Helper()
	r.crash(a, b)
	mf := r.disk.mf
	cause := errSyncFailed
	if a&0x40 != 0 {
		cause = oracleErrWrite
		r.disk.failWrite = r.disk.writes + 1
	} else {
		mf.failAtSync, mf.failKeep = mf.syncs+1, oracleSubset(b>>3)
	}
	s, _, err := openFile(r.disk)
	mf.failAtSync, mf.failKeep = 0, nil
	r.disk.failWrite = 0
	if err == nil {
		s.Discard()
		r.fatalf("a reopen whose rewrite of the meta page failed handed back a store")
	}
	if !errors.Is(err, cause) {
		r.fatalf("a reopen that failed with %v gave %v, which does not keep the cause", cause, err)
	}
	r.checkGuard()
	r.crash(a>>2, b>>1)
	r.reopen()
}

func (r *oracleRun) bulkOf(k int) {
	r.t.Helper()
	img := r.image()
	ids := make([]core.PageID, 0, k)
	for j := 0; j < k; j++ {
		p, err := r.s.Allocate(page.KindHeap)
		if err != nil {
			r.fatalf("Allocate %d of %d: %v", j, k, err)
		}
		r.handedOut(img, p, page.KindHeap)
		r.pages[p.ID()] = p
		ids = append(ids, p.ID())
	}
	for _, id := range ids {
		if err := r.s.Free(id); err != nil {
			r.fatalf("Free(%d): %v", id, err)
		}
		delete(r.pages, id)
		r.pending[id] = true
	}
	r.dirty = true
}

func (r *oracleRun) bulk(a, b byte) {
	r.t.Helper()
	k := (int(a%2)+1)*oracleListCap - 24 + int(b%48)
	if int(r.s.Meta().NextPage)+k-len(r.s.FreePages()) > oracleMaxPages {
		return
	}
	r.bulkOf(k)
}

func (r *oracleRun) corruptCopy(a, b byte) {
	r.t.Helper()
	img := r.image()
	target := MetaPage
	if n := len(img.chain); a%4 != 0 && n > 0 {
		target = img.chain[int(a%4-1)%n]
	}
	if err := oracleFlipAndOpen(r.disk.mf.durable, target, int(b%64), byte(1)<<((a>>2)%8)); err != nil {
		r.fatalf("%v", err)
	}
}

func (r *oracleRun) apply(op, a, b byte) {
	r.t.Helper()
	r.op = op % oracleOpCount
	kinds := []page.Kind{page.KindHeap, page.KindBTreeLeaf, page.KindBTreeInternal}
	switch r.op {
	case 0, 1:
		r.allocate(kinds[int(a)%len(kinds)], b%4 != 0)
	case 2:
		r.rewrite(a)
	case 3:
		r.free(a)
	case 4:
		r.refuse(a, b)
	case 5:
		r.setRoot(a)
	case 6:
		r.checkpoint()
	case 7:
		for j := 0; j < 2+int(a%6); j++ {
			r.checkpoint()
		}
	case 8:
		r.closeAndReopen(1 + int(a%3))
	case 9:
		r.crash(a, b)
		r.reopen()
	case 10:
		r.fsyncFailure(a, b)
	case 11:
		r.failCheckpointWrite(1+int(a%6), b, b>>2, a&0x40 != 0)
	case 12:
		r.bulk(a, b)
	case 13:
		r.crash(a, b)
		r.reopen()
		r.crash(a>>2, b>>2)
		r.reopen()
	case 14:
		r.storeSync(a, b)
	case 15:
		r.corruptCopy(a, b)
	case 16:
		r.allocateFailure(a, b)
	case 17:
		r.reopenFailure(a, b)
	case 18:
		r.writeFailure(a)
	}
}

func oracleOpen(mf *memFile) (s *Store, panicked any, err error) {
	defer func() {
		if p := recover(); p != nil {
			panicked = p
		}
	}()
	s, _, err = openFile(mf)
	return s, nil, err
}

func oracleRejection(target core.PageID, field int) error {
	if target != MetaPage {
		if field <= 4 || field == 6 || field == 7 {
			return ErrCorrupt
		}
		return nil
	}
	switch {
	case field <= 4:
		return ErrCorrupt
	case field >= 16 && field < 24:
		return ErrNotScuteDB
	case field >= 24 && field < 28:
		return ErrBadVersion
	case field >= 28 && field < 32:
		return ErrBadPageSize
	case field >= 40 && field < 44:
		return ErrCorrupt
	}
	return nil
}

func oracleFlipAndOpen(image []byte, target core.PageID, field int, mask byte) error {
	mf := newMemFile()
	mf.durable = append([]byte(nil), image...)
	mf.durable[int(target)*oraclePageSize+field] ^= mask
	mf.live = append([]byte(nil), mf.durable...)
	where := fmt.Sprintf("flipping bits %#02x of byte %d on page %d", mask, field, target)
	s, panicked, err := oracleOpen(mf)
	if panicked != nil {
		return fmt.Errorf("%s made Open panic: %v", where, panicked)
	}
	want := oracleRejection(target, field)
	if err != nil {
		if !errors.Is(err, ErrCorrupt) && !errors.Is(err, ErrNotScuteDB) &&
			!errors.Is(err, ErrBadVersion) && !errors.Is(err, ErrBadPageSize) {
			return fmt.Errorf("%s: Open gave %v, which is no class of rejection", where, err)
		}
		if want != nil && !errors.Is(err, want) {
			return fmt.Errorf("%s: Open gave %v, want %v", where, err, want)
		}
		return nil
	}
	defer s.Discard()
	if want != nil {
		return fmt.Errorf("%s: Open accepted the file, want %v", where, want)
	}
	img, derr := oracleDecode(mf.durable)
	if derr != nil {
		return fmt.Errorf("%s: Open accepted an image whose own bytes are inconsistent: %v", where, derr)
	}
	if got := s.Durable(); got != img.meta {
		return fmt.Errorf("%s: Open says the durable meta is %+v, the bytes say %+v", where, got, img.meta)
	}
	if !oracleSameOrder(oracleSorted(s.FreePages()), oracleSorted(img.entries)) || !oracleSameOrder(s.ListPages(), img.chain) {
		return fmt.Errorf("%s: Open reports free %s on pages %v, the bytes hold %s on pages %v", where,
			oracleShort(s.FreePages()), s.ListPages(), oracleShort(img.entries), img.chain)
	}
	if err := s.Verify(); err != nil {
		return fmt.Errorf("%s: Open accepted the file but Verify then rejected it: %v", where, err)
	}
	return nil
}

func oracleOps(triples ...[3]byte) []byte {
	out := make([]byte, 0, 3*len(triples))
	for _, t := range triples {
		out = append(out, t[:]...)
	}
	return out
}

var oracleSeeds = [][]byte{
	oracleOps(
		[3]byte{0, 0, 1}, [3]byte{0, 1, 1}, [3]byte{1, 2, 1}, [3]byte{0, 0, 0}, [3]byte{5, 1, 0},
		[3]byte{6, 0, 0}, [3]byte{3, 0, 0}, [3]byte{0, 1, 1}, [3]byte{2, 0, 0}, [3]byte{6, 0, 0},
		[3]byte{8, 2, 0}, [3]byte{7, 3, 0}, [3]byte{9, 0, 0}, [3]byte{9, 1, 6}, [3]byte{9, 2, 7},
		[3]byte{3, 1, 0}, [3]byte{8, 0, 0}, [3]byte{9, 2, 2},
	),
	oracleOps(
		[3]byte{0, 0, 1}, [3]byte{5, 1, 0}, [3]byte{12, 2, 24}, [3]byte{6, 0, 0}, [3]byte{8, 1, 0},
		[3]byte{13, 2, 0}, [3]byte{12, 0, 30}, [3]byte{7, 2, 0}, [3]byte{9, 2, 0}, [3]byte{15, 1, 7},
		[3]byte{15, 2, 3}, [3]byte{15, 3, 40}, [3]byte{15, 0, 46}, [3]byte{4, 3, 0}, [3]byte{4, 2, 5},
	),
	oracleOps(
		[3]byte{0, 0, 1}, [3]byte{0, 0, 1}, [3]byte{5, 1, 0}, [3]byte{6, 0, 0}, [3]byte{0, 0, 1},
		[3]byte{5, 2, 0}, [3]byte{3, 0, 0}, [3]byte{10, 0x82, 0}, [3]byte{0, 0, 1}, [3]byte{3, 1, 0},
		[3]byte{10, 0x86, 1}, [3]byte{0, 1, 1}, [3]byte{3, 0, 0}, [3]byte{10, 2, 2}, [3]byte{0, 1, 1},
		[3]byte{10, 1, 6}, [3]byte{0, 0, 1}, [3]byte{10, 0, 3}, [3]byte{10, 0x8A, 0},
	),
	oracleOps(
		[3]byte{0, 0, 1}, [3]byte{5, 1, 0}, [3]byte{6, 0, 0}, [3]byte{12, 0, 46}, [3]byte{11, 0, 1},
		[3]byte{11, 1, 0}, [3]byte{11, 0x41, 4}, [3]byte{11, 2, 2}, [3]byte{11, 0x42, 6}, [3]byte{11, 3, 5},
		[3]byte{0, 0, 1}, [3]byte{3, 1, 0}, [3]byte{11, 0, 0}, [3]byte{11, 1, 9}, [3]byte{6, 0, 0},
	),
	oracleOps(
		[3]byte{0, 0, 1}, [3]byte{0, 0, 1}, [3]byte{0, 0, 1}, [3]byte{0, 0, 1}, [3]byte{5, 1, 0},
		[3]byte{3, 0, 0}, [3]byte{3, 1, 0}, [3]byte{4, 1, 4}, [3]byte{4, 1, 8}, [3]byte{4, 1, 12},
		[3]byte{4, 1, 0}, [3]byte{6, 0, 0}, [3]byte{4, 2, 0}, [3]byte{4, 2, 5}, [3]byte{4, 2, 9},
		[3]byte{4, 2, 13}, [3]byte{4, 3, 0}, [3]byte{4, 3, 5}, [3]byte{4, 3, 9}, [3]byte{4, 3, 13},
		[3]byte{4, 4, 0}, [3]byte{4, 4, 5}, [3]byte{4, 4, 9}, [3]byte{4, 4, 13}, [3]byte{4, 0, 0},
		[3]byte{4, 0, 8}, [3]byte{4, 0, 12}, [3]byte{4, 0, 4}, [3]byte{16, 0, 5}, [3]byte{14, 0, 0},
		[3]byte{0, 0, 1}, [3]byte{16, 1, 6}, [3]byte{14, 1, 2}, [3]byte{0, 0, 1}, [3]byte{17, 0, 1},
		[3]byte{17, 0x42, 6}, [3]byte{0, 0, 1}, [3]byte{18, 0, 0}, [3]byte{2, 0, 0}, [3]byte{8, 0, 0},
	),
	oracleOps(
		[3]byte{0, 0, 1}, [3]byte{0, 1, 1}, [3]byte{0, 2, 1}, [3]byte{0, 0, 1}, [3]byte{0, 1, 1},
		[3]byte{0, 2, 1}, [3]byte{0, 0, 1}, [3]byte{0, 1, 1}, [3]byte{0, 2, 1}, [3]byte{0, 0, 1},
		[3]byte{0, 1, 1}, [3]byte{0, 2, 1}, [3]byte{0, 0, 1}, [3]byte{0, 1, 1}, [3]byte{0, 2, 1},
		[3]byte{16, 0, 0}, [3]byte{0, 0, 1}, [3]byte{0, 1, 1}, [3]byte{0, 2, 1}, [3]byte{0, 0, 1},
		[3]byte{0, 1, 1}, [3]byte{0, 2, 1}, [3]byte{0, 0, 1}, [3]byte{0, 1, 1}, [3]byte{0, 2, 1},
		[3]byte{0, 0, 1}, [3]byte{0, 1, 1}, [3]byte{0, 2, 1}, [3]byte{0, 0, 1}, [3]byte{0, 1, 1},
		[3]byte{0, 2, 1}, [3]byte{16, 1, 3}, [3]byte{6, 0, 0},
	),
	oracleOps(
		[3]byte{15, 0, 47}, [3]byte{15, 0, 4}, [3]byte{15, 0, 16}, [3]byte{15, 0, 24}, [3]byte{15, 0, 28},
		[3]byte{15, 0, 40}, [3]byte{15, 0, 32}, [3]byte{15, 0, 36}, [3]byte{15, 0, 51}, [3]byte{15, 4, 51},
		[3]byte{0, 0, 1}, [3]byte{0, 0, 1}, [3]byte{3, 0, 0}, [3]byte{6, 0, 0}, [3]byte{15, 1, 0},
		[3]byte{15, 1, 4}, [3]byte{15, 1, 6}, [3]byte{15, 1, 7}, [3]byte{15, 1, 16}, [3]byte{15, 1, 19},
		[3]byte{15, 1, 20}, [3]byte{15, 0, 44}, [3]byte{15, 0, 47},
	),
	oracleOps(
		[3]byte{0, 0, 1}, [3]byte{0, 0, 1}, [3]byte{0, 0, 1}, [3]byte{3, 0, 0}, [3]byte{6, 0, 0},
		[3]byte{7, 5, 0}, [3]byte{9, 1, 2}, [3]byte{7, 5, 0}, [3]byte{9, 2, 3}, [3]byte{13, 1, 1},
		[3]byte{8, 2, 0}, [3]byte{3, 0, 0}, [3]byte{8, 0, 0},
	),
}

func FuzzDiskImageAgreesWithTheStore(f *testing.F) {
	for _, seed := range oracleSeeds {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, ops []byte) {
		if len(ops) > 3*oracleMaxOps {
			ops = ops[:3*oracleMaxOps]
		}
		r := oracleStart(t)
		for i := 0; i < len(ops); i += 3 {
			r.step = i / 3
			r.apply(ops[i], oracleByte(ops, i+1), oracleByte(ops, i+2))
			r.checkLive()
		}
		r.step++
		r.closeAndReopen(1)
		r.checkLive()
		r.s.Discard()
	})
}

func TestAFreeListSpanningThreePagesReadsBackFromTheDiskThroughReopensAndCrashes(t *testing.T) {
	for _, k := range []int{2*oracleListCap + 1, 3 * oracleListCap} {
		r := oracleStart(t)
		r.setRootTo(r.allocate(page.KindBTreeLeaf, true))
		r.allocate(page.KindHeap, true)
		r.checkpoint()
		r.bulkOf(k)
		r.checkpoint()
		r.checkLive()
		if got := len(r.image().chain); got < 3 {
			t.Fatalf("%d freed pages sit on a durable chain of %d pages, want at least 3", k, got)
		}
		r.closeAndReopen(3)
		r.checkLive()
		r.crash(2, 0)
		r.reopen()
		r.crash(0, 0)
		r.reopen()
		r.checkLive()
		for j := 0; j < 5; j++ {
			r.checkpoint()
			r.checkLive()
		}
		r.crash(1, 1)
		r.reopen()
		r.checkLive()
		for len(r.s.FreePages()) > 0 {
			r.allocate(page.KindHeap, true)
		}
		r.checkLive()
		r.crash(2, 0)
		r.reopen()
		r.checkLive()
		if got := len(r.image().chain); got < 3 {
			t.Fatalf("after a power loss the durable chain has %d pages, want the %d-page chain back", got, 3)
		}
		r.s.Discard()
	}
}

func oracleCheckpointThatMustGrow(t *testing.T) *oracleRun {
	t.Helper()
	r := oracleStart(t)
	r.setRootTo(r.allocate(page.KindBTreeLeaf, true))
	r.checkpoint()
	r.bulkOf(1022)
	if m := r.s.Meta(); uint32(m.NextPage) != m.FilePages || len(r.s.FreePages()) != 0 {
		t.Fatalf("setup: next page %d, file pages %d, %d free; want a full file and nothing reusable",
			m.NextPage, m.FilePages, len(r.s.FreePages()))
	}
	return r
}

func TestAFailedWriteInsideACheckpointLeavesTheStoreExactlyAsItWas(t *testing.T) {
	sawGrow, sawList, sawMeta := false, false, false
	for k := 1; k < 32; k++ {
		r := oracleCheckpointThatMustGrow(t)
		at, n := r.failCheckpointWrite(k, byte(k), byte(3*k), k%2 == 0)
		r.checkLive()
		switch {
		case at == -1:
			if !sawGrow || !sawList || !sawMeta {
				t.Fatalf("the checkpoint succeeded at write %d before failures were seen in the growth %v, "+
					"a free-list page %v and the meta page %v", k, sawGrow, sawList, sawMeta)
			}
			r.closeAndReopen(1)
			r.checkLive()
			r.s.Discard()
			return
		case at == 0:
			sawMeta = true
		case n > oraclePageSize:
			sawGrow = true
		default:
			sawList = true
		}
		r.s.Discard()
	}
	t.Fatal("the checkpoint never got past its injected write failures")
}

func TestARecoveredCheckpointSurvivesACrashRightAfterTheReopen(t *testing.T) {
	keeps := []struct {
		name string
		keep func(int) bool
	}{
		{"nothing", func(int) bool { return false }},
		{"everything", func(int) bool { return true }},
		{"odd writes", func(i int) bool { return i%2 == 1 }},
		{"even writes", func(i int) bool { return i%2 == 0 }},
	}
	for failAt := 1; failAt <= 3; failAt++ {
		for _, k := range keeps {
			name, keep := k.name, k.keep
			for first := byte(0); first < 3; first++ {
				for second := byte(0); second < 3; second++ {
					r := oracleStart(t)
					old := r.allocate(page.KindHeap, true)
					r.allocate(page.KindHeap, true)
					r.setRootTo(old)
					r.checkpoint()
					r.setRootTo(r.allocate(page.KindHeap, true))
					r.freeID(old)
					r.allocate(page.KindBTreeInternal, true)
					before := r.committed.gen
					if !r.failCheckpointSync(failAt, keep) {
						t.Fatalf("fsync %d of a checkpoint that writes a free list did not happen", failAt)
					}
					r.crash(first, 1)
					r.reopen()
					chosen := r.committed.gen
					if chosen != before && chosen != before+1 {
						t.Fatalf("recovered generation %d after %d", chosen, before)
					}
					r.checkLive()
					r.crash(second, 0)
					r.reopen()
					if r.committed.gen != chosen {
						t.Fatalf("fsync %d failed keeping %s, crashes %d then %d: the reopen reported generation %d, "+
							"a crash right after it recovered %d", failAt, name, first, second, chosen, r.committed.gen)
					}
					for len(r.s.FreePages()) > 0 {
						r.allocate(page.KindHeap, true)
					}
					r.checkLive()
					r.crash(2, 0)
					r.reopen()
					r.checkLive()
					r.s.Discard()
				}
			}
		}
	}
}

func TestEveryBitFlipInTheMetaOrFreeListHeaderIsRejectedOrAgreesWithTheDisk(t *testing.T) {
	fresh := oracleStart(t)
	freshImage := append([]byte(nil), fresh.disk.mf.durable...)
	fresh.s.Discard()

	r := oracleStart(t)
	var ids []core.PageID
	for i := 0; i < 8; i++ {
		ids = append(ids, r.allocate(page.KindBTreeLeaf, true))
	}
	r.setRootTo(ids[2])
	for _, id := range []core.PageID{ids[0], ids[4], ids[5], ids[7]} {
		r.freeID(id)
	}
	r.checkpoint()
	if len(r.image().chain) != 1 {
		t.Fatalf("setup: want one free-list page, the chain is %v", r.image().chain)
	}
	listImage := append([]byte(nil), r.disk.mf.durable...)
	chain := r.image().chain[0]
	r.s.Discard()

	cases := []struct {
		name    string
		image   []byte
		targets []core.PageID
	}{
		{"a fresh store", freshImage, []core.PageID{MetaPage}},
		{"a store with a free list", listImage, []core.PageID{MetaPage, chain}},
	}
	for _, c := range cases {
		for _, target := range c.targets {
			for field := 0; field < 64; field++ {
				for bit := 0; bit < 8; bit++ {
					if err := oracleFlipAndOpen(c.image, target, field, byte(1)<<bit); err != nil {
						t.Errorf("%s: %v", c.name, err)
					}
				}
			}
		}
	}
}

func oracleSorted(ids []core.PageID) []core.PageID {
	out := append([]core.PageID(nil), ids...)
	sort.Slice(out, func(a, b int) bool { return out[a] < out[b] })
	return out
}
