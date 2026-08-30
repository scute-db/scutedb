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
	"sync"
	"time"

	"github.com/scute-db/scutedb/internal/codec"
	"github.com/scute-db/scutedb/internal/core"
	"github.com/scute-db/scutedb/internal/naive"
	"github.com/scute-db/scutedb/internal/page"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Println("usage: scutedb-demo scan|update|race|crash|pages|header|encode")
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
