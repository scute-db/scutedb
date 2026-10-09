package main

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/scute-db/scutedb/internal/core"
	"github.com/scute-db/scutedb/internal/page"
	"github.com/scute-db/scutedb/internal/pagestore"
)

func expPageStore() {
	fmt.Print("STEP 0x0A  The index storage manager\n\n")
	storeMagic()
	storeMeta()
	storeChunks()
	storeFreeList()
	storeDurability()
	storeRestarts()
}

func storeMagic() {
	fmt.Print("1. WHICH FILE IS THIS?\n\n")
	fmt.Print("   the first thing a reader needs is proof the file is ours,\n")
	fmt.Print("   in a format this build understands.\n\n")

	path := tmp("magic.db")
	s, err := pagestore.Create(path)
	check(err)
	check(s.Close())

	raw, err := os.ReadFile(path)
	check(err)
	fmt.Printf("   bytes 16..23 of a new file:  % 02X   %q\n\n", raw[16:24], string(raw[16:23]))

	junk := tmp("poem.txt")
	check(os.WriteFile(junk, []byte(strings.Repeat("roses are red, pages are 4096\n", 300)), 0o644))
	_, err = pagestore.Open(junk)
	fmt.Printf("   open a text file          -> %s\n", verdict(err))

	future := tmp("future.db")
	bumped := append([]byte(nil), raw...)
	binary.BigEndian.PutUint32(bumped[24:], pagestore.Version+1)
	check(os.WriteFile(future, bumped, 0o644))
	_, err = pagestore.Open(future)
	fmt.Printf("   open a version-%d file     -> %s\n", pagestore.Version+1, verdict(err))

	big := tmp("bigpages.db")
	resized := append([]byte(nil), raw...)
	binary.BigEndian.PutUint32(resized[28:], 8192)
	check(os.WriteFile(big, resized, 0o644))
	_, err = pagestore.Open(big)
	fmt.Printf("   open 8192-byte pages      -> %s\n\n", verdict(err))

	fmt.Print("   refusing is the point. reading an 8 KB-page file as 4 KB pages\n")
	fmt.Print("   would not crash. it would quietly return wrong rows.\n\n")
}

func verdict(err error) string {
	switch {
	case err == nil:
		return "ACCEPTED"
	case errors.Is(err, pagestore.ErrNotScuteDB):
		return "rejected, not a scutedb file"
	case errors.Is(err, pagestore.ErrBadVersion):
		return "rejected, unknown format version"
	case errors.Is(err, pagestore.ErrBadPageSize):
		return "rejected, wrong page size"
	}
	return "rejected: " + err.Error()
}

func storeMeta() {
	fmt.Print("2. THE META PAGE: THE ONE FIXED ADDRESS\n\n")
	fmt.Print("   after a restart nothing is in memory. page 0 is the only place\n")
	fmt.Print("   a reader can look without being told where to look.\n\n")

	path := tmp("meta.db")
	s, err := pagestore.Create(path)
	check(err)
	root, err := s.Allocate(page.KindBTreeLeaf)
	check(err)
	check(s.SetRoot(root.ID()))
	check(s.Checkpoint())
	out, err := s.DescribeMeta()
	check(err)
	fmt.Print(indent(out, "   "))
	check(s.Close())
}

func storeChunks() {
	fmt.Print("3. GROWING THE FILE IN CHUNKS\n\n")
	path := tmp("chunks.db")
	s, err := pagestore.Create(path)
	check(err)

	fmt.Printf("   %-12s %-16s %s\n", "handed out", "file size", "grown")
	fmt.Println("   " + strings.Repeat("-", 40))
	report := func() {
		check(s.Sync())
		info, err := os.Stat(path)
		check(err)
		fmt.Printf("   %-12d %-16s %d times\n", s.Meta().NextPage-1, humanBytes(info.Size()), s.Stats().Grown)
	}
	report()
	for _, target := range []int{15, 16, 31, 32, 40} {
		for int(s.Meta().NextPage)-1 < target {
			_, err := s.Allocate(page.KindHeap)
			check(err)
		}
		report()
	}
	check(s.Close())
	fmt.Printf("\n   40 pages, but the file grew 3 times, not 40. each growth adds\n")
	fmt.Printf("   %d pages (%s) in one write, so the filesystem can lay them out\n", pagestore.ChunkPages, humanBytes(pagestore.ChunkPages*page.Size))
	fmt.Print("   contiguously and the metadata update is paid once per chunk.\n")
	fmt.Print("   'handed out' and 'file size' are different numbers, so both live\n")
	fmt.Print("   in the meta page. after a restart the next page is 41, not 48.\n\n")
}

func storeFreeList() {
	fmt.Print("4. FREE LISTS, AND WHY A FREED PAGE HAS TO WAIT\n\n")
	path := tmp("free.db")
	s, err := pagestore.Create(path)
	check(err)
	var ids []core.PageID
	for i := 0; i < 6; i++ {
		p, err := s.Allocate(page.KindHeap)
		check(err)
		ids = append(ids, p.ID())
	}
	check(s.SetRoot(ids[0]))
	check(s.Checkpoint())

	for _, id := range ids[2:5] {
		check(s.Free(id))
	}
	fmt.Printf("   free pages %d, %d and %d\n", ids[2], ids[3], ids[4])
	fmt.Print(indent(s.DescribeFreeList(), "     "))

	p, err := s.Allocate(page.KindHeap)
	check(err)
	fmt.Printf("   allocate -> page %d. a NEW page, not one just freed.\n\n", p.ID())
	fmt.Print("   the last checkpoint on disk still says those three pages are in\n")
	fmt.Print("   use. if one were overwritten now and the process died, restart\n")
	fmt.Print("   would find the committed tree pointing at someone else's bytes.\n\n")

	check(s.Checkpoint())
	fmt.Print("   checkpoint\n")
	fmt.Print(indent(s.DescribeFreeList(), "     "))
	q, err := s.Allocate(page.KindHeap)
	check(err)
	fmt.Printf("   allocate -> page %d. reused, and the file did not grow.\n\n", q.ID())
	fmt.Print("   the free list lives in its own page, not threaded through the\n")
	fmt.Print("   free pages, so reusing a free page cannot damage the list that\n")
	fmt.Print("   describes the last checkpoint.\n\n")
	check(s.Close())
}

func storeDurability() {
	fmt.Print("5. DATA DURABILITY IS NOT STRUCTURAL DURABILITY\n\n")
	fmt.Print("   data durability:        the bytes of a page reached the disk.\n")
	fmt.Print("   structural durability:  the file, as a whole, describes a state\n")
	fmt.Print("                           that makes sense.\n\n")
	fmt.Print("   a checkpoint turns the first into the second, in this order:\n\n")
	fmt.Print("     1. fsync            every page written since the last one\n")
	fmt.Print("     2. write            the new free list into its own pages\n")
	fmt.Print("     3. fsync            so the list is on disk\n")
	fmt.Print("     4. write page 0     the new root, list, and page counts\n")
	fmt.Print("     5. fsync            the commit point\n\n")
	fmt.Print("   the meta page is written LAST. if the machine dies at any step\n")
	fmt.Print("   before 5, page 0 still describes the previous checkpoint, and\n")
	fmt.Print("   nothing that checkpoint points at has been touched.\n\n")

	path := tmp("durable.db")
	s, err := pagestore.Create(path)
	check(err)
	first, err := s.Allocate(page.KindHeap)
	check(err)
	copy(first[page.HeaderSize:], "committed")
	check(s.Write(first))
	check(s.SetRoot(first.ID()))
	check(s.Checkpoint())

	second, err := s.Allocate(page.KindHeap)
	check(err)
	copy(second[page.HeaderSize:], "uncommitted")
	check(s.Write(second))
	check(s.SetRoot(second.ID()))
	check(s.Free(first.ID()))
	check(s.Sync())
	fmt.Printf("   root is page %d, checkpointed. then: new root page %d, old root\n", first.ID(), second.ID())
	fmt.Print("   freed, and every page fsynced. data durable. NOT checkpointed.\n")
	check(s.Discard())

	back, err := pagestore.Open(path)
	check(err)
	got, err := back.Read(back.Root())
	check(err)
	fmt.Printf("   the process dies. reopen: root is page %d, holding %q\n\n",
		back.Root(), strings.TrimRight(string(got[page.HeaderSize:page.HeaderSize+11]), "\x00"))
	fmt.Print("   the second page's bytes are on disk. it does not matter: page 0\n")
	fmt.Print("   never learned about it, so the file still describes the old\n")
	fmt.Print("   tree, and freeing the old root did not touch its bytes.\n\n")
	check(back.Close())
}

func storeRestarts() {
	fmt.Print("6. RESTART AS A TEST: KILL -9, TWENTY TIMES\n\n")
	fmt.Print("   a child process loops: new root page, stamp it with the next\n")
	fmt.Print("   checkpoint number, free the old root, checkpoint, report. the\n")
	fmt.Print("   parent SIGKILLs it at an arbitrary moment and reopens the file.\n\n")

	self, err := os.Executable()
	check(err)
	fmt.Printf("   %-5s %-12s %-12s %-10s %s\n", "run", "child said", "file says", "root stamp", "verify")
	fmt.Println("   " + strings.Repeat("-", 58))

	clean := 0
	for run := 1; run <= 20; run++ {
		path := tmp("restart.db")
		cmd := exec.Command(self, "store-child", path)
		stdout, err := cmd.StdoutPipe()
		check(err)
		check(cmd.Start())

		last := uint32(0)
		done := make(chan struct{})
		go func() {
			sc := bufio.NewScanner(stdout)
			for sc.Scan() {
				if v, err := strconv.ParseUint(sc.Text(), 10, 32); err == nil {
					last = uint32(v)
				}
			}
			close(done)
		}()
		time.Sleep(time.Duration(40+run*13) * time.Millisecond)
		check(cmd.Process.Kill())
		cmd.Wait()
		<-done

		s, err := pagestore.Open(path)
		if errors.Is(err, fs.ErrNotExist) {
			fmt.Printf("   %-5d %-12d killed before it created the file; nothing to recover\n", run, last)
			clean++
			continue
		}
		if err != nil {
			fmt.Printf("   %-5d %-12d OPEN FAILED: %v\n", run, last, err)
			continue
		}
		gen := s.Durable().Checkpoint
		stamp := uint32(0)
		if s.Root() != pagestore.NoPage {
			p, err := s.Read(s.Root())
			check(err)
			stamp = binary.BigEndian.Uint32(p[page.HeaderSize:])
		}
		v := s.Verify()
		ok := v == nil && gen >= last && gen <= last+1 && (s.Root() == pagestore.NoPage || stamp == gen)
		status := "ok"
		if v != nil {
			status = v.Error()
		} else if !ok {
			status = "MISMATCH"
		} else {
			clean++
		}
		fmt.Printf("   %-5d %-12d %-12d %-10d %s\n", run, last, gen, stamp, status)
		check(s.Close())
	}

	fmt.Printf("\n   %d of 20 recovered to a consistent checkpoint.\n\n", clean)
	fmt.Print("   'file says' is sometimes one ahead of 'child said': the child\n")
	fmt.Print("   finished a checkpoint and died before it could print. that is\n")
	fmt.Print("   correct. the one thing that must never happen is the file being\n")
	fmt.Print("   BEHIND what the child was told was durable, or the root holding\n")
	fmt.Print("   a stamp from a different checkpoint than the one that set it.\n\n")
	fmt.Print("   a SIGKILL only kills the process; the OS still flushes what it\n")
	fmt.Print("   was given. a power cut is harsher, and cannot be staged from a\n")
	fmt.Print("   demo. the test suite covers it with a simulated disk that drops\n")
	fmt.Print("   any combination of unsynced writes.\n\n")
}

func storeChild(path string) {
	s, err := pagestore.Create(path)
	if err != nil {
		os.Exit(1)
	}
	out := bufio.NewWriter(os.Stdout)
	fmt.Fprintln(out, s.Durable().Checkpoint)
	out.Flush()
	for {
		p, err := s.Allocate(page.KindBTreeLeaf)
		if err != nil {
			os.Exit(1)
		}
		binary.BigEndian.PutUint32(p[page.HeaderSize:], s.Durable().Checkpoint+1)
		if err := s.Write(p); err != nil {
			os.Exit(1)
		}
		old := s.Root()
		if err := s.SetRoot(p.ID()); err != nil {
			os.Exit(1)
		}
		if old != pagestore.NoPage {
			if err := s.Free(old); err != nil {
				os.Exit(1)
			}
		}
		if err := s.Checkpoint(); err != nil {
			os.Exit(1)
		}
		fmt.Fprintln(out, s.Durable().Checkpoint)
		out.Flush()
	}
}
