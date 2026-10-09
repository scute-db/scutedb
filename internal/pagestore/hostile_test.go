package pagestore

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/scute-db/scutedb/internal/core"
	"github.com/scute-db/scutedb/internal/page"
)

type hostileShape struct {
	meta  Meta
	root  core.PageID
	lists []core.PageID
	free  []core.PageID
	inUse []core.PageID
}

func hostileShapeOf(s *Store) hostileShape {
	sh := hostileShape{meta: s.Durable(), root: s.Root(), lists: s.ListPages(), free: s.FreePages()}
	taken := map[core.PageID]bool{}
	for _, id := range sh.lists {
		taken[id] = true
	}
	for _, id := range sh.free {
		taken[id] = true
	}
	for id := core.PageID(1); id < sh.meta.NextPage; id++ {
		if !taken[id] {
			sh.inUse = append(sh.inUse, id)
		}
	}
	return sh
}

func (sh hostileShape) others() []core.PageID {
	var out []core.PageID
	for _, id := range sh.inUse {
		if id != sh.root {
			out = append(out, id)
		}
	}
	return out
}

func (sh hostileShape) listAt(i int) int { return int(page.Offset(sh.lists[i])) }

func (sh hostileShape) entryAt(j int) int { return sh.listAt(0) + FreeListHeader + 4*j }

func hostileRichWorkload(s *Store) error {
	var ids []core.PageID
	for i := 0; i < 8; i++ {
		p, err := s.Allocate(page.KindHeap)
		if err != nil {
			return err
		}
		copy(p[page.HeaderSize:], fmt.Sprintf("hostile payload of page %d", p.ID()))
		if err := s.Write(p); err != nil {
			return err
		}
		ids = append(ids, p.ID())
	}
	if err := s.SetRoot(ids[0]); err != nil {
		return err
	}
	for _, id := range ids[2:5] {
		if err := s.Free(id); err != nil {
			return err
		}
	}
	if err := s.Checkpoint(); err != nil {
		return err
	}
	if err := s.Free(ids[5]); err != nil {
		return err
	}
	return s.Checkpoint()
}

func hostileRichReady(raw []byte, sh hostileShape) error {
	if sh.root == NoPage {
		return errors.New("the rich fixture has no root")
	}
	if len(sh.lists) == 0 || sh.meta.FreeListPage != sh.lists[0] {
		return fmt.Errorf("the rich fixture has list pages %v and head %d", sh.lists, sh.meta.FreeListPage)
	}
	if len(sh.others()) < 2 {
		return fmt.Errorf("the rich fixture has in-use pages %v, want two besides the root", sh.inUse)
	}
	if uint32(sh.meta.NextPage)+1 >= sh.meta.FilePages {
		return fmt.Errorf("the rich fixture leaves no never-handed-out page: %+v", sh.meta)
	}
	if len(raw) != int(sh.meta.FilePages)*page.Size {
		return fmt.Errorf("the rich fixture is %d bytes, want %d pages", len(raw), sh.meta.FilePages)
	}
	head := page.Page(raw[sh.listAt(0) : sh.listAt(0)+page.Size])
	if head.ItemCount() < 2 {
		return fmt.Errorf("the rich fixture's head list page holds %d entries, want at least 2", head.ItemCount())
	}
	return nil
}

func hostileStoreFile(t *testing.T, rich bool) ([]byte, hostileShape) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fixture.db")
	s := mustCreate(t, path)
	if rich {
		if err := hostileRichWorkload(s); err != nil {
			t.Fatal(err)
		}
	}
	sh := hostileShapeOf(s)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if rich {
		if err := hostileRichReady(raw, sh); err != nil {
			t.Fatal(err)
		}
	}
	return raw, sh
}

func hostilePut(img []byte, off int, v uint32) { binary.BigEndian.PutUint32(img[off:], v) }

func hostileView(img []byte, id core.PageID) page.Page {
	off := int(page.Offset(id))
	return page.Page(img[off : off+page.Size])
}

func hostileSet(off int, v func(hostileShape) uint32) func([]byte, hostileShape) []byte {
	return func(img []byte, sh hostileShape) []byte {
		hostilePut(img, off, v(sh))
		return img
	}
}

func hostileForge(img []byte, sh hostileShape, id, next core.PageID, entries ...core.PageID) []byte {
	p := page.New(id, page.KindFreeList)
	binary.BigEndian.PutUint32(p[offFreeListNext:], uint32(next))
	for j, e := range entries {
		binary.BigEndian.PutUint32(p[FreeListHeader+4*j:], uint32(e))
	}
	p.SetItemCount(uint16(len(entries)))
	p.SetFreeStart(uint16(FreeListHeader + 4*len(entries)))
	copy(img[page.Offset(id):], p)
	hostilePut(img, sh.listAt(len(sh.lists)-1)+offFreeListNext, uint32(id))
	hostilePut(img, offFreeCount, sh.meta.FreeCount+uint32(len(entries)))
	return img
}

func hostileIsAny(err error, want ...error) bool {
	for _, w := range want {
		if errors.Is(err, w) {
			return true
		}
	}
	return false
}

func hostileFirstDiff(a, b []byte) string {
	if len(a) != len(b) {
		return fmt.Sprintf("size went from %d to %d bytes", len(a), len(b))
	}
	for i := range a {
		if a[i] != b[i] {
			return fmt.Sprintf("byte %d (page %d) went from %02X to %02X", i, i/page.Size, a[i], b[i])
		}
	}
	return "no difference"
}

func hostileMem(img []byte) *memFile {
	return &memFile{durable: append([]byte(nil), img...), live: append([]byte(nil), img...)}
}

func hostileExpectRejected(t *testing.T, path string, img []byte, want []error) {
	t.Helper()
	if err := os.WriteFile(path, img, 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := Open(path)
	if err == nil {
		s.Discard()
		t.Errorf("Open accepted the damaged file; want one of %v", want)
	} else if !hostileIsAny(err, want...) {
		t.Errorf("Open gave %v; want one of %v", err, want)
	}
	after, rerr := os.ReadFile(path)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if err != nil && !bytes.Equal(after, img) {
		t.Errorf("Open rejected the file but changed it: %s", hostileFirstDiff(img, after))
	}

	mf := hostileMem(img)
	ms, _, merr := openFile(mf)
	if merr == nil {
		ms.Discard()
		t.Errorf("openFile over a memFile accepted the damaged image; want one of %v", want)
		return
	}
	if !hostileIsAny(merr, want...) {
		t.Errorf("openFile over a memFile gave %v; want one of %v", merr, want)
	}
	if len(mf.log) != 0 || mf.syncs != 0 {
		t.Errorf("rejecting the image left %d unsynced writes and issued %d fsyncs; a rejected file must not be touched",
			len(mf.log), mf.syncs)
	}
	if !bytes.Equal(mf.live, img) || !bytes.Equal(mf.durable, img) {
		t.Errorf("rejecting the image changed it: %s", hostileFirstDiff(img, mf.live))
	}
}

type hostileCase struct {
	name   string
	rich   bool
	want   []error
	mutate func(img []byte, sh hostileShape) []byte
}

func hostileCases() []hostileCase {
	corrupt := []error{ErrCorrupt}
	u := func(v uint32) func(hostileShape) uint32 { return func(hostileShape) uint32 { return v } }
	id := func(f func(hostileShape) core.PageID) func(hostileShape) uint32 {
		return func(sh hostileShape) uint32 { return uint32(f(sh)) }
	}
	next := id(func(sh hostileShape) core.PageID { return sh.meta.NextPage })
	unused := func(sh hostileShape) uint32 { return sh.meta.FilePages - 1 }
	pastFile := func(sh hostileShape) uint32 { return sh.meta.FilePages + ChunkPages }
	root := id(func(sh hostileShape) core.PageID { return sh.root })
	head := id(func(sh hostileShape) core.PageID { return sh.lists[0] })
	other := func(i int) func(hostileShape) uint32 {
		return func(sh hostileShape) uint32 { return uint32(sh.others()[i]) }
	}
	onHead := func(f func(p page.Page, sh hostileShape)) func([]byte, hostileShape) []byte {
		return func(img []byte, sh hostileShape) []byte {
			f(hostileView(img, sh.lists[0]), sh)
			return img
		}
	}
	onMeta := func(f func(p page.Page)) func([]byte, hostileShape) []byte {
		return func(img []byte, sh hostileShape) []byte {
			f(page.Page(img[:page.Size]))
			return img
		}
	}
	entry := func(j int, v func(hostileShape) uint32) func([]byte, hostileShape) []byte {
		return func(img []byte, sh hostileShape) []byte {
			hostilePut(img, sh.entryAt(j), v(sh))
			return img
		}
	}
	forge := func(listID func(hostileShape) core.PageID, nextID func(hostileShape) core.PageID,
		entries ...func(hostileShape) uint32) func([]byte, hostileShape) []byte {
		return func(img []byte, sh hostileShape) []byte {
			ids := make([]core.PageID, len(entries))
			for i, e := range entries {
				ids[i] = core.PageID(e(sh))
			}
			return hostileForge(img, sh, listID(sh), nextID(sh), ids...)
		}
	}
	spare := func(sh hostileShape) core.PageID { return sh.others()[1] }
	end := func(hostileShape) core.PageID { return NoPage }
	cut := func(n func(hostileShape) int) func([]byte, hostileShape) []byte {
		return func(img []byte, sh hostileShape) []byte { return img[:n(sh)] }
	}

	return []hostileCase{
		{"the next page is 0, the meta page itself", true, corrupt, hostileSet(offNextPage, u(0))},
		{"the next page is one past the pages the meta says the file holds", true, corrupt,
			hostileSet(offNextPage, func(sh hostileShape) uint32 { return sh.meta.FilePages + 1 })},
		{"the next page and the file pages both run past the end of the file", true, corrupt,
			func(img []byte, sh hostileShape) []byte {
				hostilePut(img, offNextPage, pastFile(sh))
				hostilePut(img, offFilePages, pastFile(sh))
				return img
			}},
		{"the file pages claim one page more than the file holds", true, corrupt,
			hostileSet(offFilePages, func(sh hostileShape) uint32 { return sh.meta.FilePages + 1 })},
		{"the file pages claim a whole chunk more than the file holds", true, corrupt, hostileSet(offFilePages, pastFile)},
		{"the root is the next page to hand out", true, corrupt, hostileSet(offRoot, next)},
		{"the root is a page that was never handed out", true, corrupt, hostileSet(offRoot, unused)},
		{"the root is past the end of the file", true, corrupt, hostileSet(offRoot, pastFile)},
		{"the root is the largest page id", true, corrupt, hostileSet(offRoot, u(MaxPages))},
		{"the root is the head free-list page", true, corrupt, hostileSet(offRoot, head)},
		{"the root is a free page", true, corrupt,
			hostileSet(offRoot, id(func(sh hostileShape) core.PageID { return sh.free[0] }))},
		{"the free count is one lower than the list holds", true, corrupt,
			hostileSet(offFreeCount, func(sh hostileShape) uint32 { return sh.meta.FreeCount - 1 })},
		{"the free count is one higher than the list holds", true, corrupt,
			hostileSet(offFreeCount, func(sh hostileShape) uint32 { return sh.meta.FreeCount + 1 })},
		{"the free count is 0 although the list holds pages", true, corrupt, hostileSet(offFreeCount, u(0))},
		{"the free count is the largest count", true, corrupt, hostileSet(offFreeCount, u(MaxPages))},
		{"the free list head is missing although the count is not", true, corrupt,
			hostileSet(offFreeList, u(uint32(NoPage)))},
		{"the free list head is the next page to hand out", true, corrupt, hostileSet(offFreeList, next)},
		{"the free list head is a page that was never handed out", true, corrupt, hostileSet(offFreeList, unused)},
		{"the free list head is past the end of the file", true, corrupt, hostileSet(offFreeList, pastFile)},
		{"the free list head is the largest page id", true, corrupt, hostileSet(offFreeList, u(MaxPages))},
		{"the free list head is the root", true, corrupt, hostileSet(offFreeList, root)},
		{"the free list head is an in-use page", true, corrupt, hostileSet(offFreeList, other(0))},
		{"the list page has kind heap", true, corrupt,
			onHead(func(p page.Page, _ hostileShape) { p.SetKind(page.KindHeap) })},
		{"the list page has kind free", true, corrupt,
			onHead(func(p page.Page, _ hostileShape) { p.SetKind(page.KindFree) })},
		{"the list page has kind meta", true, corrupt,
			onHead(func(p page.Page, _ hostileShape) { p.SetKind(page.KindMeta) })},
		{"the list page has a kind this build has never heard of", true, corrupt,
			onHead(func(p page.Page, _ hostileShape) { p.SetKind(page.Kind(0xEE)) })},
		{"the list page header names an in-use page", true, corrupt,
			onHead(func(p page.Page, sh hostileShape) { p.SetID(sh.others()[0]) })},
		{"the list page header names the meta page", true, corrupt,
			onHead(func(p page.Page, _ hostileShape) { p.SetID(MetaPage) })},
		{"the list page claims one entry more than fits", true, corrupt,
			onHead(func(p page.Page, _ hostileShape) { p.SetItemCount(FreeListCapacity + 1) })},
		{"the list page claims 65535 entries", true, corrupt,
			onHead(func(p page.Page, _ hostileShape) { p.SetItemCount(0xFFFF) })},
		{"a free entry is page 0", true, corrupt, entry(0, u(uint32(MetaPage)))},
		{"a free entry is the next page to hand out", true, corrupt, entry(0, next)},
		{"a free entry is a page that was never handed out", true, corrupt, entry(0, unused)},
		{"a free entry is past the end of the file", true, corrupt, entry(0, pastFile)},
		{"a free entry is the largest page id", true, corrupt, entry(0, u(MaxPages))},
		{"a free entry appears twice", true, corrupt,
			func(img []byte, sh hostileShape) []byte {
				copy(img[sh.entryAt(1):sh.entryAt(1)+4], img[sh.entryAt(0):sh.entryAt(0)+4])
				return img
			}},
		{"a free entry is the root", true, corrupt, entry(0, root)},
		{"a free entry is the list page that holds it", true, corrupt, entry(0, head)},
		{"the list chain continues into an in-use page", true, corrupt,
			onHead(func(p page.Page, sh hostileShape) {
				binary.BigEndian.PutUint32(p[offFreeListNext:], uint32(sh.others()[0]))
			})},
		{"the list chain continues into the next page to hand out", true, corrupt,
			onHead(func(p page.Page, sh hostileShape) {
				binary.BigEndian.PutUint32(p[offFreeListNext:], uint32(sh.meta.NextPage))
			})},
		{"the list chain continues past the end of the file", true, corrupt,
			onHead(func(p page.Page, sh hostileShape) {
				binary.BigEndian.PutUint32(p[offFreeListNext:], pastFile(sh))
			})},
		{"the list chain continues to the largest page id", true, corrupt,
			onHead(func(p page.Page, _ hostileShape) { binary.BigEndian.PutUint32(p[offFreeListNext:], MaxPages) })},
		{"a second list page repeats an entry of the first", true, corrupt,
			forge(spare, end, id(func(sh hostileShape) core.PageID { return sh.free[0] }))},
		{"a second list page names the head list page as free", true, corrupt, forge(spare, end, head)},
		{"a second list page names itself as free", true, corrupt, forge(spare, end, other(1))},
		{"a second list page names the root as free", true, corrupt, forge(spare, end, root)},
		{"a second list page names page 0 as free", true, corrupt, forge(spare, end, u(uint32(MetaPage)))},
		{"a second list page names a page never handed out", true, corrupt, forge(spare, end, next)},
		{"a second list page points back at the head", true, corrupt,
			forge(spare, func(sh hostileShape) core.PageID { return sh.lists[0] }, other(0))},
		{"a second list page points at itself", true, corrupt, forge(spare, spare, other(0))},
		{"a second list page is the root", true, corrupt,
			forge(func(sh hostileShape) core.PageID { return sh.root }, end, other(0))},
		{"the page size is 8192", true, []error{ErrBadPageSize}, hostileSet(offPageSize, u(8192))},
		{"the page size is 2048", true, []error{ErrBadPageSize}, hostileSet(offPageSize, u(2048))},
		{"the page size is 0", true, []error{ErrBadPageSize}, hostileSet(offPageSize, u(0))},
		{"the page size is one byte larger", true, []error{ErrBadPageSize}, hostileSet(offPageSize, u(page.Size+1))},
		{"the version is one newer", true, []error{ErrBadVersion}, hostileSet(offVersion, u(Version+1))},
		{"the version is 0", true, []error{ErrBadVersion}, hostileSet(offVersion, u(0))},
		{"the version is the largest version", true, []error{ErrBadVersion}, hostileSet(offVersion, u(0xFFFFFFFF))},
		{"the first magic byte is changed", true, []error{ErrNotScuteDB},
			func(img []byte, _ hostileShape) []byte { img[offMagic] ^= 0x20; return img }},
		{"the magic lost its terminating NUL", true, []error{ErrNotScuteDB},
			func(img []byte, _ hostileShape) []byte { img[offMagic+len(Magic)-1] = '!'; return img }},
		{"the meta page is wiped to zeros while the rest of the store remains", true,
			[]error{ErrNotScuteDB, ErrCorrupt},
			func(img []byte, _ hostileShape) []byte {
				copy(img[:page.Size], make([]byte, page.Size))
				return img
			}},
		{"the meta page is wiped and the file is cut back to the pages handed out", true,
			[]error{ErrNotScuteDB, ErrCorrupt},
			func(img []byte, sh hostileShape) []byte {
				copy(img[:page.Size], make([]byte, page.Size))
				return img[:int(sh.meta.NextPage)*page.Size]
			}},
		{"the meta page is wiped and only the root follows it", true,
			[]error{ErrNotScuteDB, ErrCorrupt},
			func(img []byte, sh hostileShape) []byte {
				copy(img[:page.Size], make([]byte, page.Size))
				copy(img[page.Size:], img[page.Offset(sh.root):page.Offset(sh.root)+page.Size])
				return img[:2*page.Size]
			}},
		{"page 0 says it is page 1", true, corrupt, onMeta(func(p page.Page) { p.SetID(1) })},
		{"page 0 says it is the largest page id", true, corrupt, onMeta(func(p page.Page) { p.SetID(core.PageID(MaxPages)) })},
		{"page 0 has kind free-list", true, corrupt, onMeta(func(p page.Page) { p.SetKind(page.KindFreeList) })},
		{"page 0 has kind free", true, corrupt, onMeta(func(p page.Page) { p.SetKind(page.KindFree) })},
		{"page 0 has a kind this build has never heard of", true, corrupt, onMeta(func(p page.Page) { p.SetKind(page.Kind(0xEE)) })},
		{"the file is cut one byte short of a page", true, corrupt, cut(func(hostileShape) int { return page.Size - 1 })},
		{"the file is cut just after the magic", true, corrupt, cut(func(hostileShape) int { return offMagic + len(Magic) })},
		{"the file is cut one byte short of the pages it claims", true, corrupt,
			cut(func(sh hostileShape) int { return int(sh.meta.FilePages)*page.Size - 1 })},
		{"the file is cut one page short of the pages it claims", true, corrupt,
			cut(func(sh hostileShape) int { return int(sh.meta.FilePages-1) * page.Size })},
		{"the file is cut back to the pages handed out", true, corrupt,
			cut(func(sh hostileShape) int { return int(sh.meta.NextPage) * page.Size })},
		{"the file is cut back to its meta page", true, corrupt, cut(func(hostileShape) int { return page.Size })},

		{"the next page of a fresh store is 0", false, corrupt, hostileSet(offNextPage, u(0))},
		{"the file pages of a fresh store are 0", false, corrupt, hostileSet(offFilePages, u(0))},
		{"the root of a fresh store is page 1, which was never handed out", false, corrupt, hostileSet(offRoot, u(1))},
		{"a fresh store claims one free page with no list", false, corrupt, hostileSet(offFreeCount, u(1))},
		{"the free list head of a fresh store is page 1, which was never handed out", false, corrupt,
			hostileSet(offFreeList, u(1))},
		{"a fresh store is cut back to its meta page", false, corrupt, cut(func(hostileShape) int { return page.Size })},
		{"the version of a fresh store is 0", false, []error{ErrBadVersion}, hostileSet(offVersion, u(0))},
	}
}

func hostileOpensCleanly(t *testing.T, path string, raw []byte, sh hostileShape) {
	t.Helper()
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := Open(path)
	if err != nil {
		t.Fatalf("the undamaged fixture does not open: %v", err)
	}
	defer s.Close()
	if err := s.Verify(); err != nil {
		t.Fatalf("the undamaged fixture does not verify: %v", err)
	}
	if got := hostileShapeOf(s); fmt.Sprint(got) != fmt.Sprint(sh) {
		t.Fatalf("the undamaged fixture reopened as %+v, it was written as %+v", got, sh)
	}
}

func TestOpenRejectsEveryHostileRewriteOfACheckpointedStoreAndLeavesTheFileAsItWas(t *testing.T) {
	rich, richShape := hostileStoreFile(t, true)
	bare, bareShape := hostileStoreFile(t, false)
	dir := t.TempDir()
	hostileOpensCleanly(t, filepath.Join(dir, "rich.db"), rich, richShape)
	hostileOpensCleanly(t, filepath.Join(dir, "bare.db"), bare, bareShape)

	for i, c := range hostileCases() {
		c, path := c, filepath.Join(dir, fmt.Sprintf("case%03d.db", i))
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			src, sh := bare, bareShape
			if c.rich {
				src, sh = rich, richShape
			}
			img := c.mutate(append([]byte(nil), src...), sh)
			if bytes.Equal(img, src) {
				t.Fatal("the mutation changed nothing, so the case tests nothing")
			}
			hostileExpectRejected(t, path, img, c.want)
		})
	}
}

const hostileMaxImage = 4 * ChunkPages * page.Size

var hostileBase struct {
	once  sync.Once
	rich  []byte
	bare  []byte
	shape hostileShape
	err   error
}

func hostileMemImage(rich bool) ([]byte, hostileShape, error) {
	mf := newMemFile()
	s, err := create(page.NewFile(mf))
	if err != nil {
		return nil, hostileShape{}, err
	}
	if rich {
		if err := hostileRichWorkload(s); err != nil {
			return nil, hostileShape{}, err
		}
	}
	sh := hostileShapeOf(s)
	if err := s.Close(); err != nil {
		return nil, hostileShape{}, err
	}
	return append([]byte(nil), mf.live...), sh, nil
}

func hostileImages(tb testing.TB) ([]byte, []byte, hostileShape) {
	tb.Helper()
	hostileBase.once.Do(func() {
		hostileBase.rich, hostileBase.shape, hostileBase.err = hostileMemImage(true)
		if hostileBase.err == nil {
			hostileBase.err = hostileRichReady(hostileBase.rich, hostileBase.shape)
		}
		if hostileBase.err == nil {
			hostileBase.bare, _, hostileBase.err = hostileMemImage(false)
		}
	})
	if hostileBase.err != nil {
		tb.Fatalf("building the fuzz base images: %v", hostileBase.err)
	}
	return hostileBase.rich, hostileBase.bare, hostileBase.shape
}

type hostilePatch struct {
	off  int
	data []byte
}

func hostileU32(v uint32) []byte {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], v)
	return b[:]
}

func hostileEncode(mode byte, cut int, patches ...hostilePatch) []byte {
	out := []byte{mode}
	if cut >= 0 {
		out[0] |= 4
		out = append(out, byte(cut>>16), byte(cut>>8), byte(cut))
	}
	for _, p := range patches {
		out = append(out, byte(p.off>>16), byte(p.off>>8), byte(p.off), byte(len(p.data)))
		out = append(out, p.data...)
	}
	return out
}

func hostileDecode(data, rich, bare []byte) []byte {
	if len(data) == 0 {
		return nil
	}
	mode, rest := data[0], data[1:]
	var img []byte
	switch mode & 3 {
	case 1:
		img = append([]byte(nil), rich...)
	case 2:
		img = append([]byte(nil), bare...)
	default:
		if len(rest) > hostileMaxImage {
			rest = rest[:hostileMaxImage]
		}
		return append([]byte(nil), rest...)
	}
	if mode&4 != 0 && len(rest) >= 3 {
		n := (int(rest[0])<<16 | int(rest[1])<<8 | int(rest[2])) % (hostileMaxImage + 1)
		rest = rest[3:]
		if n <= len(img) {
			img = img[:n]
		} else {
			img = append(img, make([]byte, n-len(img))...)
		}
	}
	for len(rest) >= 4 && len(img) > 0 {
		off := (int(rest[0])<<16 | int(rest[1])<<8 | int(rest[2])) % len(img)
		n := int(rest[3])
		rest = rest[4:]
		if n > len(rest) {
			n = len(rest)
		}
		copy(img[off:], rest[:n])
		rest = rest[n:]
	}
	return img
}

func hostileInterruptedCreate(img []byte) bool {
	if len(img) > ChunkPages*page.Size {
		return false
	}
	for _, b := range img {
		if b != 0 {
			return false
		}
	}
	return true
}

func hostileExercise(t *testing.T, mf *memFile, s *Store) {
	t.Helper()
	if err := s.Verify(); err != nil {
		t.Fatalf("Open accepted a file that Verify rejects: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close of a freshly opened store: %v", err)
	}
	mf.restart()
	s, _, err := openFile(mf)
	if err != nil {
		t.Fatalf("a file Open accepted and Close closed no longer opens: %v", err)
	}
	if err := s.Verify(); err != nil {
		t.Fatalf("Verify after reopening: %v", err)
	}

	m := s.Meta()
	lists := map[core.PageID]bool{}
	for _, id := range s.ListPages() {
		lists[id] = true
	}
	free := map[core.PageID]bool{}
	for _, id := range s.FreePages() {
		free[id] = true
	}
	held := map[core.PageID][]byte{}
	for id := core.PageID(1); id < m.NextPage; id++ {
		if lists[id] || free[id] {
			continue
		}
		p, err := s.Read(id)
		if err != nil {
			t.Fatalf("reading in-use page %d: %v", id, err)
		}
		held[id] = p
	}

	p, err := s.Allocate(page.KindHeap)
	if err != nil {
		t.Fatalf("Allocate on a store Open accepted: %v", err)
	}
	if p.ID() == MetaPage || uint32(p.ID()) >= uint32(s.Meta().NextPage) {
		t.Fatalf("Allocate handed out page %d with next page %d", p.ID(), s.Meta().NextPage)
	}
	if _, inUse := held[p.ID()]; inUse {
		t.Fatalf("Allocate handed out page %d, which the durable state holds in use", p.ID())
	}
	if lists[p.ID()] {
		t.Fatalf("Allocate handed out page %d, which holds the durable free list", p.ID())
	}
	copy(p[page.HeaderSize:], "hostile fuzz page")
	if err := s.Write(p); err != nil {
		t.Fatal(err)
	}
	if err := s.SetRoot(p.ID()); err != nil {
		t.Fatal(err)
	}
	if err := s.Checkpoint(); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	if err := s.Verify(); err != nil {
		t.Fatalf("Verify after a checkpoint: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	mf.restart()

	back, _, err := openFile(mf)
	if err != nil {
		t.Fatalf("reopening after a checkpoint: %v", err)
	}
	defer back.Close()
	if err := back.Verify(); err != nil {
		t.Fatalf("Verify after the last reopen: %v", err)
	}
	if back.Root() != p.ID() {
		t.Fatalf("the root is %d after a checkpoint that set it to %d", back.Root(), p.ID())
	}
	got, err := back.Read(p.ID())
	if err != nil || !bytes.HasPrefix(got[page.HeaderSize:], []byte("hostile fuzz page")) {
		t.Fatalf("the checkpointed root page did not survive: %v", err)
	}
	for id, want := range held {
		got, err := back.Read(id)
		if err != nil {
			t.Fatalf("in-use page %d is no longer readable: %v", id, err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("in-use page %d changed although nothing wrote to it", id)
		}
	}
}

func FuzzOpenNeverPanicsOnArbitraryBytes(f *testing.F) {
	rich, bare, sh := hostileImages(f)
	head := int(page.Offset(sh.lists[0]))
	headPage := append(page.Page(nil), rich[head:head+page.Size]...)
	heapHeader := append(page.Page(nil), headPage...)
	heapHeader.SetKind(page.KindHeap)
	countHeader := append(page.Page(nil), headPage...)
	countHeader.SetItemCount(FreeListCapacity + 1)

	f.Add(hostileEncode(1, -1))
	f.Add(hostileEncode(2, -1))
	f.Add(hostileEncode(2, -1, hostilePatch{offNextPage, hostileU32(0)}))
	f.Add(hostileEncode(1, -1, hostilePatch{offNextPage, hostileU32(0)}))
	f.Add(hostileEncode(1, -1, hostilePatch{offFreeCount, hostileU32(sh.meta.FreeCount + 1)}))
	f.Add(hostileEncode(1, -1, hostilePatch{offFreeList, hostileU32(sh.meta.FilePages + ChunkPages)}))
	f.Add(hostileEncode(1, -1, hostilePatch{offRoot, hostileU32(uint32(sh.lists[0]))}))
	f.Add(hostileEncode(1, -1, hostilePatch{head, heapHeader[:page.HeaderSize]}))
	f.Add(hostileEncode(1, -1, hostilePatch{head, countHeader[:page.HeaderSize]}))
	f.Add(hostileEncode(1, -1, hostilePatch{head + FreeListHeader, hostileU32(uint32(sh.root))}))
	f.Add(hostileEncode(1, -1, hostilePatch{head + offFreeListNext, hostileU32(uint32(sh.lists[0]))}))
	f.Add(hostileEncode(1, -1, hostilePatch{offMagic + len(Magic) - 1, []byte{'!'}}))
	f.Add(hostileEncode(2, -1, hostilePatch{offVersion, hostileU32(0)}))
	f.Add(hostileEncode(2, -1, hostilePatch{offPageSize, hostileU32(2048)}))
	f.Add(hostileEncode(1, page.Size-1))
	f.Add(hostileEncode(1, int(sh.meta.NextPage)*page.Size))
	f.Add(hostileEncode(1, int(sh.meta.FilePages+3)*page.Size))
	f.Add(hostileEncode(2, 3*page.Size))
	f.Add(hostileEncode(1, -1, hostilePatch{0, make([]byte, 255)}))
	f.Add([]byte{0})
	f.Add(append([]byte{0}, Magic[:]...))
	f.Add(append([]byte{0}, make([]byte, page.Size)...))
	f.Add(append([]byte{0}, bytes.Repeat([]byte("this is not a database.\n"), 400)...))
	f.Add(append([]byte{0}, bare[:2*page.Size]...))

	f.Fuzz(func(t *testing.T, data []byte) {
		rich, bare, _ := hostileImages(t)
		img := hostileDecode(data, rich, bare)
		mf := hostileMem(img)
		s, created, err := openFile(mf)
		if err == nil && created != hostileInterruptedCreate(img) {
			t.Fatalf("openFile reported created=%v for a %d-byte image; only an all-zero image of at most one "+
				"chunk is an interrupted create", created, len(img))
		}
		if err != nil {
			if !hostileIsAny(err, ErrNotScuteDB, ErrBadVersion, ErrBadPageSize, ErrCorrupt) {
				t.Fatalf("Open rejected the file with %v, which is none of the documented classes", err)
			}
			if len(mf.log) != 0 || mf.syncs != 0 || !bytes.Equal(mf.live, img) || !bytes.Equal(mf.durable, img) {
				t.Fatalf("Open rejected the file (%v) but left %d unsynced writes and fsynced %d times: %s",
					err, len(mf.log), mf.syncs, hostileFirstDiff(img, mf.live))
			}
			return
		}
		hostileExercise(t, mf, s)
	})
}
