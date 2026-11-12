package disktree

import (
	"bytes"
	"testing"

	"github.com/scute-db/scutedb/internal/bufpool"
	"github.com/scute-db/scutedb/internal/core"
	"github.com/scute-db/scutedb/internal/page"
)

func FuzzTreeMatchesAMap(f *testing.F) {
	f.Add([]byte{3, 0, 4, 'a', 'b', 'c', 'd', 3, 4, 'a', 'b', 'c', 'd', 2, 4, 'a', 'b', 'c', 'd', 5, 1, 'a'})
	f.Add([]byte{0x80, 0, 200, 1, 0, 201, 2, 0, 202, 3, 0, 203, 4, 0, 204, 5, 0, 205, 6})
	f.Add(bytes.Repeat([]byte{0x81, 0, 8, 1, 2, 3, 4, 5, 6, 7, 8}, 40))
	f.Fuzz(func(t *testing.T, script []byte) {
		if len(script) == 0 || len(script) > 6000 {
			return
		}
		head := script[0]
		pool := newPool(t, bufpool.NewMemSource(), 3+int(head%6), bufpool.Policy(head/6%3), bufpool.Mode(head/18%2))
		tr, _, err := Create(pool)
		if err != nil {
			t.Fatal(err)
		}
		d := &treeDriver{t: t, tr: tr, model: map[string]core.RowID{}}
		for pos := 1; pos+2 < len(script); {
			op, flags, size := int(script[pos]), script[pos+1], int(script[pos+2])%16+1
			pos += 3
			if pos+size > len(script) {
				size = len(script) - pos
			}
			key := append([]byte(nil), script[pos:pos+size]...)
			pos += size
			if len(key) == 0 {
				break
			}
			if flags&0x80 != 0 {
				key = bytes.Repeat(key, 1000/len(key)+1)[:900+int(flags)%101]
			}
			if flags&0x40 != 0 && len(d.keys) > 0 {
				key = []byte(d.keys[int(flags)%len(d.keys)])
			}
			bound := append(append([]byte(nil), key...), flags)
			d.do(op, key, bound, core.RowID{Page: core.PageID(pos), Slot: uint16(op)})
		}
		mustValidate(t, tr)
		checkModel(t, tr, d.model)
	})
}

func seedTree(t testing.TB) (*bufpool.MemSource, core.PageID, []core.PageID) {
	src := bufpool.NewMemSource()
	tr, root, err := Create(newPool(t, src, 16, bufpool.LRU, bufpool.WriteBack))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 1500; i++ {
		mustPut(t, tr, wideKey(i), ridOf(i))
	}
	if err := tr.Pool().FlushAll(); err != nil {
		t.Fatal(err)
	}
	var ids []core.PageID
	for id := core.PageID(1); ; id++ {
		if _, err := src.Read(id); err != nil {
			break
		}
		ids = append(ids, id)
	}
	return src, root, ids
}

func FuzzCorruptPagesNeverPanic(f *testing.F) {
	clean, _, _ := seedTree(f)
	leaf, _ := clean.Read(2)
	f.Add(uint8(0), []byte(leaf[16:200]), uint16(0))
	f.Add(uint8(1), []byte{0xFF, 0xFF, 0, 0, 0, 0, 0, 1}, uint16(6))
	f.Add(uint8(2), []byte{0, 0, 0, 0}, uint16(16))
	f.Add(uint8(5), []byte{0x0F, 0xF0, 0x00, 0x20}, uint16(24))
	f.Add(uint8(0), []byte{0, 0, 0, 1}, uint16(16))
	f.Fuzz(func(t *testing.T, which uint8, patch []byte, at uint16) {
		src, root, ids := seedTree(t)
		id := ids[int(which)%len(ids)]
		p, err := src.Read(id)
		if err != nil {
			t.Fatal(err)
		}
		copy(p[int(at)%page.Size:], patch)
		p.SetID(id)
		if err := src.Write(p); err != nil {
			t.Fatal(err)
		}
		tr, err := Open(newPool(t, src, 4, bufpool.LRU, bufpool.WriteBack), root)
		if err != nil {
			return
		}
		_ = tr.Validate()
		noPins(t, tr, "Validate")
		for i := 0; i < 1500; i += 97 {
			_, _, _ = tr.Get(wideKey(i))
			_ = tr.Put(wideKey(i*3+1), ridOf(i))
			_, _ = tr.Delete(wideKey(i + 13))
			noPins(t, tr, "an operation on a damaged tree")
		}
		drain(t, tr.Scan(wideKey(100), nil), 1_000_000)
		noPins(t, tr, "a scan of a damaged tree")
	})
}
