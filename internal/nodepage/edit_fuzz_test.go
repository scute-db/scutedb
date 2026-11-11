package nodepage

import (
	"errors"
	"testing"

	"github.com/scute-db/scutedb/internal/core"
	"github.com/scute-db/scutedb/internal/page"
)

func FuzzEditsKeepTheNodeValid(f *testing.F) {
	f.Add([]byte{0, 3, 'a', 'b', 'c', 0, 1, 'a', 1, 0, 2, 0, 9}, true)
	f.Add([]byte{0, 200, 1, 2, 3, 4, 5, 0, 5, 9, 9, 9, 9, 9, 1, 1, 3, 1, 7}, false)
	f.Add([]byte{0, 255, 0xFF, 0, 1, 0x00, 1, 0}, true)
	f.Add([]byte{0x80, 19, 'k', 0x80, 3, 'a', 0x80, 7, 'b', 0x80, 9, 'c', 0x80, 11, 'd', 1, 2}, true)

	f.Fuzz(func(t *testing.T, script []byte, leaf bool) {
		if len(script) > 4096 {
			script = script[:4096]
		}
		var n Node
		if leaf {
			n = NewLeaf(1)
		} else {
			var err error
			n, err = NewInternal(1, 1)
			if err != nil {
				t.Fatal(err)
			}
		}
		first := core.PageID(0)
		var model []modelEntry
		for pos := 0; pos+1 < len(script); {
			op, long, arg := script[pos]%4, script[pos]&0x80 != 0, int(script[pos+1])
			pos += 2
			switch op {
			case 0:
				size := arg%64 + 1
				if pos+size > len(script) {
					size = len(script) - pos
				}
				if size == 0 {
					return
				}
				k := append([]byte(nil), script[pos:pos+size]...)
				pos += size
				for long && len(k) < 990+arg%20 {
					k = append(k, byte(len(k)))
				}
				i, found := modelSearch(model, k)
				e := modelEntry{key: k, rid: core.RowID{Page: core.PageID(pos), Slot: uint16(arg)}, child: core.PageID(pos)}
				var err error
				if leaf {
					err = n.InsertLeaf(i, k, e.rid)
				} else {
					err = n.InsertInternal(i, k, e.child)
				}
				switch {
				case found:
					if !errors.Is(err, ErrOrder) {
						t.Fatalf("inserting a duplicate gave %v, want ErrOrder", err)
					}
				case len(k) > MaxKeySize:
					if !errors.Is(err, ErrKeyTooLarge) {
						t.Fatalf("inserting %d bytes gave %v, want ErrKeyTooLarge", len(k), err)
					}
				case errors.Is(err, ErrNoFit):
					if n.Fits(len(k)) {
						t.Fatal("ErrNoFit while Fits says yes")
					}
				case err != nil:
					t.Fatal(err)
				default:
					model = append(model, modelEntry{})
					copy(model[i+1:], model[i:])
					model[i] = e
				}
			case 1:
				err := n.Remove(arg % (len(model) + 1))
				if arg%(len(model)+1) == len(model) {
					if !errors.Is(err, ErrBadIndex) {
						t.Fatalf("Remove past the end gave %v", err)
					}
				} else if err != nil {
					t.Fatal(err)
				} else {
					i := arg % (len(model) + 1)
					model = append(model[:i], model[i+1:]...)
				}
			case 2:
				if len(model) == 0 {
					continue
				}
				i := arg % len(model)
				if leaf {
					rid := core.RowID{Page: core.PageID(arg * 7), Slot: uint16(pos)}
					if err := n.SetRow(i, rid); err != nil {
						t.Fatal(err)
					}
					model[i].rid = rid
				} else {
					c := arg % (len(model) + 1)
					if err := n.SetChild(c, core.PageID(pos)); err != nil {
						t.Fatal(err)
					}
					if c == 0 {
						first = core.PageID(pos)
					} else {
						model[c-1].child = core.PageID(pos)
					}
				}
			case 3:
				raw := append(page.Page(nil), n.Page...)
				back, err := Load(raw)
				if err != nil {
					t.Fatal(err)
				}
				if err := back.Validate(); err != nil {
					t.Fatal(err)
				}
			}
			checkAgainstModel(t, n, first, model)
		}
	})
}
