package main

import (
	"fmt"
	"os"
	"path/filepath"
	"unsafe"

	"github.com/scute-db/scutedb/internal/codec"
	"github.com/scute-db/scutedb/internal/core"
	"github.com/scute-db/scutedb/internal/nodepage"
	"github.com/scute-db/scutedb/internal/page"
)

func expNodePage() {
	fmt.Print("STEP 0x09  Nodes as pages\n\n")

	section1()
	section2()
	section3()
	section4()
	section5()
}

func nodeKey(v int64) []byte { return codec.AppendKeyInt64(nil, v) }

func section1() {
	fmt.Print("1. WHY A POINTER CANNOT GO ON DISK\n\n")

	type inMemoryNode struct {
		children []*int
		next     *int
	}
	var n inMemoryNode

	fmt.Printf("   an in-memory child link is a Go pointer: %d bytes\n", unsafe.Sizeof(n.next))
	fmt.Print("   it holds a RAM address, valid only while this process lives.\n")
	fmt.Print("   write it to disk, restart, and it points at someone else's memory.\n\n")
	fmt.Printf("   an on-disk child link is a core.PageID: %d bytes\n", unsafe.Sizeof(core.PageID(0)))
	fmt.Print("   it holds a page number. page 12 is at byte 12 * 4096 = 49152,\n")
	fmt.Print("   in this process, in the next one, and on another machine.\n\n")
	fmt.Print("   that swap is the whole step. everything else follows from it.\n\n")
}

func section2() {
	fmt.Print("2. FANOUT IS COMPUTED, NOT CHOSEN\n\n")
	fmt.Print(indent(nodepage.FanoutTable(8), "   "))

	old := page.Usable / (8 + 4)
	real := nodepage.MaxKeys(page.KindBTreeInternal, 8)
	fmt.Printf("   an earlier estimate in this repo said %d keys per internal node:\n", old)
	fmt.Printf("   %d usable bytes / (8 key + 4 child) = %d.\n", page.Usable, old)
	fmt.Printf("   the real answer is %d, because that estimate forgot the %d-byte\n", real, nodepage.NodeHeaderSize)
	fmt.Printf("   node header and the %d-byte slot per entry. overhead is %d%% of fanout.\n\n",
		nodepage.SlotSize, (old-real)*100/old)
}

func section3() {
	fmt.Print("3. A REAL LEAF PAGE, BYTE FOR BYTE\n\n")

	leaf := nodepage.NewLeaf(1)
	leaf.SetNext(2)
	for _, v := range []int64{-1, 10, 20} {
		if err := leaf.AppendLeaf(nodeKey(v), core.RowID{Page: core.PageID(v + 2), Slot: uint16(v + 1)}); err != nil {
			fail(err)
		}
	}
	if err := leaf.Validate(); err != nil {
		fail(err)
	}
	fmt.Print(indent(leaf.Describe(), "   "))

	fmt.Print("   read the first cell yourself:\n")
	lo := int(leaf.FreeEnd()) + 2*(8+nodepage.LeafSuffix)
	fmt.Printf("   slot 0 says the cell is at %d. bytes %d..%d are\n", lo, lo, lo+13)
	fmt.Printf("     % 02X\n", []byte(leaf.Page[lo:lo+14]))
	fmt.Print("     first 8 bytes  = the key, big-endian with the sign bit flipped\n")
	fmt.Print("     7F FF FF FF FF FF FF FF  is -1 from step 0x03\n")
	fmt.Print("     next 4 bytes   = row id page number\n")
	fmt.Print("     last 2 bytes   = row id slot number\n\n")
}

func section4() {
	fmt.Print("4. AN INTERNAL PAGE HOLDS N KEYS AND N+1 CHILDREN\n\n")

	root, err := nodepage.NewInternal(0, 1)
	if err != nil {
		fail(err)
	}
	root.SetFirstChild(1)
	if err := root.AppendInternal(nodeKey(20), 2); err != nil {
		fail(err)
	}
	if err := root.Validate(); err != nil {
		fail(err)
	}
	fmt.Print(indent(root.Describe(), "   "))
	fmt.Print("   one key, two children. the extra child has nowhere to sit in the\n")
	fmt.Print("   slot array, so it lives in the node header instead.\n\n")
}

func section5() {
	fmt.Print("5. WALKING A TREE THAT IS ONLY BYTES ON DISK\n\n")

	dir, err := os.MkdirTemp("", "scutedb-nodepage")
	if err != nil {
		fail(err)
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "tree.db")

	pf, err := page.Open(path)
	if err != nil {
		fail(err)
	}

	root, err := nodepage.NewInternal(0, 1)
	if err != nil {
		fail(err)
	}
	root.SetFirstChild(1)
	if err := root.AppendInternal(nodeKey(20), 2); err != nil {
		fail(err)
	}

	left := nodepage.NewLeaf(1)
	left.SetNext(2)
	for _, v := range []int64{-1, 10} {
		if err := left.AppendLeaf(nodeKey(v), core.RowID{Page: 100, Slot: uint16(v + 1)}); err != nil {
			fail(err)
		}
	}

	right := nodepage.NewLeaf(2)
	for _, v := range []int64{20, 30} {
		if err := right.AppendLeaf(nodeKey(v), core.RowID{Page: 200, Slot: uint16(v)}); err != nil {
			fail(err)
		}
	}

	for _, n := range []nodepage.Node{root, left, right} {
		if err := n.Validate(); err != nil {
			fail(err)
		}
		if err := pf.Write(n.Page); err != nil {
			fail(err)
		}
	}
	if err := pf.Sync(); err != nil {
		fail(err)
	}
	if err := pf.Close(); err != nil {
		fail(err)
	}

	info, err := os.Stat(path)
	if err != nil {
		fail(err)
	}
	fmt.Printf("   wrote %s: %d bytes = %d pages of %d\n\n", filepath.Base(path), info.Size(),
		info.Size()/page.Size, page.Size)

	reopened, err := page.Open(path)
	if err != nil {
		fail(err)
	}
	defer reopened.Close()

	for _, want := range []int64{10, 30} {
		fmt.Printf("   looking up %d, starting at page 0\n", want)
		id := core.PageID(0)
		for {
			raw, err := reopened.Read(id)
			if err != nil {
				fail(err)
			}
			n, err := nodepage.Load(raw)
			if err != nil {
				fail(err)
			}
			fmt.Printf("     read page %d at byte offset %d, kind %s, %d keys\n",
				id, page.Offset(id), n.Kind(), n.KeyCount())
			if n.Leaf() {
				at, found := n.Search(nodeKey(want))
				if !found {
					fmt.Printf("     %d is not in this leaf\n\n", want)
					break
				}
				r := n.Row(at)
				fmt.Printf("     found at slot %d -> row id page %d slot %d\n\n", at, r.Page, r.Slot)
				break
			}
			id = n.ChildFor(nodeKey(want))
			fmt.Printf("     not a leaf, so follow a page id: next is page %d\n", id)
		}
	}

	fmt.Print("   nothing above was a pointer. every hop was a page number\n")
	fmt.Print("   turned into a byte offset by multiplying by 4096.\n\n")
	fmt.Print("   this is how the real ones do it too:\n")
	fmt.Print("     postgres  BlockNumber, a uint32 page index into the relation file\n")
	fmt.Print("     sqlite    32-bit page numbers, 1-based, page 1 is the schema root\n")
	fmt.Print("     innodb    32-bit page numbers inside a tablespace, 16 KB pages\n\n")
}

func indent(s string, with string) string {
	out := with
	for i := 0; i < len(s); i++ {
		out += string(s[i])
		if s[i] == '\n' && i != len(s)-1 {
			out += with
		}
	}
	return out + "\n"
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "demo failed:", err)
	os.Exit(1)
}
