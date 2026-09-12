package btree

import (
	"bytes"

	"github.com/scute-db/scutedb/internal/core"
)

type Stats struct {
	BorrowsLeft  int
	BorrowsRight int
	Merges       int
	Collapses    int
}

func (t *Tree) Stats() Stats { return t.stats }

func (t *Tree) ResetStats() { t.stats = Stats{} }

func (t *Tree) Delete(key []byte) bool {
	if t.root == nil {
		return false
	}
	if !t.delete(t.root, key) {
		return false
	}
	t.count--
	for !t.root.leaf && len(t.root.keys) == 0 {
		t.root = t.root.children[0]
		t.stats.Collapses++
	}
	return true
}

func (t *Tree) delete(n *node, key []byte) bool {
	if n.leaf {
		i := n.leafIndex(key)
		if i >= len(n.keys) || !bytes.Equal(n.keys[i], key) {
			return false
		}
		n.keys = append(n.keys[:i], n.keys[i+1:]...)
		n.rows = append(n.rows[:i], n.rows[i+1:]...)
		return true
	}
	i := n.childIndex(key)
	if !t.delete(n.children[i], key) {
		return false
	}
	if len(n.children[i].keys) < t.minKeys {
		t.repair(n, i)
	}
	return true
}

func (t *Tree) repair(p *node, i int) {
	if i > 0 && len(p.children[i-1].keys) > t.minKeys {
		t.borrowFromLeft(p, i)
		t.stats.BorrowsLeft++
		return
	}
	if i < len(p.children)-1 && len(p.children[i+1].keys) > t.minKeys {
		t.borrowFromRight(p, i)
		t.stats.BorrowsRight++
		return
	}
	if i > 0 {
		t.merge(p, i-1)
	} else {
		t.merge(p, i)
	}
	t.stats.Merges++
}

func (t *Tree) borrowFromLeft(p *node, i int) {
	left, child := p.children[i-1], p.children[i]
	last := len(left.keys) - 1

	if child.leaf {
		child.keys = append([][]byte{left.keys[last]}, child.keys...)
		child.rows = append([]core.RowID{left.rows[last]}, child.rows...)
		left.keys = left.keys[:last:last]
		left.rows = left.rows[:last:last]
		p.keys[i-1] = cloneKey(child.keys[0])
		return
	}

	lastChild := len(left.children) - 1
	child.keys = append([][]byte{p.keys[i-1]}, child.keys...)
	child.children = append([]*node{left.children[lastChild]}, child.children...)
	p.keys[i-1] = left.keys[last]
	left.keys = left.keys[:last:last]
	left.children = left.children[:lastChild:lastChild]
}

func (t *Tree) borrowFromRight(p *node, i int) {
	child, right := p.children[i], p.children[i+1]

	if child.leaf {
		child.keys = append(child.keys, right.keys[0])
		child.rows = append(child.rows, right.rows[0])
		right.keys = right.keys[1:]
		right.rows = right.rows[1:]
		p.keys[i] = cloneKey(right.keys[0])
		return
	}

	child.keys = append(child.keys, p.keys[i])
	child.children = append(child.children, right.children[0])
	p.keys[i] = right.keys[0]
	right.keys = right.keys[1:]
	right.children = right.children[1:]
}

func (t *Tree) merge(p *node, i int) {
	left, right := p.children[i], p.children[i+1]

	if left.leaf {
		left.keys = append(left.keys, right.keys...)
		left.rows = append(left.rows, right.rows...)
		left.next = right.next
	} else {
		left.keys = append(left.keys, p.keys[i])
		left.keys = append(left.keys, right.keys...)
		left.children = append(left.children, right.children...)
	}

	p.keys = append(p.keys[:i], p.keys[i+1:]...)
	p.children = append(p.children[:i+1], p.children[i+2:]...)
}

func (t *Tree) LeafFill() (leaves, keys, capacity int) {
	if t.root == nil {
		return 0, 0, 0
	}
	for n := t.firstLeaf(); n != nil; n = n.next {
		leaves++
		keys += len(n.keys)
		capacity += t.maxKeys
	}
	return leaves, keys, capacity
}

func (t *Tree) Separators() [][]byte {
	var out [][]byte
	if t.root == nil {
		return out
	}
	collectSeparators(t.root, &out)
	return out
}

func collectSeparators(n *node, out *[][]byte) {
	if n.leaf {
		return
	}
	for _, k := range n.keys {
		*out = append(*out, cloneKey(k))
	}
	for _, c := range n.children {
		collectSeparators(c, out)
	}
}
