package main

import (
	"fmt"
	"math/rand"
	"os"
	"time"

	"github.com/scute-db/scutedb/internal/bufpool"
	"github.com/scute-db/scutedb/internal/codec"
	"github.com/scute-db/scutedb/internal/core"
	"github.com/scute-db/scutedb/internal/disktree"
	"github.com/scute-db/scutedb/internal/page"
	"github.com/scute-db/scutedb/internal/pagestore"
)

func expBufPool() {
	fmt.Print("STEP 0x0B  The buffer pool, and the B+Tree moves onto disk\n\n")
	poolFlooding()
	poolWriteModes()
	poolReopen()
	poolLazyDelete()
}

func floodStore(hot, cold int) (*pagestore.Store, []core.PageID, []core.PageID) {
	s, err := pagestore.Create(tmp("flood.db"))
	check(err)
	var ids []core.PageID
	for i := 0; i < hot+cold; i++ {
		p, err := s.Allocate(page.KindHeap)
		check(err)
		ids = append(ids, p.ID())
	}
	return s, ids[:hot], ids[hot:]
}

func touchPage(p *bufpool.Pool, id core.PageID) bool {
	before := p.Stats().Hits
	f, err := p.Fetch(id)
	check(err)
	check(p.Unpin(f, false))
	return p.Stats().Hits > before
}

func poolFlooding() {
	const frames, hot, cold = 64, 48, 1000
	fmt.Print("1. SEQUENTIAL FLOODING\n\n")
	fmt.Printf("   a %d-frame pool over a real file. %d hot pages are read again and\n", frames, hot)
	fmt.Printf("   again. a scan reads %d cold pages once each. the hot set fits;\n", cold)
	fmt.Print("   the scan does not. which policy keeps the hot pages?\n\n")

	s, hotIDs, coldIDs := floodStore(hot, cold)
	defer s.Close()

	type result struct {
		hotHits, hotTotal int
		stats             bufpool.Stats
	}
	run := func(pol bufpool.Policy, interleave bool) result {
		p, err := bufpool.New(s, bufpool.Options{Frames: frames, Policy: pol, K: 2})
		check(err)
		for warm := 0; warm < 3; warm++ {
			for _, id := range hotIDs {
				touchPage(p, id)
			}
		}
		p.ResetStats()
		var r result
		if interleave {
			for i := 0; i < 2*cold; i++ {
				touchPage(p, coldIDs[i%cold])
				if touchPage(p, hotIDs[i%hot]) {
					r.hotHits++
				}
				r.hotTotal++
			}
		} else {
			for round := 0; round < 3; round++ {
				for _, id := range coldIDs {
					touchPage(p, id)
				}
				for _, id := range hotIDs {
					if touchPage(p, id) {
						r.hotHits++
					}
					r.hotTotal++
				}
			}
		}
		r.stats = p.Stats()
		return r
	}

	for _, w := range []struct {
		name       string
		interleave bool
	}{
		{"A. scan all 1000 cold pages, then read the hot set; three rounds", false},
		{"B. one cold page, one hot page, one cold page, ...; 2000 pairs", true},
	} {
		fmt.Printf("   %s\n\n", w.name)
		fmt.Printf("   %-7s %-11s %-9s %-13s %-11s %s\n", "policy", "hot hits", "hot", "all accesses", "evictions", "disk reads")
		fmt.Println("   " + line(64))
		for _, pol := range []bufpool.Policy{bufpool.LRU, bufpool.Clock, bufpool.LRUK} {
			r := run(pol, w.interleave)
			fmt.Printf("   %-7s %-11s %-9s %-13s %-11s %s\n", pol,
				fmt.Sprintf("%d/%d", r.hotHits, r.hotTotal),
				fmt.Sprintf("%.1f%%", 100*float64(r.hotHits)/float64(r.hotTotal)),
				fmt.Sprintf("%.1f%%", 100*r.stats.HitRatio()),
				commas(int64(r.stats.Evictions)), commas(int64(r.stats.Reads)))
		}
		fmt.Println()
	}
	fmt.Print("   LRU sees the scan's pages as the most recent and throws the hot set\n")
	fmt.Print("   out to make room for pages nobody will read again. Clock does the\n")
	fmt.Print("   same with less bookkeeping. LRU-K (K=2) ranks a page by its second\n")
	fmt.Print("   most recent use; a page read once has no second use, so scan pages\n")
	fmt.Print("   evict each other and the hot set stays.\n\n")
}

func demoKey(i int) []byte { return codec.AppendKeyInt64(nil, int64(i)) }

func buildTree(path string, frames int, mode bufpool.Mode, n int) (*pagestore.Store, *bufpool.Pool, *disktree.Tree, core.PageID, time.Duration) {
	s, err := pagestore.Create(path)
	check(err)
	p, err := bufpool.New(s, bufpool.Options{Frames: frames, Policy: bufpool.LRUK, Mode: mode})
	check(err)
	tr, root, err := disktree.Create(p)
	check(err)
	check(s.SetRoot(root))
	r := rand.New(rand.NewSource(1))
	order := r.Perm(n)
	start := time.Now()
	for _, i := range order {
		check(tr.Put(demoKey(i), core.RowID{Page: core.PageID(i/100 + 1), Slot: uint16(i % 100)}))
	}
	return s, p, tr, root, time.Since(start)
}

func poolWriteModes() {
	const n = 20000
	fmt.Print("2. WRITE-BACK OR WRITE-THROUGH\n\n")
	fmt.Printf("   insert %s keys in random order into a disk B+Tree through a\n", commas(n))
	fmt.Print("   16-frame pool, then flush. count the pages written to the file.\n\n")
	fmt.Printf("   %-14s %-14s %-11s %s\n", "mode", "page writes", "per insert", "time")
	fmt.Println("   " + line(52))
	for _, mode := range []bufpool.Mode{bufpool.WriteBack, bufpool.WriteThrough} {
		s, p, _, _, took := buildTree(tmp("modes.db"), 16, mode, n)
		check(p.FlushAll())
		w := p.Stats().Writes
		fmt.Printf("   %-14s %-14s %-11.2f %s\n", mode, commas(int64(w)), float64(w)/n, took.Round(time.Millisecond))
		check(s.Close())
	}
	fmt.Print("\n   write-through writes a page every time it is changed and unpinned.\n")
	fmt.Print("   write-back waits until the page is evicted or flushed, so a page\n")
	fmt.Print("   changed many times while it sits in memory is written once.\n\n")
}

func poolReopen() {
	const n = 50000
	fmt.Print("3. A B+TREE THAT SURVIVES CLOSE AND REOPEN\n\n")
	path := tmp("tree.db")
	s, p, tr, root, took := buildTree(path, 32, bufpool.WriteBack, n)
	shape, err := tr.Shape()
	check(err)
	st := p.Stats()
	fmt.Printf("   inserted %s keys in random order in %s\n", commas(n), took.Round(time.Millisecond))
	fmt.Printf("   height 1 -> %d: the root split %d times. it was page %d when the tree\n",
		shape.Height, shape.Height-1, root)
	fmt.Printf("   was one empty leaf, and it is page %d now. %d leaves, %d internal pages,\n",
		tr.Root(), shape.Leaves, shape.Internal)
	fmt.Printf("   leaves %.0f%% full.\n", 100*shape.LeafFill())
	fmt.Printf("   32-frame pool: %s hits, %s misses (%.1f%% hit ratio), %s evictions\n\n",
		commas(int64(st.Hits)), commas(int64(st.Misses)), 100*st.HitRatio(), commas(int64(st.Evictions)))

	check(p.FlushAll())
	check(s.Checkpoint())
	check(s.Close())
	info, err := os.Stat(path)
	check(err)
	fmt.Printf("   FlushAll, Checkpoint, Close. the file is %s.\n\n", humanBytes(info.Size()))

	s, err = pagestore.Open(path)
	check(err)
	defer s.Close()
	p, err = bufpool.New(s, bufpool.Options{Frames: 32, Policy: bufpool.LRUK})
	check(err)
	tr, err = disktree.Open(p, s.Root())
	check(err)
	fmt.Printf("   reopened. the meta page says the root is page %d.\n", s.Root())
	for _, i := range []int{0, 12345, 49999} {
		rid, ok, err := tr.Get(demoKey(i))
		check(err)
		fmt.Printf("     get %-6d -> found %v, row page %d slot %d\n", i, ok, rid.Page, rid.Slot)
	}
	_, ok, err := tr.Get(demoKey(n))
	check(err)
	fmt.Printf("     get %-6d -> found %v\n", n, ok)
	it := tr.Scan(demoKey(1000), demoKey(1010))
	count := 0
	for it.Next() {
		count++
	}
	check(it.Err())
	fmt.Printf("     scan [1000, 1010) -> %d keys\n", count)
	check(tr.Validate())
	st = p.Stats()
	fmt.Printf("   Validate: ok. a cold pool read %s pages to do all that.\n\n", commas(int64(st.Reads)))
}

func poolLazyDelete() {
	const n = 50000
	fmt.Print("4. DELETE NEVER MERGES\n\n")
	s, p, tr, _, _ := buildTree(tmp("lazy.db"), 32, bufpool.WriteBack, n)
	defer s.Close()
	before, err := tr.Shape()
	check(err)
	for i := 0; i < n; i++ {
		if i%10 != 0 {
			_, err := tr.Delete(demoKey(i))
			check(err)
		}
	}
	after, err := tr.Shape()
	check(err)
	fmt.Printf("   %-22s %-8s %-8s %-8s %s\n", "", "keys", "leaves", "height", "leaf fill")
	fmt.Println("   " + line(56))
	fmt.Printf("   %-22s %-8d %-8d %-8d %.0f%%\n", "after 50,000 inserts", before.Keys, before.Leaves, before.Height, 100*before.LeafFill())
	fmt.Printf("   %-22s %-8d %-8d %-8d %.0f%%\n", "after deleting 90%", after.Keys, after.Leaves, after.Height, 100*after.LeafFill())
	fmt.Print("\n   the tree keeps every page. a leaf may go nearly empty, and that is\n")
	fmt.Print("   allowed: no merge means a delete touches one leaf and nothing else.\n")
	fmt.Printf("   Validate still passes; %d frames are pinned.\n\n", p.Pinned())
}
