package page

import (
	"fmt"
	"testing"
)

func TestAppendUpdatesHeader(t *testing.T) {
	p := New(1, KindHeap)
	if _, err := p.Append([]byte("scute-db")); err != nil {
		t.Fatal(err)
	}
	fmt.Printf("BEFORE  bytes 6-9: % 02X   itemCount=%d  freeStart=%d (0x%02X)\n",
		p[6:10], p.ItemCount(), p.FreeStart(), p.FreeStart())

	if _, err := p.Append([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	fmt.Printf("AFTER   bytes 6-9: % 02X   itemCount=%d  freeStart=%d (0x%02X)\n",
		p[6:10], p.ItemCount(), p.FreeStart(), p.FreeStart())
	fmt.Printf("        data area:  %q\n", string(p[HeaderSize:p.FreeStart()]))

	want := []byte{0x00, 0x02, 0x00, 0x1D}
	for i, w := range want {
		if p[6+i] != w {
			t.Fatalf("byte %d = %02X, want %02X", 6+i, p[6+i], w)
		}
	}
}
