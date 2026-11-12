package disktree

import (
	"errors"
	"fmt"

	"github.com/scute-db/scutedb/internal/bufpool"
	"github.com/scute-db/scutedb/internal/core"
	"github.com/scute-db/scutedb/internal/nodepage"
	"github.com/scute-db/scutedb/internal/page"
)

var (
	ErrCorrupt = errors.New("scutedb/disktree: tree is corrupt")
	ErrNoPool  = errors.New("scutedb/disktree: no buffer pool")
)

type Tree struct {
	pool    *bufpool.Pool
	root    core.PageID
	pending []write
}

func Create(pool *bufpool.Pool) (*Tree, core.PageID, error) {
	if pool == nil {
		return nil, 0, ErrNoPool
	}
	f, err := pool.NewPage(page.KindBTreeLeaf)
	if err != nil {
		return nil, 0, fmt.Errorf("scutedb/disktree: create: %w", err)
	}
	id := f.ID()
	copy(f.Page(), nodepage.NewLeaf(id).Page)
	f.MarkVerified()
	if err := pool.Unpin(f, true); err != nil {
		return nil, 0, fmt.Errorf("scutedb/disktree: create: %w", err)
	}
	return &Tree{pool: pool, root: id}, id, nil
}

func Open(pool *bufpool.Pool, root core.PageID) (*Tree, error) {
	if pool == nil {
		return nil, ErrNoPool
	}
	t := &Tree{pool: pool, root: root}
	f, _, err := t.fetch(root)
	if err != nil {
		return nil, fmt.Errorf("scutedb/disktree: open root %d: %w", root, err)
	}
	if err := t.unpin(f, false, nil); err != nil {
		return nil, err
	}
	return t, nil
}

func (t *Tree) Root() core.PageID   { return t.root }
func (t *Tree) Pool() *bufpool.Pool { return t.pool }

func (t *Tree) unpin(f *bufpool.Frame, dirty bool, err error) error {
	if uerr := t.pool.Unpin(f, dirty); uerr != nil {
		if err == nil {
			return uerr
		}
		return errors.Join(err, uerr)
	}
	return err
}

func (t *Tree) fetch(id core.PageID) (*bufpool.Frame, nodepage.Node, error) {
	f, err := t.pool.Fetch(id)
	if err != nil {
		return nil, nodepage.Node{}, err
	}
	n, err := nodepage.Load(f.Page())
	if err == nil && !f.Verified() {
		if err = n.Validate(); err == nil {
			f.MarkVerified()
		}
	}
	if err != nil {
		return nil, nodepage.Node{}, t.unpin(f, false, fmt.Errorf("%w: page %d: %w", ErrCorrupt, id, err))
	}
	return f, n, nil
}

type step struct {
	id    core.PageID
	index int
}

func (t *Tree) descend(key []byte, path *[]step) (*bufpool.Frame, nodepage.Node, error) {
	f, n, err := t.fetch(t.root)
	if err != nil {
		return nil, nodepage.Node{}, err
	}
	for !n.Leaf() {
		i := n.ChildIndex(key)
		child, level := n.Child(i), n.Level()
		if path != nil {
			*path = append(*path, step{id: f.ID(), index: i})
		}
		if err := t.unpin(f, false, nil); err != nil {
			return nil, nodepage.Node{}, err
		}
		if f, n, err = t.fetch(child); err != nil {
			return nil, nodepage.Node{}, err
		}
		if n.Level() != level-1 {
			return nil, nodepage.Node{}, t.unpin(f, false, fmt.Errorf("%w: page %d is at level %d under a level-%d node",
				ErrCorrupt, child, n.Level(), level))
		}
	}
	return f, n, nil
}

func (t *Tree) Get(key []byte) (core.RowID, bool, error) {
	f, n, err := t.descend(key, nil)
	if err != nil {
		return core.RowID{}, false, err
	}
	var rid core.RowID
	i, found := n.Search(key)
	if found {
		rid = n.Row(i)
	}
	return rid, found, t.unpin(f, false, nil)
}

func (t *Tree) Put(key []byte, rid core.RowID) error {
	if err := nodepage.CheckKey(key); err != nil {
		return fmt.Errorf("scutedb/disktree: put: %w", err)
	}
	if err := t.resume(); err != nil {
		return err
	}
	var path []step
	f, n, err := t.descend(key, &path)
	if err != nil {
		return err
	}
	i, found := n.Search(key)
	if found {
		err := n.SetRow(i, rid)
		return t.unpin(f, err == nil, err)
	}
	if n.Fits(len(key)) {
		err := n.InsertLeaf(i, key, rid)
		return t.unpin(f, err == nil, err)
	}
	full := readImage(n)
	full.insert(i, entry{key: append([]byte(nil), key...), rid: rid})
	if err := t.unpin(f, false, nil); err != nil {
		return err
	}
	return t.split(path, full)
}

func (t *Tree) Delete(key []byte) (bool, error) {
	if err := t.resume(); err != nil {
		return false, err
	}
	f, n, err := t.descend(key, nil)
	if err != nil {
		return false, err
	}
	i, found := n.Search(key)
	if !found {
		return false, t.unpin(f, false, nil)
	}
	err = n.Remove(i)
	return err == nil, t.unpin(f, err == nil, err)
}

func (t *Tree) Height() (int, error) {
	f, n, err := t.fetch(t.root)
	if err != nil {
		return 0, err
	}
	h := int(n.Level()) + 1
	return h, t.unpin(f, false, nil)
}
