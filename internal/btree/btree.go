package btree

import (
	"bytes"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/scute-db/scutedb/internal/core"
)

const (
	MinOrder     = 3
	DefaultOrder = 64
)

var (
	ErrBadOrder  = errors.New("scutedb/btree: order must be at least 3")
	ErrInvariant = errors.New("scutedb/btree: invariant violated")
)

type node struct {
	leaf     bool
	keys     [][]byte
	rows     []core.RowID
	children []*node
}

type Tree struct {
	root    *node
	order   int
	maxKeys int
	minKeys int
	count   int
}

func New(order int) (*Tree, error) {
	if order < MinOrder {
		return nil, fmt.Errorf("%w: got %d", ErrBadOrder, order)
	}
	return &Tree{
		root:    &node{leaf: true},
		order:   order,
		maxKeys: order - 1,
		minKeys: (order - 1) / 2,
	}, nil
}

func (t *Tree) ensure() {
	if t.order == 0 {
		t.order = DefaultOrder
		t.maxKeys = DefaultOrder - 1
		t.minKeys = (DefaultOrder - 1) / 2
	}
	if t.root == nil {
		t.root = &node{leaf: true}
	}
}

func (t *Tree) Order() int {
	if t.order == 0 {
		return DefaultOrder
	}
	return t.order
}

func (t *Tree) Len() int { return t.count }

func (t *Tree) Height() int {
	if t.root == nil {
		return 0
	}
	h, n := 1, t.root
	for !n.leaf {
		n = n.children[0]
		h++
	}
	return h
}

func cloneKey(k []byte) []byte {
	out := make([]byte, len(k))
	copy(out, k)
	return out
}

func (n *node) childIndex(key []byte) int {
	return sort.Search(len(n.keys), func(i int) bool {
		return bytes.Compare(key, n.keys[i]) < 0
	})
}

func (n *node) leafIndex(key []byte) int {
	return sort.Search(len(n.keys), func(i int) bool {
		return bytes.Compare(n.keys[i], key) >= 0
	})
}

func (t *Tree) Get(key []byte) (core.RowID, bool) {
	if t.root == nil {
		return core.RowID{}, false
	}
	n := t.root
	for !n.leaf {
		n = n.children[n.childIndex(key)]
	}
	i := n.leafIndex(key)
	if i < len(n.keys) && bytes.Equal(n.keys[i], key) {
		return n.rows[i], true
	}
	return core.RowID{}, false
}

func (t *Tree) Put(key []byte, rid core.RowID) {
	t.ensure()
	sep, right, split := t.insert(t.root, key, rid)
	if !split {
		return
	}
	t.root = &node{
		leaf:     false,
		keys:     [][]byte{sep},
		children: []*node{t.root, right},
	}
}

func (t *Tree) insert(n *node, key []byte, rid core.RowID) ([]byte, *node, bool) {
	if n.leaf {
		i := n.leafIndex(key)
		if i < len(n.keys) && bytes.Equal(n.keys[i], key) {
			n.rows[i] = rid
			return nil, nil, false
		}
		n.keys = append(n.keys, nil)
		copy(n.keys[i+1:], n.keys[i:])
		n.keys[i] = cloneKey(key)

		n.rows = append(n.rows, core.RowID{})
		copy(n.rows[i+1:], n.rows[i:])
		n.rows[i] = rid

		t.count++
		if len(n.keys) <= t.maxKeys {
			return nil, nil, false
		}
		return t.splitLeaf(n)
	}

	i := n.childIndex(key)
	sep, right, split := t.insert(n.children[i], key, rid)
	if !split {
		return nil, nil, false
	}

	n.keys = append(n.keys, nil)
	copy(n.keys[i+1:], n.keys[i:])
	n.keys[i] = sep

	n.children = append(n.children, nil)
	copy(n.children[i+2:], n.children[i+1:])
	n.children[i+1] = right

	if len(n.keys) <= t.maxKeys {
		return nil, nil, false
	}
	return t.splitInternal(n)
}

func (t *Tree) splitLeaf(n *node) ([]byte, *node, bool) {
	mid := len(n.keys) / 2
	right := &node{leaf: true}
	right.keys = append(right.keys, n.keys[mid:]...)
	right.rows = append(right.rows, n.rows[mid:]...)
	n.keys = n.keys[:mid:mid]
	n.rows = n.rows[:mid:mid]
	return cloneKey(right.keys[0]), right, true
}

func (t *Tree) splitInternal(n *node) ([]byte, *node, bool) {
	mid := len(n.keys) / 2
	sep := n.keys[mid]
	right := &node{leaf: false}
	right.keys = append(right.keys, n.keys[mid+1:]...)
	right.children = append(right.children, n.children[mid+1:]...)
	n.keys = n.keys[:mid:mid]
	n.children = n.children[: mid+1 : mid+1]
	return sep, right, true
}

func (t *Tree) Validate() error {
	if t.root == nil {
		return nil
	}
	depth := -1
	return t.check(t.root, 0, &depth, nil, nil, true)
}

func (t *Tree) check(n *node, level int, leafDepth *int, low, high []byte, isRoot bool) error {
	for i := 1; i < len(n.keys); i++ {
		if bytes.Compare(n.keys[i-1], n.keys[i]) >= 0 {
			return fmt.Errorf("%w: keys out of order at level %d: %q then %q",
				ErrInvariant, level, n.keys[i-1], n.keys[i])
		}
	}
	for _, k := range n.keys {
		if low != nil && bytes.Compare(k, low) < 0 {
			return fmt.Errorf("%w: key %q below its subtree bound %q", ErrInvariant, k, low)
		}
		if high != nil && bytes.Compare(k, high) >= 0 {
			return fmt.Errorf("%w: key %q at or above its subtree bound %q", ErrInvariant, k, high)
		}
	}
	if len(n.keys) > t.maxKeys {
		return fmt.Errorf("%w: node at level %d holds %d keys, max %d",
			ErrInvariant, level, len(n.keys), t.maxKeys)
	}
	if !isRoot && len(n.keys) < t.minKeys {
		return fmt.Errorf("%w: node at level %d holds %d keys, min %d",
			ErrInvariant, level, len(n.keys), t.minKeys)
	}

	if n.leaf {
		if len(n.rows) != len(n.keys) {
			return fmt.Errorf("%w: leaf has %d keys but %d rows",
				ErrInvariant, len(n.keys), len(n.rows))
		}
		if *leafDepth == -1 {
			*leafDepth = level
		} else if *leafDepth != level {
			return fmt.Errorf("%w: leaves at differing depths %d and %d",
				ErrInvariant, *leafDepth, level)
		}
		return nil
	}

	if len(n.children) != len(n.keys)+1 {
		return fmt.Errorf("%w: internal node has %d keys but %d children",
			ErrInvariant, len(n.keys), len(n.children))
	}
	if isRoot && len(n.children) < 2 {
		return fmt.Errorf("%w: internal root has only %d child", ErrInvariant, len(n.children))
	}
	for i, c := range n.children {
		childLow, childHigh := low, high
		if i > 0 {
			childLow = n.keys[i-1]
		}
		if i < len(n.keys) {
			childHigh = n.keys[i]
		}
		if err := t.check(c, level+1, leafDepth, childLow, childHigh, false); err != nil {
			return err
		}
	}
	return nil
}

func (t *Tree) Dump() string {
	var sb strings.Builder
	if t.root == nil {
		return ""
	}
	t.dump(&sb, t.root, 0)
	return sb.String()
}

func (t *Tree) dump(sb *strings.Builder, n *node, level int) {
	indent := strings.Repeat("    ", level)
	kind := "internal"
	if n.leaf {
		kind = "leaf"
	}
	parts := make([]string, len(n.keys))
	for i, k := range n.keys {
		parts[i] = string(k)
	}
	fmt.Fprintf(sb, "%s%-8s [%s]\n", indent, kind, strings.Join(parts, " "))
	for _, c := range n.children {
		t.dump(sb, c, level+1)
	}
}
