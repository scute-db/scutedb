package nodepage

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/scute-db/scutedb/internal/core"
	"github.com/scute-db/scutedb/internal/page"
)

func key(i int) []byte {
	b := make([]byte, 8)
	binary.BigEndian.PutUint64(b, uint64(i))
	return b
}

func rid(i int) core.RowID {
	return core.RowID{Page: core.PageID(i / 10), Slot: uint16(i % 10)}
}

func TestLeafRoundTrip(t *testing.T) {
	n := NewLeaf(7)
	n.SetNext(9)
	for i := 0; i < 50; i++ {
		if err := n.AppendLeaf(key(i), rid(i)); err != nil {
			t.Fatalf("AppendLeaf(%d): %v", i, err)
		}
	}
	if err := n.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if n.ID() != 7 || n.Next() != 9 || n.Level() != 0 || !n.Leaf() {
		t.Fatalf("header round trip failed: id %d next %d level %d leaf %v",
			n.ID(), n.Next(), n.Level(), n.Leaf())
	}
	if n.KeyCount() != 50 {
		t.Fatalf("key count %d, want 50", n.KeyCount())
	}
	for i := 0; i < 50; i++ {
		if !bytes.Equal(n.Key(i), key(i)) {
			t.Fatalf("key %d is % 02X, want % 02X", i, n.Key(i), key(i))
		}
		if n.Row(i) != rid(i) {
			t.Fatalf("row %d is %+v, want %+v", i, n.Row(i), rid(i))
		}
	}
}

func TestInternalRoundTrip(t *testing.T) {
	n, err := NewInternal(4, 2)
	if err != nil {
		t.Fatalf("NewInternal: %v", err)
	}
	n.SetFirstChild(100)
	for i := 0; i < 30; i++ {
		if err := n.AppendInternal(key(i*10), core.PageID(101+i)); err != nil {
			t.Fatalf("AppendInternal(%d): %v", i, err)
		}
	}
	if err := n.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if n.Leaf() || n.Level() != 2 {
		t.Fatalf("leaf %v level %d, want false and 2", n.Leaf(), n.Level())
	}
	if n.ChildCount() != 31 {
		t.Fatalf("child count %d, want 31", n.ChildCount())
	}
	if n.Child(0) != 100 {
		t.Fatalf("child 0 is %d, want 100 (it lives in the node header)", n.Child(0))
	}
	for i := 1; i <= 30; i++ {
		if want := core.PageID(100 + i); n.Child(i) != want {
			t.Fatalf("child %d is %d, want %d", i, n.Child(i), want)
		}
	}
}

func TestChildForRoutesLikeTheTreeDoes(t *testing.T) {
	n, err := NewInternal(1, 1)
	if err != nil {
		t.Fatal(err)
	}
	n.SetFirstChild(10)
	for i, k := range []int{20, 40, 60} {
		if err := n.AppendInternal(key(k), core.PageID(11+i)); err != nil {
			t.Fatal(err)
		}
	}
	cases := []struct {
		k    int
		want core.PageID
	}{
		{0, 10}, {19, 10}, {20, 11}, {39, 11}, {40, 12}, {59, 12}, {60, 13}, {999, 13},
	}
	for _, c := range cases {
		if got := n.ChildFor(key(c.k)); got != c.want {
			t.Errorf("ChildFor(%d) = page %d, want %d", c.k, got, c.want)
		}
	}
}

func TestSearchFindsAndPlaces(t *testing.T) {
	n := NewLeaf(1)
	for _, k := range []int{10, 20, 30} {
		if err := n.AppendLeaf(key(k), rid(k)); err != nil {
			t.Fatal(err)
		}
	}
	cases := []struct {
		k     int
		at    int
		found bool
	}{
		{5, 0, false}, {10, 0, true}, {15, 1, false}, {20, 1, true},
		{25, 2, false}, {30, 2, true}, {35, 3, false},
	}
	for _, c := range cases {
		at, found := n.Search(key(c.k))
		if at != c.at || found != c.found {
			t.Errorf("Search(%d) = (%d, %v), want (%d, %v)", c.k, at, found, c.at, c.found)
		}
	}
}

func TestMaxKeysMatchesWhatActuallyFits(t *testing.T) {
	mkKey := func(keyLen, i int) []byte {
		k := bytes.Repeat([]byte{0xAA}, keyLen)
		var b [8]byte
		binary.BigEndian.PutUint64(b[:], uint64(i))
		n := keyLen
		if n > 8 {
			n = 8
		}
		copy(k[keyLen-n:], b[8-n:])
		return k
	}

	for keyLen := 1; keyLen <= 64; keyLen++ {
		leaf := NewLeaf(1)
		count := 0
		for {
			if err := leaf.AppendLeaf(mkKey(keyLen, count), rid(count)); err != nil {
				break
			}
			count++
		}
		if want := MaxKeys(page.KindBTreeLeaf, keyLen); count != want {
			t.Errorf("leaf with %d-byte keys held %d, MaxKeys says %d", keyLen, count, want)
		}

		internal, err := NewInternal(1, 1)
		if err != nil {
			t.Fatal(err)
		}
		count = 0
		for {
			if err := internal.AppendInternal(mkKey(keyLen, count), core.PageID(count)); err != nil {
				break
			}
			count++
		}
		if want := MaxKeys(page.KindBTreeInternal, keyLen); count != want {
			t.Errorf("internal with %d-byte keys held %d, MaxKeys says %d", keyLen, count, want)
		}
		if got := Fanout(keyLen); got != count+1 {
			t.Errorf("Fanout(%d) = %d, node held %d keys so it has %d children",
				keyLen, got, count, count+1)
		}
	}
}

func TestFullNodeRefusesAndStaysValid(t *testing.T) {
	n := NewLeaf(1)
	i := 0
	for {
		if err := n.AppendLeaf(key(i), rid(i)); err != nil {
			if !errors.Is(err, ErrNoFit) {
				t.Fatalf("append %d failed with %v, want ErrNoFit", i, err)
			}
			break
		}
		i++
	}
	if err := n.Validate(); err != nil {
		t.Fatalf("a full node failed Validate: %v", err)
	}
	if n.FreeSpace() < 0 || n.FreeSpace() >= n.EntrySize(8) {
		t.Fatalf("free space %d should be non-negative and smaller than one entry (%d)",
			n.FreeSpace(), n.EntrySize(8))
	}
	if n.Fits(8) {
		t.Error("Fits says another 8-byte key fits, but Append refused one")
	}
}

func TestGoldenLeafLayout(t *testing.T) {
	n := NewLeaf(7)
	n.SetNext(9)
	if err := n.AppendLeaf([]byte("aa"), core.RowID{Page: 3, Slot: 1}); err != nil {
		t.Fatal(err)
	}
	if err := n.AppendLeaf([]byte("bb"), core.RowID{Page: 4, Slot: 2}); err != nil {
		t.Fatal(err)
	}
	if err := n.Validate(); err != nil {
		t.Fatal(err)
	}

	want := []struct {
		at    int
		bytes []byte
		what  string
	}{
		{0, []byte{0, 0, 0, 7}, "page id 7"},
		{4, []byte{byte(page.KindBTreeLeaf)}, "kind btree-leaf"},
		{5, []byte{0}, "flags"},
		{6, []byte{0, 2}, "key count 2"},
		{8, []byte{0, 0x20}, "free start 32 = 24 + 2 slots"},
		{10, []byte{0x0F, 0xF0}, "free end 4080 = 4096 - 2 cells of 8"},
		{12, []byte{0, 0, 0, 0}, "reserved"},
		{16, []byte{0, 0, 0, 9}, "next leaf is page 9"},
		{20, []byte{0, 0}, "level 0"},
		{22, []byte{0, 0}, "padding"},
		{24, []byte{0x0F, 0xF8, 0, 8}, "slot 0 -> cell at 4088, 8 bytes"},
		{28, []byte{0x0F, 0xF0, 0, 8}, "slot 1 -> cell at 4080, 8 bytes"},
		{4088, []byte{'a', 'a', 0, 0, 0, 3, 0, 1}, "cell 0: key aa, row page 3 slot 1"},
		{4080, []byte{'b', 'b', 0, 0, 0, 4, 0, 2}, "cell 1: key bb, row page 4 slot 2"},
	}
	for _, w := range want {
		got := []byte(n.Page[w.at : w.at+len(w.bytes)])
		if !bytes.Equal(got, w.bytes) {
			t.Errorf("bytes %d..%d are % 02X, want % 02X (%s)",
				w.at, w.at+len(w.bytes)-1, got, w.bytes, w.what)
		}
	}

	for i := int(n.FreeStart()); i < int(n.FreeEnd()); i++ {
		if n.Page[i] != 0 {
			t.Fatalf("free space byte %d is %02X, want 00", i, n.Page[i])
		}
	}
}

func TestFirstKeysCellSitsHighestInThePage(t *testing.T) {
	n := NewLeaf(1)
	for i := 0; i < 5; i++ {
		if err := n.AppendLeaf(key(i), rid(i)); err != nil {
			t.Fatal(err)
		}
	}
	prev := page.Size + 1
	for i := 0; i < n.KeyCount(); i++ {
		lo, _ := n.cellBounds(i)
		if lo >= prev {
			t.Fatalf("cell %d starts at %d, not below the previous cell at %d", i, lo, prev)
		}
		prev = lo
	}
}

func TestKeyCopiesAndKeyRefViews(t *testing.T) {
	n := NewLeaf(1)
	if err := n.AppendLeaf([]byte("abcd"), rid(1)); err != nil {
		t.Fatal(err)
	}
	cp := n.Key(0)
	cp[0] = 'z'
	if !bytes.Equal(n.Key(0), []byte("abcd")) {
		t.Error("writing to the slice from Key changed the page; Key must copy")
	}
	ref := n.KeyRef(0)
	ref[0] = 'z'
	if !bytes.Equal(n.Key(0), []byte("zbcd")) {
		t.Error("writing to the slice from KeyRef did not change the page; KeyRef must be a view")
	}
}

func TestRoundTripThroughARealFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nodes.db")
	pf, err := page.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer pf.Close()

	leaf := NewLeaf(0)
	leaf.SetNext(1)
	for i := 0; i < 20; i++ {
		if err := leaf.AppendLeaf(key(i), rid(i)); err != nil {
			t.Fatal(err)
		}
	}
	if err := pf.Write(leaf.Page); err != nil {
		t.Fatal(err)
	}
	if err := pf.Sync(); err != nil {
		t.Fatal(err)
	}

	raw, err := pf.Read(0)
	if err != nil {
		t.Fatal(err)
	}
	back, err := Load(raw)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := back.Validate(); err != nil {
		t.Fatalf("Validate after a disk round trip: %v", err)
	}
	if back.Next() != 1 || back.KeyCount() != 20 {
		t.Fatalf("next %d count %d, want 1 and 20", back.Next(), back.KeyCount())
	}
	for i := 0; i < 20; i++ {
		if !bytes.Equal(back.Key(i), key(i)) || back.Row(i) != rid(i) {
			t.Fatalf("entry %d did not survive the round trip", i)
		}
	}
	if !bytes.Equal(back.Page, leaf.Page) {
		t.Error("the page read back is not byte-identical to the one written")
	}
}

func TestLoadRejectsPagesThatAreNotNodes(t *testing.T) {
	for _, k := range []page.Kind{page.KindFree, page.KindMeta, page.KindHeap} {
		p := page.New(1, k)
		if _, err := Load(p); !errors.Is(err, ErrBadKind) {
			t.Errorf("Load of a %s page gave %v, want ErrBadKind", k, err)
		}
	}
	if _, err := Load(make(page.Page, 10)); !errors.Is(err, core.ErrShortPage) {
		t.Errorf("Load of a short page gave %v, want ErrShortPage", err)
	}
}

func TestNewInternalRejectsLevelZero(t *testing.T) {
	if _, err := NewInternal(1, 0); !errors.Is(err, ErrBadLevel) {
		t.Errorf("NewInternal at level 0 gave %v, want ErrBadLevel", err)
	}
}

func TestAppendRejectsTheWrongKind(t *testing.T) {
	leaf := NewLeaf(1)
	if err := leaf.AppendInternal(key(1), 2); !errors.Is(err, ErrBadKind) {
		t.Errorf("AppendInternal on a leaf gave %v, want ErrBadKind", err)
	}
	internal, err := NewInternal(1, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := internal.AppendLeaf(key(1), rid(1)); !errors.Is(err, ErrBadKind) {
		t.Errorf("AppendLeaf on an internal node gave %v, want ErrBadKind", err)
	}
}

func TestValidateCatchesCorruption(t *testing.T) {
	build := func() Node {
		n := NewLeaf(1)
		for i := 0; i < 6; i++ {
			if err := n.AppendLeaf(key(i), rid(i)); err != nil {
				t.Fatal(err)
			}
		}
		return n
	}

	cases := []struct {
		name    string
		corrupt func(Node)
	}{
		{"key count no longer matches free start", func(n Node) { n.SetItemCount(5) }},
		{"free start moved", func(n Node) { n.SetFreeStart(n.FreeStart() + 4) }},
		{"free end moved", func(n Node) { n.SetFreeEnd(n.FreeEnd() - 8) }},
		{"a slot points into the slot array", func(n Node) {
			binary.BigEndian.PutUint16(n.Page[n.slotOffset(2):], SlotArrayStart)
		}},
		{"a slot points past the page", func(n Node) {
			binary.BigEndian.PutUint16(n.Page[n.slotOffset(2):], page.Size-2)
		}},
		{"a cell is too short to hold a row id", func(n Node) {
			binary.BigEndian.PutUint16(n.Page[n.slotOffset(2)+2:], 4)
		}},
		{"keys are out of order", func(n Node) {
			a, b := n.slotOffset(2), n.slotOffset(3)
			var tmp [SlotSize]byte
			copy(tmp[:], n.Page[a:a+SlotSize])
			copy(n.Page[a:a+SlotSize], n.Page[b:b+SlotSize])
			copy(n.Page[b:b+SlotSize], tmp[:])
		}},
		{"two slots point at the same cell", func(n Node) {
			lo, _ := n.cellBounds(3)
			binary.BigEndian.PutUint16(n.Page[n.slotOffset(2):], uint16(lo))
		}},
		{"padding was used for something", func(n Node) { n.Page[offPad] = 1 }},
		{"a leaf claims a level", func(n Node) { n.setLevel(3) }},
		{"the kind byte was flipped", func(n Node) { n.SetKind(page.KindHeap) }},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			n := build()
			if err := n.Validate(); err != nil {
				t.Fatalf("the clean node did not validate: %v", err)
			}
			c.corrupt(n)
			if err := n.Validate(); err == nil {
				t.Errorf("Validate accepted a node where %s", c.name)
			}
		})
	}
}

func TestEmptyNodeIsValid(t *testing.T) {
	leaf := NewLeaf(3)
	if err := leaf.Validate(); err != nil {
		t.Errorf("an empty leaf failed Validate: %v", err)
	}
	if leaf.FreeSpace() != EntryArea {
		t.Errorf("an empty leaf has %d free, want %d", leaf.FreeSpace(), EntryArea)
	}
	internal, err := NewInternal(3, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := internal.Validate(); err != nil {
		t.Errorf("an empty internal node failed Validate: %v", err)
	}
}

func TestDescribeMentionsWhatItDecoded(t *testing.T) {
	n := NewLeaf(7)
	n.SetNext(9)
	if err := n.AppendLeaf([]byte("aa"), core.RowID{Page: 3, Slot: 1}); err != nil {
		t.Fatal(err)
	}
	got := n.Describe()
	for _, want := range []string{
		"page 7", "btree-leaf", "next leaf = page 9", "0F F8", "row id -> page 3 slot 1",
	} {
		if !bytes.Contains([]byte(got), []byte(want)) {
			t.Errorf("Describe output is missing %q\n%s", want, got)
		}
	}
}

func TestFanoutTableIsHonestAboutOverhead(t *testing.T) {
	out := FanoutTable(8)
	if !bytes.Contains([]byte(out), []byte(fmt.Sprintf("%d", EntryArea))) {
		t.Errorf("FanoutTable should show the %d bytes left for entries\n%s", EntryArea, out)
	}
}
