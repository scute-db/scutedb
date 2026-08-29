package page

import (
	"fmt"
	"strings"
)

func (p Page) Describe() string {
	var b strings.Builder
	fmt.Fprintf(&b, "raw header:  % 02X\n\n", p[:HeaderSize])
	fmt.Fprintf(&b, "%-7s %-14s %-34s %s\n", "offset", "bytes", "calculation", "value")
	fmt.Fprintln(&b, strings.Repeat("-", 82))

	row := func(off int, n int, calc string, val string) {
		fmt.Fprintf(&b, "%-7s %-14s %-34s %s\n",
			fmt.Sprintf("%d-%d", off, off+n-1),
			fmt.Sprintf("% 02X", p[off:off+n]), calc, val)
	}

	id := p.ID()
	row(offID, 4, fmt.Sprintf("%d*16777216 + %d*65536 + %d*256 + %d",
		p[0], p[1], p[2], p[3]), fmt.Sprintf("page id = %d", id))
	row(offKind, 1, "single byte, a code", fmt.Sprintf("kind = %d (%s)", p[offKind], p.Kind()))
	row(offFlags, 1, "8 on/off bits", fmt.Sprintf("flags = %08b", p[offFlags]))
	row(offItemCount, 2, fmt.Sprintf("%d*256 + %d", p[6], p[7]),
		fmt.Sprintf("item count = %d", p.ItemCount()))
	row(offFreeStart, 2, fmt.Sprintf("%d*256 + %d", p[8], p[9]),
		fmt.Sprintf("free start = %d", p.FreeStart()))
	row(offFreeEnd, 2, fmt.Sprintf("%d*256 + %d", p[10], p[11]),
		fmt.Sprintf("free end = %d", p.FreeEnd()))
	row(offReserved, 4, "unused until 0x13", "reserved (checksum)")

	fmt.Fprintf(&b, "\nfree space = %d - %d = %d bytes\n",
		p.FreeEnd(), p.FreeStart(), p.FreeSpace())
	used := int(p.FreeStart()) - HeaderSize
	if used > 0 {
		fmt.Fprintf(&b, "data area  = bytes %d..%d  %q\n",
			HeaderSize, int(p.FreeStart())-1, string(p[HeaderSize:p.FreeStart()]))
	}
	return b.String()
}
