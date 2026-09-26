package nodepage

import (
	"fmt"
	"strings"

	"github.com/scute-db/scutedb/internal/page"
)

func Hexdump(b []byte, base int) string {
	var s strings.Builder
	for off := 0; off < len(b); off += 16 {
		end := off + 16
		if end > len(b) {
			end = len(b)
		}
		line := b[off:end]
		fmt.Fprintf(&s, "%04X  ", base+off)
		for i := 0; i < 16; i++ {
			if i == 8 {
				s.WriteByte(' ')
			}
			if i < len(line) {
				fmt.Fprintf(&s, "%02X ", line[i])
			} else {
				s.WriteString("   ")
			}
		}
		s.WriteString(" |")
		for _, c := range line {
			if c >= 0x20 && c < 0x7F {
				s.WriteByte(c)
			} else {
				s.WriteByte('.')
			}
		}
		s.WriteString("|\n")
	}
	return s.String()
}

func (n Node) Map() string {
	var s strings.Builder
	count := n.KeyCount()
	fmt.Fprintf(&s, "%-14s %-13s %s\n", "region", "bytes", "holds")
	fmt.Fprintln(&s, strings.Repeat("-", 62))
	fmt.Fprintf(&s, "%-14s %-13s %s\n", "page header",
		fmt.Sprintf("0..%d", page.HeaderSize-1), "id, kind, item count, free start, free end")
	fmt.Fprintf(&s, "%-14s %-13s %s\n", "node header",
		fmt.Sprintf("%d..%d", page.HeaderSize, SlotArrayStart-1), n.linkMeaning()+", level, 2 pad")
	fmt.Fprintf(&s, "%-14s %-13s %s\n", "slot array",
		fmt.Sprintf("%d..%d", SlotArrayStart, int(n.FreeStart())-1),
		fmt.Sprintf("%d slots x %d bytes, in key order", count, SlotSize))
	fmt.Fprintf(&s, "%-14s %-13s %s\n", "free space",
		fmt.Sprintf("%d..%d", n.FreeStart(), int(n.FreeEnd())-1),
		fmt.Sprintf("%d bytes, grows shut from both ends", n.FreeSpace()))
	fmt.Fprintf(&s, "%-14s %-13s %s\n", "cells",
		fmt.Sprintf("%d..%d", n.FreeEnd(), page.Size-1),
		fmt.Sprintf("%d cells, packed from the end, insertion order", count))
	return s.String()
}

func (n Node) linkMeaning() string {
	if n.Leaf() {
		return "next leaf"
	}
	return "first child"
}

func (n Node) Describe() string {
	var s strings.Builder

	fmt.Fprintf(&s, "page %d  %s  level %d  %d keys\n\n", n.ID(), n.Kind(), n.Level(), n.KeyCount())
	s.WriteString(n.Map())

	fmt.Fprintf(&s, "\nheaders, byte for byte\n\n")
	fmt.Fprint(&s, Hexdump(n.Page[:SlotArrayStart], 0))
	fmt.Fprintf(&s, "\n%-9s %-14s %s\n", "offset", "bytes", "meaning")
	fmt.Fprintln(&s, strings.Repeat("-", 62))
	row := func(off, size int, meaning string) {
		fmt.Fprintf(&s, "%-9s %-14s %s\n",
			fmt.Sprintf("%d..%d", off, off+size-1),
			fmt.Sprintf("% 02X", n.Page[off:off+size]), meaning)
	}
	row(0, 4, fmt.Sprintf("page id = %d", n.ID()))
	row(4, 1, fmt.Sprintf("kind = %d (%s)", n.Page[4], n.Kind()))
	row(5, 1, fmt.Sprintf("flags = %08b", n.Flags()))
	row(6, 2, fmt.Sprintf("key count = %d", n.KeyCount()))
	row(8, 2, fmt.Sprintf("free start = %d (slot array ends here)", n.FreeStart()))
	row(10, 2, fmt.Sprintf("free end = %d (lowest cell starts here)", n.FreeEnd()))
	row(12, 4, "reserved for the checksum in 0x13")
	row(offLink, 4, fmt.Sprintf("%s = page %d", n.linkMeaning(), n.link()))
	row(offLevel, 2, fmt.Sprintf("level = %d (0 means leaf)", n.Level()))
	row(offPad, 2, "padding, keeps the slot array 4-byte aligned")

	count := n.KeyCount()
	if count == 0 {
		fmt.Fprintf(&s, "\nno entries\n")
		return s.String()
	}

	fmt.Fprintf(&s, "\nslot array at %d, %d slots of %d bytes\n\n", SlotArrayStart, count, SlotSize)
	fmt.Fprint(&s, Hexdump(n.Page[SlotArrayStart:n.FreeStart()], SlotArrayStart))
	fmt.Fprintf(&s, "\n%-6s %-9s %-12s %-9s %s\n", "slot", "at", "bytes", "cell at", "length")
	fmt.Fprintln(&s, strings.Repeat("-", 62))
	for i := 0; i < count; i++ {
		at := n.slotOffset(i)
		lo, hi := n.cellBounds(i)
		fmt.Fprintf(&s, "%-6d %-9d %-12s %-9d %d\n",
			i, at, fmt.Sprintf("% 02X", n.Page[at:at+SlotSize]), lo, hi-lo)
	}

	fmt.Fprintf(&s, "\ncells from %d to %d, packed downward\n\n", n.FreeEnd(), page.Size-1)
	fmt.Fprint(&s, Hexdump(n.Page[n.FreeEnd():], int(n.FreeEnd())))

	fmt.Fprintf(&s, "\n%-6s %-9s %-22s %s\n", "slot", "cell at", "key bytes", "then")
	fmt.Fprintln(&s, strings.Repeat("-", 68))
	for i := 0; i < count; i++ {
		lo, _ := n.cellBounds(i)
		key := n.KeyRef(i)
		var tail string
		if n.Leaf() {
			r := n.Row(i)
			tail = fmt.Sprintf("row id -> page %d slot %d", r.Page, r.Slot)
		} else {
			tail = fmt.Sprintf("child -> page %d", n.Child(i+1))
		}
		fmt.Fprintf(&s, "%-6d %-9d %-22s %s\n", i, lo, fmt.Sprintf("% 02X", key), tail)
	}

	if !n.Leaf() {
		fmt.Fprintf(&s, "\n%d keys, %d children\n", count, n.ChildCount())
		fmt.Fprintf(&s, "  child 0 = page %d  (from the node header, keys below key 0)\n", n.Child(0))
		for i := 1; i <= count; i++ {
			fmt.Fprintf(&s, "  child %d = page %d  (from slot %d, keys >= % 02X)\n",
				i, n.Child(i), i-1, n.KeyRef(i-1))
		}
	}
	return s.String()
}

func FanoutTable(keyLen int) string {
	var s strings.Builder
	fmt.Fprintf(&s, "page %d bytes, key %d bytes\n\n", page.Size, keyLen)
	fmt.Fprintf(&s, "  page header      %4d\n", page.HeaderSize)
	fmt.Fprintf(&s, "  node header      %4d\n", NodeHeaderSize)
	fmt.Fprintf(&s, "  left for entries %4d\n\n", EntryArea)

	internal := MaxKeys(page.KindBTreeInternal, keyLen)
	leaf := MaxKeys(page.KindBTreeLeaf, keyLen)
	fmt.Fprintf(&s, "  internal entry = %d slot + %d key + %d child  = %d bytes -> %d keys, %d children\n",
		SlotSize, keyLen, InternalSuffix, SlotSize+keyLen+InternalSuffix, internal, internal+1)
	fmt.Fprintf(&s, "  leaf entry     = %d slot + %d key + %d row id = %d bytes -> %d keys\n\n",
		SlotSize, keyLen, LeafSuffix, SlotSize+keyLen+LeafSuffix, leaf)

	fmt.Fprintf(&s, "  %-8s %-14s %s\n", "levels", "reads per get", "keys it holds")
	fmt.Fprintln(&s, "  "+strings.Repeat("-", 46))
	capacity := float64(leaf)
	for level := 1; level <= 4; level++ {
		fmt.Fprintf(&s, "  %-8d %-14d %s\n", level, level, commas(capacity))
		capacity *= float64(internal + 1)
	}
	return s.String()
}

func commas(v float64) string {
	s := fmt.Sprintf("%.0f", v)
	if len(s) > 15 {
		return fmt.Sprintf("%.3g", v)
	}
	var out []byte
	for i, c := range []byte(s) {
		if i > 0 && (len(s)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, c)
	}
	return string(out)
}
