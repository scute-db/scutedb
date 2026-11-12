package disktree

import (
	"errors"
	"fmt"
	"sync"

	"github.com/scute-db/scutedb/internal/bufpool"
	"github.com/scute-db/scutedb/internal/core"
	"github.com/scute-db/scutedb/internal/nodepage"
	"github.com/scute-db/scutedb/internal/page"
)

var (
	ErrCorrupt   = errors.New("scutedb/disktree: tree is corrupt")
	ErrNoPool    = errors.New("scutedb/disktree: no buffer pool")
	ErrNoMTR     = errors.New("scutedb/disktree: no mini-transaction")
	ErrOtherPool = errors.New("scutedb/disktree: the mini-transaction belongs to another buffer pool")
)

type Tree struct {
	mu    sync.RWMutex
	pool  *bufpool.Pool
	root  core.PageID
	pause func(stage string)
}

func MinFrames(height int) int {
	if height < 1 {
		height = 1
	}
	return 2*height + 1
}

func Create(pool *bufpool.Pool) (*Tree, core.PageID, error) {
	if pool == nil {
		return nil, 0, ErrNoPool
	}
	m := pool.Begin()
	t, id, err := CreateMTR(m)
	if err != nil {
		return nil, 0, fmt.Errorf("scutedb/disktree: create: %w", abort(m, err))
	}
	if err := m.Commit(); err != nil {
		return nil, 0, fmt.Errorf("scutedb/disktree: create: %w", err)
	}
	return t, id, nil
}

func CreateMTR(m *bufpool.MTR) (*Tree, core.PageID, error) {
	if m == nil {
		return nil, 0, ErrNoMTR
	}
	f, err := m.NewPage(page.KindBTreeLeaf)
	if err != nil {
		return nil, 0, err
	}
	if err := m.WLatch(f); err != nil {
		return nil, 0, err
	}
	id := f.ID()
	copy(f.Page(), nodepage.NewLeaf(id).Page)
	f.MarkVerified()
	return &Tree{pool: m.Pool(), root: id}, id, nil
}

func Open(pool *bufpool.Pool, root core.PageID) (*Tree, error) {
	if pool == nil {
		return nil, ErrNoPool
	}
	t := &Tree{pool: pool, root: root}
	v, err := t.read(nil, root)
	if err != nil {
		return nil, fmt.Errorf("scutedb/disktree: open root %d: %w", root, err)
	}
	if err := t.release(v, nil); err != nil {
		return nil, err
	}
	return t, nil
}

func (t *Tree) Root() core.PageID   { return t.root }
func (t *Tree) Pool() *bufpool.Pool { return t.pool }

func abort(m *bufpool.MTR, err error) error {
	if aerr := m.Abort(); aerr != nil {
		return errors.Join(err, aerr)
	}
	return err
}

func (t *Tree) at(stage string) {
	if t.pause != nil {
		t.pause(stage)
	}
}

func (t *Tree) owns(m *bufpool.MTR) error {
	if m == nil {
		return ErrNoMTR
	}
	if m.Pool() != t.pool {
		return ErrOtherPool
	}
	return nil
}

func (t *Tree) unpin(f *bufpool.Frame, dirty bool, err error) error {
	if uerr := t.pool.Unpin(f, dirty); uerr != nil {
		if err == nil {
			return uerr
		}
		return errors.Join(err, uerr)
	}
	return err
}

type view struct {
	f   *bufpool.Frame
	n   nodepage.Node
	own bool
}

func (t *Tree) read(m *bufpool.MTR, id core.PageID) (view, error) {
	if m != nil && m.Holds(id) {
		f, err := m.Fetch(id)
		if err != nil {
			return view{}, err
		}
		n, err := check(f, id)
		return view{f: f, n: n}, err
	}
	f, err := t.pool.Fetch(id)
	if err != nil {
		return view{}, err
	}
	t.pool.RLatch(f)
	v := view{f: f, own: true}
	if v.n, err = check(f, id); err != nil {
		return view{}, t.release(v, err)
	}
	return v, nil
}

func check(f *bufpool.Frame, id core.PageID) (nodepage.Node, error) {
	n, err := nodepage.Load(f.Page())
	if err == nil && !f.Verified() {
		if err = n.Validate(); err == nil {
			f.MarkVerified()
		}
	}
	if err != nil {
		return nodepage.Node{}, fmt.Errorf("%w: page %d: %w", ErrCorrupt, id, err)
	}
	return n, nil
}

func (t *Tree) release(v view, err error) error {
	if !v.own {
		return err
	}
	t.pool.RUnlatch(v.f)
	return t.unpin(v.f, false, err)
}

type step struct {
	id    core.PageID
	index int
}

func (t *Tree) descend(m *bufpool.MTR, key []byte, path *[]step) (core.PageID, view, error) {
	id := t.root
	v, err := t.read(m, id)
	if err != nil {
		return 0, view{}, err
	}
	for !v.n.Leaf() {
		i := v.n.ChildIndex(key)
		child, level := v.n.Child(i), v.n.Level()
		if path != nil {
			*path = append(*path, step{id: id, index: i})
		}
		if err := t.release(v, nil); err != nil {
			return 0, view{}, err
		}
		id = child
		if v, err = t.read(m, id); err != nil {
			return 0, view{}, err
		}
		if v.n.Level() != level-1 {
			return 0, view{}, t.release(v, fmt.Errorf("%w: page %d is at level %d under a level-%d node",
				ErrCorrupt, id, v.n.Level(), level))
		}
	}
	return id, v, nil
}

func (t *Tree) Get(key []byte) (core.RowID, bool, error) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	_, v, err := t.descend(nil, key, nil)
	if err != nil {
		return core.RowID{}, false, err
	}
	var rid core.RowID
	i, found := v.n.Search(key)
	if found {
		rid = v.n.Row(i)
	}
	return rid, found, t.release(v, nil)
}

func (t *Tree) Put(key []byte, rid core.RowID) error {
	if err := nodepage.CheckKey(key); err != nil {
		return fmt.Errorf("scutedb/disktree: put: %w", err)
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	m := t.pool.Begin()
	if err := t.put(m, key, rid); err != nil {
		return abort(m, err)
	}
	return m.Commit()
}

func (t *Tree) PutMTR(m *bufpool.MTR, key []byte, rid core.RowID) error {
	if err := nodepage.CheckKey(key); err != nil {
		return fmt.Errorf("scutedb/disktree: put: %w", err)
	}
	if err := t.owns(m); err != nil {
		return err
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.put(m, key, rid)
}

func (t *Tree) leaf(m *bufpool.MTR, key []byte, path *[]step) (*bufpool.Frame, nodepage.Node, bool, error) {
	id, v, err := t.descend(m, key, path)
	if err != nil {
		return nil, nodepage.Node{}, false, err
	}
	if err := t.release(v, nil); err != nil {
		return nil, nodepage.Node{}, false, err
	}
	t.at("leaf")
	held := m.Holds(id)
	f, err := m.Fetch(id)
	if err != nil {
		return nil, nodepage.Node{}, false, err
	}
	if err := m.WLatch(f); err != nil {
		return nil, nodepage.Node{}, false, err
	}
	n, err := check(f, id)
	if err == nil && !n.Leaf() {
		err = fmt.Errorf("%w: page %d stopped being a leaf", ErrCorrupt, id)
	}
	return f, n, held, err
}

func (t *Tree) put(m *bufpool.MTR, key []byte, rid core.RowID) error {
	var path []step
	f, n, held, err := t.leaf(m, key, &path)
	if err != nil {
		return err
	}
	i, found := n.Search(key)
	if found {
		return n.SetRow(i, rid)
	}
	if n.Fits(len(key)) {
		return n.InsertLeaf(i, key, rid)
	}
	full := readImage(n)
	full.insert(i, entry{key: append([]byte(nil), key...), rid: rid})
	if !held {
		if err := m.Release(f); err != nil {
			return err
		}
	}
	return t.split(m, path, full)
}

func (t *Tree) Delete(key []byte) (bool, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	m := t.pool.Begin()
	ok, err := t.delete(m, key)
	if err != nil {
		return false, abort(m, err)
	}
	return ok, m.Commit()
}

func (t *Tree) DeleteMTR(m *bufpool.MTR, key []byte) (bool, error) {
	if err := t.owns(m); err != nil {
		return false, err
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.delete(m, key)
}

func (t *Tree) delete(m *bufpool.MTR, key []byte) (bool, error) {
	f, n, held, err := t.leaf(m, key, nil)
	if err != nil {
		return false, err
	}
	i, found := n.Search(key)
	if !found {
		if held {
			return false, nil
		}
		return false, m.Release(f)
	}
	if err := n.Remove(i); err != nil {
		return false, err
	}
	return true, nil
}

func (t *Tree) Height() (int, error) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	v, err := t.read(nil, t.root)
	if err != nil {
		return 0, err
	}
	h := int(v.n.Level()) + 1
	return h, t.release(v, nil)
}
