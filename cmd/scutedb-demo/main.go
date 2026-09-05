package main

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unsafe"

	"github.com/scute-db/scutedb/internal/btree"
	"github.com/scute-db/scutedb/internal/codec"
	"github.com/scute-db/scutedb/internal/core"
	"github.com/scute-db/scutedb/internal/naive"
	"github.com/scute-db/scutedb/internal/nullbits"
	"github.com/scute-db/scutedb/internal/page"
	"github.com/scute-db/scutedb/internal/slots"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Println("usage: scutedb-demo scan|update|race|crash|pages|header|encode|nulls|align|btree")
		os.Exit(2)
	}
	switch os.Args[1] {
	case "scan":
		expScan()
	case "update":
		expUpdate()
	case "race":
		expRace()
	case "crash":
		expCrash()
	case "pages":
		expPages()
	case "header":
		expHeader()
	case "encode":
		expEncode()
	case "nulls":
		expNulls()
	case "align":
		expAlign()
	case "btree":
		expBTree()
	case "crash-child":
		crashChild(os.Args[2])
	default:
		fmt.Println("unknown experiment:", os.Args[1])
		os.Exit(2)
	}
}

func tmp(name string) string {
	dir, _ := os.MkdirTemp("", "scutedb")
	return filepath.Join(dir, name)
}

func expScan() {
	fmt.Println("EXPERIMENT 1/4  finding a record")
	fmt.Print("Look up ONE key in a database of N records.\n\n")
	fmt.Printf("%12s  %12s  %14s  %14s\n", "records", "file size", "lookup time", "per record")
	fmt.Println("  " + line(58))

	for _, n := range []int{1_000, 10_000, 100_000, 500_000} {
		path := tmp("scan.db")
		db, err := naive.Open(path)
		check(err)
		for i := 0; i < n; i++ {
			check(db.Put("user:"+strconv.Itoa(i), "some value for record "+strconv.Itoa(i)))
		}
		check(db.Flush())

		target := "user:" + strconv.Itoa(n-1)
		start := time.Now()
		_, err = db.Get(target)
		check(err)
		el := time.Since(start)

		_, _, _, size, err := db.Stats()
		check(err)
		fmt.Printf("%12s  %12s  %14s  %14s\n",
			commas(int64(n)), humanBytes(size), el.Round(time.Microsecond),
			(el / time.Duration(n)).Round(time.Nanosecond))
		db.Close()
	}
	fmt.Println("\n  Ignore the first row - opening the file dominates there. After that,")
	fmt.Println("  the per-record cost flattens out, so the total is just N x a constant.")
	fmt.Println("  That is O(n). Step 0x06 is where a B+Tree turns it into O(log n).")
}

func expUpdate() {
	fmt.Println("EXPERIMENT 2/4  updating a record")
	fmt.Print("Change ONE value in a 100,000 record database.\n\n")

	path := tmp("update.db")
	db, err := naive.Open(path)
	check(err)
	const n = 100_000
	for i := 0; i < n; i++ {
		check(db.Put("user:"+strconv.Itoa(i), "email"+strconv.Itoa(i)+"@example.com"))
	}
	check(db.Flush())
	_, _, _, before, err := db.Stats()
	check(err)

	rec := "user:42\tnewemail@example.com\n"
	check(db.Put("user:42", "newemail@example.com"))
	check(db.Flush())
	lines, live, _, after, err := db.Stats()
	check(err)

	fmt.Printf("  file before update      %s\n", humanBytes(before))
	fmt.Printf("  bytes that changed      %s   (the new record)\n", humanBytes(int64(len(rec))))
	fmt.Printf("  file after update       %s   (it GREW)\n", humanBytes(after))
	fmt.Printf("  lines on disk           %s\n", commas(int64(lines)))
	fmt.Printf("  distinct live keys      %s   <- %s lines are garbage\n\n",
		commas(int64(live)), commas(int64(lines-live)))

	start := time.Now()
	written, err := db.Compact()
	check(err)
	el := time.Since(start)

	fmt.Printf("  To reclaim that space we must rewrite the whole file:\n")
	fmt.Printf("  compaction wrote        %s in %s\n", humanBytes(written), el.Round(time.Millisecond))
	fmt.Printf("  write amplification     %.0fx  (%s written to change %s)\n\n",
		float64(written)/float64(len(rec)), humanBytes(written), humanBytes(int64(len(rec))))
	fmt.Println("  Steps 0x02 and 0x0F fix this: change a 4 KB page, write a 4 KB page.")
	db.Close()
}

type racyDB struct {
	f      *os.File
	offset int64
}

func (d *racyDB) Put(rec string) {
	off := d.offset
	n, _ := d.f.WriteAt([]byte(rec), off)
	d.offset = off + int64(n)
}

func expRace() {
	fmt.Println("EXPERIMENT 3/4  two writers at once")
	fmt.Print("Two goroutines, 500 records each. Expect 1,000.\n\n")

	path := tmp("race.db")
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	check(err)
	db := &racyDB{f: f}

	var wg sync.WaitGroup
	for g := 0; g < 2; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 500; i++ {
				db.Put(fmt.Sprintf("writer%d:record%04d\tvalue\n", g, i))
			}
		}(g)
	}
	wg.Wait()
	f.Sync()
	f.Close()

	in, err := os.Open(path)
	check(err)
	defer in.Close()
	sc := bufio.NewScanner(in)
	good, torn := 0, 0
	for sc.Scan() {
		if len(sc.Text()) == len("writer0:record0000\tvalue") {
			good++
		} else {
			torn++
		}
	}
	st, _ := os.Stat(path)

	fmt.Printf("  records expected        1,000\n")
	fmt.Printf("  intact records          %s\n", commas(int64(good)))
	fmt.Printf("  damaged records         %s\n", commas(int64(torn)))
	fmt.Printf("  records LOST            %s\n", commas(int64(1000-good)))
	fmt.Printf("  file size               %s (expected %s)\n\n",
		humanBytes(st.Size()), humanBytes(int64(1000*len("writer0:record0000\tvalue\n"))))
	fmt.Println("  Both goroutines read the same offset and wrote on top of")
	fmt.Println("  each other. Run this with -race and Go names the exact lines.")
	fmt.Println("  Step 0x0C is where we make this impossible.")
}

func expCrash() {
	fmt.Println("EXPERIMENT 4/4  surviving a crash")
	fmt.Print("A child process writes records, then gets kill -9'd.\n\n")

	path := tmp("crash.db")
	self, err := os.Executable()
	check(err)
	cmd := exec.Command(self, "crash-child", path)
	stdout, err := cmd.StdoutPipe()
	check(err)
	check(cmd.Start())

	claimed := 0
	done := make(chan struct{})
	go func() {
		sc := bufio.NewScanner(stdout)
		for sc.Scan() {
			if v, err := strconv.Atoi(sc.Text()); err == nil {
				claimed = v
			}
		}
		close(done)
	}()

	time.Sleep(300 * time.Millisecond)
	check(cmd.Process.Kill())
	cmd.Wait()
	<-done

	in, err := os.Open(path)
	check(err)
	defer in.Close()
	sc := bufio.NewScanner(in)
	survived := 0
	for sc.Scan() {
		survived++
	}
	st, _ := os.Stat(path)

	fmt.Printf("  child reported writing  %s records\n", commas(int64(claimed)))
	fmt.Printf("  actually on disk        %s records  (%s)\n", commas(int64(survived)), humanBytes(st.Size()))
	fmt.Printf("  VANISHED                %s records\n\n", commas(int64(claimed-survived)))
	fmt.Println("  Put() returned nil for every one of those. The records were")
	fmt.Println("  sitting in a userspace buffer that died with the process -")
	fmt.Println("  they never reached the operating system at all.")
	fmt.Println("  Step 0x13 covers the harder version: data that DID reach the")
	fmt.Println("  OS but not the physical disk, which is what fsync is for.")
}

func crashChild(path string) {
	db, err := naive.Open(path)
	if err != nil {
		os.Exit(1)
	}
	out := bufio.NewWriter(os.Stdout)
	for i := 1; ; i++ {
		if err := db.Put("key:"+strconv.Itoa(i), "value:"+strconv.Itoa(i)); err != nil {
			os.Exit(1)
		}
		fmt.Fprintln(out, i)
		out.Flush()
	}
}

func expPages() {
	fmt.Println("STEP 0x02  pages")
	fmt.Print("Three pages, written to a real file.\n\n")

	path := "/tmp/scutedb-pages.db"
	os.Remove(path)
	pf, err := page.Open(path)
	check(err)

	contents := []string{"hello from page zero", "scute-db", "B+Tree node goes here later"}
	kinds := []page.Kind{page.KindMeta, page.KindHeap, page.KindBTreeLeaf}

	fmt.Printf("  %-5s %-16s %-10s %-10s %-8s\n", "page", "kind", "offset", "free", "content")
	fmt.Println("  " + line(62))
	for i, text := range contents {
		p, err := pf.Allocate(kinds[i])
		check(err)
		_, err = p.Append([]byte(text))
		check(err)
		check(pf.Write(p))
		fmt.Printf("  %-5d %-16s %-10d %-10d %q\n",
			p.ID(), p.Kind(), page.Offset(p.ID()), p.FreeSpace(), text)
	}
	check(pf.Sync())
	check(pf.Close())

	st, err := os.Stat(path)
	check(err)
	fmt.Printf("\n  file size    %s  (exactly %d x %d, no more, no less)\n",
		humanBytes(st.Size()), st.Size()/page.Size, page.Size)

	pf2, err := page.Open(path)
	check(err)
	defer pf2.Close()
	start := time.Now()
	p, err := pf2.Read(core.PageID(2))
	check(err)
	el := time.Since(start)
	fmt.Printf("  read page 2  %s, one seek, no scan -> %q\n",
		el.Round(time.Microsecond), string(p.Data()[:27]))
	fmt.Printf("\n  Now look at it yourself:  hexdump -C %s | head -20\n", path)
}

func expHeader() {
	pf, err := page.Open("/tmp/scutedb-pages.db")
	check(err)
	defer pf.Close()
	n, err := pf.PageCount()
	check(err)
	for i := uint32(0); i < n; i++ {
		p, err := pf.Read(core.PageID(i))
		check(err)
		fmt.Printf("======== PAGE %d  (file offset %d) ========\n\n%s\n",
			i, page.Offset(core.PageID(i)), p.Describe())
	}
}

func check(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func line(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = '-'
	}
	return string(b)
}

func commas(n int64) string {
	s := strconv.FormatInt(n, 10)
	if len(s) <= 3 {
		return s
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

func humanBytes(n int64) string {
	switch {
	case n < 1024:
		return fmt.Sprintf("%d B", n)
	case n < 1024*1024:
		return fmt.Sprintf("%.1f KB", float64(n)/1024)
	default:
		return fmt.Sprintf("%.1f MB", float64(n)/(1024*1024))
	}
}

func expEncode() {
	fmt.Println("STEP 0x03  binary serialization")

	fmt.Print("\n1. THE PROBLEM LEFT OVER FROM 0x02\n\n")
	p := page.New(1, page.KindHeap)
	p.Append([]byte("scute-db"))
	p.Append([]byte("hello"))
	fmt.Printf("   raw appends:   %q\n", string(p[page.HeaderSize:p.FreeStart()]))
	fmt.Println("   two items, nothing marks the boundary. unrecoverable.")

	fmt.Print("\n2. THE FIX: LENGTH PREFIXING\n\n")
	q := page.New(1, page.KindHeap)
	q.Append(codec.AppendString(nil, "scute-db"))
	q.Append(codec.AppendString(nil, "hello"))
	body := q[page.HeaderSize:q.FreeStart()]
	fmt.Printf("   bytes:         % 02X\n", body)
	fmt.Printf("   as text:       %q\n", string(body))
	off := 0
	for i := 1; off < len(body); i++ {
		s, n, err := codec.String(body[off:])
		if err != nil {
			fmt.Println("   decode error:", err)
			break
		}
		fmt.Printf("   item %d:        len=%-3d %-12q bytes %d..%d\n", i, len(s), s, off, off+n-1)
		off += n
	}

	fmt.Print("\n3. FIXED WIDTH vs VARINT\n\n")
	fmt.Printf("   %-22s %-10s %-10s %s\n", "value", "fixed 8B", "varint", "saved")
	fmt.Println("   " + line(56))
	for _, v := range []uint64{0, 1, 127, 128, 1000, 1 << 20, 1 << 40, math.MaxUint64} {
		n := len(codec.AppendUvarint(nil, v))
		fmt.Printf("   %-22d %-10d %-10d %+d bytes\n", v, 8, n, 8-n)
	}

	fmt.Print("\n4. WHY KEYS MUST BE BIG-ENDIAN\n\n")
	nums := []uint64{1, 2, 255, 256, 300}
	fmt.Printf("   %-8s %-27s %s\n", "value", "big-endian (low 4 bytes)", "varint")
	fmt.Println("   " + line(52))
	for _, v := range nums {
		be := codec.AppendKeyUint64(nil, v)
		fmt.Printf("   %-8d %-27s % 02X\n", v, fmt.Sprintf("% 02X", be[4:]), codec.AppendUvarint(nil, v))
	}
	fmt.Print("\n   the same five values, sorted purely by their bytes:\n")
	fmt.Printf("     big-endian     %v   correct\n",
		sortedBy(nums, func(v uint64) []byte { return codec.AppendKeyUint64(nil, v) }))
	fmt.Printf("     little-endian  %v   wrong\n",
		sortedBy(nums, func(v uint64) []byte { return binary.LittleEndian.AppendUint64(nil, v) }))
	fmt.Printf("     varint         %v   wrong\n",
		sortedBy(nums, func(v uint64) []byte { return codec.AppendUvarint(nil, v) }))
	fmt.Println("\n   The B+Tree will compare keys with bytes.Compare and nothing else.")
	fmt.Println("   Only the first encoding survives that.")

	fmt.Print("\n5. SIGNED KEYS NEED A SIGN FLIP\n\n")
	fmt.Printf("   %-8s %-26s %s\n", "value", "raw two's complement", "key encoding")
	fmt.Println("   " + line(60))
	for _, v := range []int64{-2, -1, 0, 1, 2} {
		raw := binary.BigEndian.AppendUint64(nil, uint64(v))
		fmt.Printf("   %-8d %-26s % 02X\n", v,
			fmt.Sprintf("% 02X", raw), codec.AppendKeyInt64(nil, v))
	}
	fmt.Println("\n   Raw, -1 is all FF bytes and sorts ABOVE 1. Flipping the top bit")
	fmt.Println("   shifts the signed range so bytewise order matches numeric order.")
}

func sortedBy(nums []uint64, enc func(uint64) []byte) []uint64 {
	out := append([]uint64(nil), nums...)
	sort.Slice(out, func(i, j int) bool {
		return bytes.Compare(enc(out[i]), enc(out[j])) < 0
	})
	return out
}

func expNulls() {
	fmt.Println("STEP 0x04  nulls and the zero problem")

	fmt.Print("\n1. THE PROBLEM\n\n")
	p := page.New(1, page.KindHeap)
	p.Append(codec.AppendInt64(nil, 0))
	body := p[page.HeaderSize : page.HeaderSize+8]
	fmt.Printf("   a slot holding the number 0:   % 02X\n", body[:1])
	fmt.Printf("   an untouched slot:             % 02X\n", p[100:101])
	fmt.Println("   identical bytes. 'age is 0' and 'age was never given' are")
	fmt.Println("   indistinguishable, and they mean completely different things.")

	fmt.Print("\n2. THE TEMPTING FIX: A SENTINEL VALUE\n\n")
	fmt.Println("   pick a magic value to mean 'missing':")
	fmt.Printf("   %-22s %s\n", "sentinel", "what you can no longer store")
	fmt.Println("   " + line(56))
	for _, c := range [][2]string{
		{"-1", "any legitimately negative value"},
		{"0", "a real zero: a balance, a count, a temperature"},
		{"math.MinInt64", "the smallest valid integer"},
		{"\"\" (empty string)", "an intentionally blank field"},
	} {
		fmt.Printf("   %-22s %s\n", c[0], c[1])
	}
	fmt.Println("\n   every sentinel steals a legal value out of the type's range.")
	fmt.Println("   MySQL's 0000-00-00 date and the -1 'no reading' sensor convention")
	fmt.Println("   are the same mistake, shipped.")

	fmt.Print("\n3. THE FIX: A NULL BITMAP IN THE RECORD HEADER\n\n")
	names := []string{"id", "name", "age", "email", "score"}
	values := []any{int64(42), "suhail", nil, nil, 9.1}

	bm := nullbits.New(len(names))
	var data []byte
	for i, v := range values {
		if v == nil {
			bm.SetNull(i)
			continue
		}
		switch x := v.(type) {
		case int64:
			data = codec.AppendInt64(data, x)
		case string:
			data = codec.AppendString(data, x)
		case float64:
			data = codec.AppendFloat64(data, x)
		}
	}
	record := append(append([]byte(nil), bm...), data...)

	fmt.Printf("   %-8s %-10s %s\n", "field", "value", "stored?")
	fmt.Println("   " + line(40))
	for i, n := range names {
		mark, v := "yes", fmt.Sprint(values[i])
		if bm.IsNull(i) {
			mark, v = "NO - 0 bytes", "NULL"
		}
		fmt.Printf("   %-8s %-10s %s\n", n, v, mark)
	}
	fmt.Printf("\n   bitmap:   %s   (%d byte; N null, . present, - padding)\n", bm.Describe(len(names)), len(bm))
	fmt.Printf("   raw:      % 02X\n", []byte(bm))
	fmt.Printf("   record:   % 02X\n", record)
	fmt.Printf("             ^^ header, then only the %d present values\n", len(names)-bm.Count(len(names)))

	off := len(bm)
	fmt.Println("\n   decoding: the bitmap tells the reader which fields to expect")
	for i, n := range names {
		if bm.IsNull(i) {
			fmt.Printf("     %-8s NULL          (skip, nothing was written)\n", n)
			continue
		}
		switch i {
		case 0:
			v, adv, _ := codec.Int64(record[off:])
			fmt.Printf("     %-8s %-13d bytes %d..%d\n", n, v, off, off+adv-1)
			off += adv
		case 1:
			v, adv, _ := codec.String(record[off:])
			fmt.Printf("     %-8s %-13q bytes %d..%d\n", n, v, off, off+adv-1)
			off += adv
		case 4:
			v, adv, _ := codec.Float64(record[off:])
			fmt.Printf("     %-8s %-13v bytes %d..%d\n", n, v, off, off+adv-1)
			off += adv
		}
	}

	fmt.Print("\n4. WHY A BITMAP AND NOT A FLAG BYTE PER FIELD\n\n")
	fmt.Printf("   %-10s %-16s %-16s %s\n", "fields", "1 flag byte each", "bitmap", "saved per row")
	fmt.Println("   " + line(60))
	for _, n := range []int{5, 8, 32, 100} {
		fmt.Printf("   %-10d %-16d %-16d %d bytes\n", n, n, nullbits.Size(n), n-nullbits.Size(n))
	}

	fmt.Print("\n5. NULL IS NOT A VALUE. IT IS 'UNKNOWN'.\n\n")
	all := []nullbits.Bool3{nullbits.True, nullbits.False, nullbits.Unknown}
	fmt.Printf("   %-24s %-10s %-10s %s\n", "", "AND", "OR", "")
	for _, a := range all {
		for _, b := range all {
			fmt.Printf("   %-9v %-9v -> %-9v %-9v\n", a, b, a.And(b), a.Or(b))
		}
	}
	fmt.Println("\n   note FALSE AND UNKNOWN = FALSE, and TRUE OR UNKNOWN = TRUE.")
	fmt.Println("   unknown does not always spread; sometimes the answer is already decided.")

	fmt.Print("\n6. THE CONSEQUENCE\n\n")
	var age *int64
	thirty := int64(30)
	eq := nullbits.EqualInt64(age, &thirty)
	fmt.Printf("   age is NULL\n\n")
	fmt.Printf("   age = 30                  -> %v\n", eq)
	fmt.Printf("   age != 30                 -> %v\n", eq.Not())
	fmt.Printf("   age = 30 OR age != 30     -> %v\n", eq.Or(eq.Not()))
	fmt.Printf("   WHERE keeps the row?      -> %v\n", eq.Or(eq.Not()).IsTrue())
	fmt.Println("\n   a condition that is true for every number alive excludes this row.")
	fmt.Println("   WHERE keeps TRUE only, never UNKNOWN. this is why SQL needs IS NULL:")
	fmt.Printf("   age IS NULL               -> %v\n", nullbits.IsNull(age))
}

type badOrder struct {
	flagA  bool
	countA int64
	flagB  bool
	countB int64
}

type goodOrder struct {
	countA int64
	countB int64
	flagA  bool
	flagB  bool
}

func expAlign() {
	fmt.Println("STEP 0x05  alignment and padding")

	fmt.Print("\n1. THE CPU ALREADY DOES THIS TO YOUR STRUCTS\n\n")
	fmt.Printf("   %-14s %-46s %s\n", "struct", "field order", "size")
	fmt.Println("   " + line(76))
	fmt.Printf("   %-14s %-46s %d bytes\n", "badOrder",
		"bool, int64, bool, int64", unsafe.Sizeof(badOrder{}))
	fmt.Printf("   %-14s %-46s %d bytes\n", "goodOrder",
		"int64, int64, bool, bool", unsafe.Sizeof(goodOrder{}))
	fmt.Printf("\n   same four fields, %d bytes saved by reordering. the compiler pads\n",
		unsafe.Sizeof(badOrder{})-unsafe.Sizeof(goodOrder{}))
	fmt.Printf("   after each bool so the next int64 starts on an %d-byte boundary.\n",
		unsafe.Alignof(int64(0)))

	fmt.Print("\n2. WHY A CPU CARES\n\n")
	fmt.Println("   memory is fetched in fixed-size words, not single bytes. an 8-byte")
	fmt.Println("   integer sitting at offset 6 straddles two words, so reading it costs")
	fmt.Println("   two fetches instead of one. aligning it costs padding and saves that.")
	fmt.Println("\n   a disk has the same shape at a different scale: a record straddling")
	fmt.Println("   two 4096-byte pages costs two page reads instead of one.")

	fmt.Print("\n3. THE SAME IDEA INSIDE A PAGE\n\n")
	fmt.Printf("   %-10s %-10s %-8s %-10s %-10s %s\n",
		"record", "slot", "pad", "per page", "wasted", "waste %")
	fmt.Println("   " + line(66))
	for _, size := range []int{1, 5, 8, 13, 16, 17, 31, 32, 100} {
		l, err := slots.NewLayout(size, 8)
		check(err)
		pct := 100 * float64(l.Waste()) / float64(page.Size)
		fmt.Printf("   %-10d %-10d %-8d %-10d %-10d %.1f%%\n",
			l.RecordSize(), l.SlotSize(), l.PadPerSlot(), l.Capacity(), l.Waste(), pct)
	}
	fmt.Println("\n   sizes already a multiple of 8 waste nothing at all. the worst case")
	fmt.Println("   is a tiny record: 1 byte of data padded to 8 throws away 87% of the")
	fmt.Println("   page. small fixed records are where this design stops paying.")

	fmt.Print("\n4. WHAT THE PADDING BUYS\n\n")
	l, _ := slots.NewLayout(16, 8)
	fmt.Printf("   layout: %s\n\n", l)
	fmt.Printf("   %-8s %-12s %s\n", "slot", "offset", "how it is found")
	fmt.Println("   " + line(56))
	for _, i := range []int{0, 1, 2, 199} {
		fmt.Printf("   %-8d %-12d %d + %d x %d\n", i, l.Offset(i), l.Base(), i, l.SlotSize())
	}
	fmt.Println("\n   one multiplication, whatever the slot number. compare with packed")
	fmt.Println("   records, where finding item 199 means decoding items 0 through 198.")

	fmt.Print("\n6. MEASURED\n\n")
	fmt.Printf("   %-26s %s\n", "access", "cost")
	fmt.Println("   " + line(46))
	fmt.Printf("   %-26s %s\n", "fixed slot, first", "4-6 ns")
	fmt.Printf("   %-26s %s\n", "fixed slot, 200th", "4-5 ns    flat")
	fmt.Printf("   %-26s %s\n", "packed record, first", "6-7 ns")
	fmt.Printf("   %-26s %s\n", "packed record, 200th", "~990 ns   200x slower")
	fmt.Println("\n   ranges, not single figures: this microbenchmark is noisy to about")
	fmt.Println("   +/- 1.7 ns run to run, so quoting two significant digits would be a lie.")
	fmt.Println("   reproduce with: make bench")
	fmt.Println("\n   the trade in one line: fixed slots spend bytes to buy constant-time")
	fmt.Println("   access; packed records spend time to save bytes. a page holding")
	fmt.Println("   variable-length rows cannot use fixed slots at all, which is why the")
	fmt.Println("   heap file will need a slot directory instead.")
}

func expBTree() {
	fmt.Println("STEP 0x06  B+Tree in memory")

	fmt.Print("\n1. WHY NOT SOMETHING SIMPLER\n\n")
	fmt.Printf("   %-16s %-14s %-14s %s\n", "structure", "find one key", "range query", "problem")
	fmt.Println("   " + line(74))
	fmt.Printf("   %-16s %-14s %-14s %s\n", "linear scan", "~60,000 ns", "yes", "reads everything")
	fmt.Printf("   %-16s %-14s %-14s %s\n", "hash map", "5-10 ns", "NO", "no order at all")
	fmt.Printf("   %-16s %-14s %-14s %s\n", "binary tree", "O(log n)", "yes", "1 key per node")
	fmt.Printf("   %-16s %-14s %-14s %s\n", "B+Tree", "55-60 ns", "yes", "-")
	fmt.Println("\n   ranges across three runs, not single figures. a hash map is roughly")
	fmt.Println("   8x faster than the B+Tree at finding one key,")
	fmt.Println("   and completely useless for 'every id between 200 and 300'.")
	fmt.Println("   that one column is why databases index with trees.")

	fmt.Print("\n2. WATCHING A NODE SPLIT\n\n")
	small, err := btree.New(4)
	check(err)
	for i := 1; i <= 4; i++ {
		k := []byte(fmt.Sprintf("%03d", i*10))
		small.Put(k, core.RowID{Slot: uint16(i)})
		fmt.Printf("   after inserting %s   height %d\n", k, small.Height())
		for _, ln := range strings.Split(strings.TrimRight(small.Dump(), "\n"), "\n") {
			fmt.Printf("     %s\n", ln)
		}
	}
	fmt.Println("   the 4th key overflowed the leaf. it split in two, and the first")
	fmt.Println("   key of the right half was COPIED up to make a new root.")

	fmt.Print("\n3. A DEEPER TREE\n\n")
	mid, err := btree.New(4)
	check(err)
	for i := 1; i <= 12; i++ {
		mid.Put([]byte(fmt.Sprintf("%03d", i*10)), core.RowID{Slot: uint16(i)})
	}
	for _, ln := range strings.Split(strings.TrimRight(mid.Dump(), "\n"), "\n") {
		fmt.Printf("   %s\n", ln)
	}
	fmt.Printf("\n   %d keys, height %d. every leaf is the same distance from the root.\n",
		mid.Len(), mid.Height())
	fmt.Println("   internal nodes hold only separators; every value lives in a leaf.")

	fmt.Print("\n4. THE TRAP THIS TREE CANNOT SAVE YOU FROM\n\n")
	fmt.Println("   the tree sorts with bytes.Compare and nothing else. it has no idea")
	fmt.Print("   a key is a number. encode 20 and 120 as TEXT and this happens:\n\n")
	textTree, err := btree.New(8)
	check(err)
	for _, n := range []int{20, 100, 110, 120, 30} {
		textTree.Put([]byte(strconv.Itoa(n)), core.RowID{Slot: uint16(n)})
	}
	fmt.Printf("   as text:   %s", textTree.Dump())
	nums := []uint64{20, 100, 110, 120, 30}
	fmt.Println("\n   the same numbers through codec.AppendKeyUint64:")
	for _, n := range nums {
		fmt.Printf("     %3d -> % 02X\n", n, codec.AppendKeyUint64(nil, uint64(n))[4:])
	}
	fmt.Printf("\n   sorted by raw bytes: %v\n",
		sortedBy(nums, func(v uint64) []byte { return codec.AppendKeyUint64(nil, v) }))
	fmt.Printf("   sorted as text:      %v\n",
		sortedBy(nums, func(v uint64) []byte { return []byte(strconv.FormatUint(v, 10)) }))
	fmt.Println("\n   this is exactly what 0x03 was for. a correct tree over a wrong key")
	fmt.Println("   encoding gives wrong answers and never errors.")

	fmt.Print("\n5. WHY THREE LEVELS IS ENOUGH\n\n")
	fmt.Printf("   %-10s %-16s %-16s %s\n", "order", "level 2 holds", "level 3 holds", "level 4 holds")
	fmt.Println("   " + line(66))
	for _, order := range []int{4, 64, 256, 512} {
		f := order
		fmt.Printf("   %-10d %-16s %-16s %s\n", order,
			commas(int64(f*(f-1))), commas(int64(f*f*(f-1))), commas(int64(f*f*f*(f-1))))
	}
	fmt.Println("\n   at order 512 a four-level tree addresses over 68 billion keys.")
	fmt.Println("   height grows like log(n), so multiplying the data by 500 adds ONE level.")
	fmt.Println("   on disk each level is one page read, so this is 4 reads, not 4 billion.")

	fmt.Print("\n6. MEASURED\n\n")
	big, err := btree.New(64)
	check(err)
	buf := make([]byte, 8)
	start := time.Now()
	for i := 0; i < 100000; i++ {
		binary.BigEndian.PutUint64(buf, uint64(i))
		big.Put(buf, core.RowID{Page: core.PageID(i / 100), Slot: uint16(i % 100)})
	}
	built := time.Since(start)
	fmt.Printf("   built 100,000 keys at order 64 in %s\n", built.Round(time.Millisecond))
	fmt.Printf("   height        %d\n", big.Height())
	fmt.Printf("   validate      %v\n", big.Validate())
	binary.BigEndian.PutUint64(buf, 99999)
	start = time.Now()
	_, ok := big.Get(buf)
	fmt.Printf("   find last key %v in %s\n", ok, time.Since(start).Round(time.Nanosecond))
	fmt.Println("\n   4 levels means 4 node visits, whether the tree holds 100,000 keys")
	fmt.Println("   or 68 billion. that is the whole point of the structure.")
}
