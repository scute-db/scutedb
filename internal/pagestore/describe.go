package pagestore

import (
	"fmt"
	"strings"

	"github.com/scute-db/scutedb/internal/core"
	"github.com/scute-db/scutedb/internal/page"
)

func (s *Store) DescribeMeta() (string, error) {
	if err := s.usable(); err != nil {
		return "", err
	}
	p, err := s.pf.Read(MetaPage)
	if err != nil {
		return "", err
	}
	return describeMetaPage(p), nil
}

func hexRows(p []byte, from, to int) string {
	var b strings.Builder
	for off := from; off < to; off += 16 {
		fmt.Fprintf(&b, "%04X  ", off)
		for i := 0; i < 16; i++ {
			if i == 8 {
				b.WriteByte(' ')
			}
			fmt.Fprintf(&b, "%02X ", p[off+i])
		}
		b.WriteString(" |")
		for i := 0; i < 16; i++ {
			c := p[off+i]
			if c >= 0x20 && c < 0x7F {
				b.WriteByte(c)
			} else {
				b.WriteByte('.')
			}
		}
		b.WriteString("|\n")
	}
	return b.String()
}

func describeMetaPage(p page.Page) string {
	var b strings.Builder
	fmt.Fprint(&b, "page 0, the meta page\n\n")
	b.WriteString(hexRows(p, 0, 64))

	fmt.Fprintf(&b, "\n%-9s %-26s %s\n", "offset", "bytes", "meaning")
	fmt.Fprintln(&b, strings.Repeat("-", 76))
	row := func(off, size int, meaning string) {
		fmt.Fprintf(&b, "%-9s %-26s %s\n",
			fmt.Sprintf("%d..%d", off, off+size-1),
			fmt.Sprintf("% 02X", p[off:off+size]), meaning)
	}
	row(0, 4, fmt.Sprintf("page id = %d", p.ID()))
	row(4, 1, fmt.Sprintf("kind = %d (%s)", p[4], p.Kind()))
	row(offMagic, 8, fmt.Sprintf("magic = %q, NUL-terminated", string(p[offMagic:offMagic+7])))

	m, err := decodeMeta(p)
	if err != nil {
		fmt.Fprintf(&b, "\nnot a readable meta page: %v\n", err)
		return b.String()
	}
	row(offVersion, 4, fmt.Sprintf("format version = %d", m.Version))
	row(offPageSize, 4, fmt.Sprintf("page size = %d", m.PageSize))
	row(offRoot, 4, rootLabel(m.Root))
	row(offFreeList, 4, listLabel(m.FreeListPage))
	row(offFreeCount, 4, fmt.Sprintf("free page count = %d", m.FreeCount))
	row(offNextPage, 4, fmt.Sprintf("next page to hand out = %d", m.NextPage))
	row(offFilePages, 4, fmt.Sprintf("pages the file holds = %d (%d bytes)", m.FilePages, int(m.FilePages)*page.Size))
	row(offCheckpoint, 4, fmt.Sprintf("checkpoint generation = %d", m.Checkpoint))
	return b.String()
}

func rootLabel(id core.PageID) string {
	if id == NoPage {
		return "root = none yet (0 is safe: page 0 is always this page)"
	}
	return fmt.Sprintf("root = page %d", id)
}

func listLabel(id core.PageID) string {
	if id == NoPage {
		return "free list = none (nothing is free)"
	}
	return fmt.Sprintf("free list starts on page %d", id)
}

func joinIDs(ids []core.PageID) string {
	if len(ids) == 0 {
		return "none"
	}
	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = fmt.Sprintf("%d", id)
	}
	return strings.Join(parts, " ")
}

func (s *Store) DescribeFreeList() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%-26s %s\n", "reusable now", joinIDs(s.free))
	fmt.Fprintf(&b, "%-26s %s\n", "pending, after checkpoint", joinIDs(s.pending))
	fmt.Fprintf(&b, "%-26s %s\n", "holding the free list", joinIDs(s.listPage))
	return b.String()
}

func (s *Store) Describe() string {
	var b strings.Builder
	used := int(s.nextPage) - 1 - len(s.free) - len(s.pending) - len(s.listPage)
	fmt.Fprintf(&b, "%-28s %d pages, %d bytes\n", "file holds", s.filePages, int(s.filePages)*page.Size)
	fmt.Fprintf(&b, "%-28s %d\n", "handed out so far", int(s.nextPage)-1)
	fmt.Fprintf(&b, "%-28s %d\n", "  in use", used)
	fmt.Fprintf(&b, "%-28s %d\n", "  free, reusable now", len(s.free))
	fmt.Fprintf(&b, "%-28s %d\n", "  freed, waiting on a checkpoint", len(s.pending))
	fmt.Fprintf(&b, "%-28s %d\n", "  holding the free list", len(s.listPage))
	fmt.Fprintf(&b, "%-28s %d\n", "never handed out", s.filePages-uint32(s.nextPage))
	fmt.Fprintf(&b, "%-28s %s\n", "root", rootLabel(s.root))
	fmt.Fprintf(&b, "%-28s %d\n", "checkpoint generation", s.durable.Checkpoint)
	return b.String()
}
