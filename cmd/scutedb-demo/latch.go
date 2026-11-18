package main

import (
	"fmt"
	"runtime"
	"sync"
	"time"

	"github.com/scute-db/scutedb/internal/bufpool"
	"github.com/scute-db/scutedb/internal/core"
	"github.com/scute-db/scutedb/internal/latch"
	"github.com/scute-db/scutedb/internal/page"
)

func expLatch() {
	fmt.Print("STEP 0x0C  Concurrency: latches, lock order, and lock striping\n\n")
	latchDeadlock()
	latchOrdered()
	latchStriping()
}

func latchDeadlock() {
	const wait = 500 * time.Millisecond
	fmt.Print("1. TWO LOCKS, OPPOSITE ORDER\n\n")
	fmt.Print("   goroutine A takes lock 1, then wants lock 2.\n")
	fmt.Print("   goroutine B takes lock 2, then wants lock 1.\n\n")
	var one, two latch.Mutex
	holding := make(chan string, 2)
	proceed := make(chan struct{})
	cancel := make(chan struct{})
	result := make(chan string, 2)
	run := func(name string, first, second *latch.Mutex) {
		first.Lock()
		holding <- name
		<-proceed
		got := second.LockOrCancel(cancel)
		if got {
			second.Unlock()
		}
		first.Unlock()
		if got {
			result <- name + " got its second lock"
		} else {
			result <- name + " gave up and let go of its first lock"
		}
	}
	go run("A", &one, &two)
	go run("B", &two, &one)
	fmt.Printf("   %s holds its first lock\n", <-holding)
	fmt.Printf("   %s holds its first lock\n", <-holding)
	close(proceed)
	start := time.Now()
	select {
	case r := <-result:
		fmt.Printf("   unexpected: %s\n\n", r)
		return
	case <-time.After(wait):
	}
	fmt.Printf("   after %s neither has finished: each waits for the lock the other holds.\n", time.Since(start).Round(time.Millisecond))
	fmt.Print("   that is a deadlock. a plain sync.Mutex would wait forever; these locks\n")
	fmt.Print("   take a cancel channel, so the watchdog closes it.\n")
	close(cancel)
	fmt.Printf("   %s\n", <-result)
	fmt.Printf("   %s\n\n", <-result)
}

func latchOrdered() {
	const rounds = 200000
	fmt.Print("2. THE SAME TWO LOCKS THROUGH LockInOrder\n\n")
	var one, two latch.Mutex
	counter := 0
	start := time.Now()
	var wg sync.WaitGroup
	for g := 0; g < 2; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			set := []latch.Ordered{{ID: 1, Lock: &one}, {ID: 2, Lock: &two}}
			if g == 1 {
				set[0], set[1] = set[1], set[0]
			}
			for i := 0; i < rounds; i++ {
				unlock, err := latch.LockInOrder(set...)
				check(err)
				counter++
				unlock()
			}
		}(g)
	}
	wg.Wait()
	fmt.Printf("   A lists the locks as (1, 2), B as (2, 1). LockInOrder sorts by id, so\n")
	fmt.Printf("   both take lock 1 first. %s critical sections in %s, no deadlock.\n\n",
		commas(int64(counter)), time.Since(start).Round(time.Millisecond))
}

type entry struct {
	frame int
	pins  int
}

func tableRun(stripes, goroutines, total int) time.Duration {
	const pages = 4096
	locks := latch.NewStriped(stripes)
	tables := make([]map[uint64]*entry, locks.Stripes())
	for i := range tables {
		tables[i] = map[uint64]*entry{}
	}
	for id := uint64(0); id < pages; id++ {
		tables[locks.Index(id)][id] = &entry{frame: int(id)}
	}
	per := total / goroutines
	var wg sync.WaitGroup
	start := time.Now()
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			id := uint64(g * 977)
			for i := 0; i < per; i++ {
				id = (id*1103515245 + 12345) % pages
				s := locks.Index(id)
				mu := locks.At(s)
				mu.Lock()
				tables[s][id].pins++
				mu.Unlock()
			}
		}(g)
	}
	wg.Wait()
	return time.Since(start)
}

func poolRun(stripes, goroutines, total int) time.Duration {
	const pages = 2048
	src := bufpool.NewMemSource()
	var ids []core.PageID
	for i := 0; i < pages; i++ {
		p, err := src.Allocate(page.KindHeap)
		check(err)
		ids = append(ids, p.ID())
	}
	pool, err := bufpool.New(src, bufpool.Options{Frames: pages, Stripes: stripes})
	check(err)
	for _, id := range ids {
		f, err := pool.Fetch(id)
		check(err)
		check(pool.Unpin(f, false))
	}
	per := total / goroutines
	var wg sync.WaitGroup
	start := time.Now()
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			at := g * 977
			for i := 0; i < per; i++ {
				at = (at*1103515245 + 12345) % pages
				f, err := pool.Fetch(ids[at])
				check(err)
				check(pool.Unpin(f, false))
			}
		}(g)
	}
	wg.Wait()
	return time.Since(start)
}

func best(run func() time.Duration) time.Duration {
	b := run()
	for i := 0; i < 2; i++ {
		if d := run(); d < b {
			b = d
		}
	}
	return b
}

func latchStriping() {
	const tableOps, poolOps = 4000000, 1000000
	fmt.Print("3. LOCK STRIPING: ONE MUTEX VS 64 STRIPES\n\n")
	fmt.Printf("   this machine: %d CPUs, GOMAXPROCS %d. each cell is the best of 3 runs.\n\n", runtime.NumCPU(), runtime.GOMAXPROCS(0))
	fmt.Printf("   a page table alone: %s lookups that bump a pin count, split\n", commas(tableOps))
	fmt.Print("   evenly over the goroutines, millions of operations per second\n\n")
	fmt.Printf("   %-12s %12s %12s %8s\n", "goroutines", "1 mutex", "64 stripes", "ratio")
	fmt.Println("   " + line(48))
	for _, g := range []int{1, 2, 4, 8} {
		one := best(func() time.Duration { return tableRun(1, g, tableOps) })
		many := best(func() time.Duration { return tableRun(64, g, tableOps) })
		fmt.Printf("   %-12d %12.1f %12.1f %7.1fx\n", g, mops(tableOps, one), mops(tableOps, many), one.Seconds()/many.Seconds())
	}
	fmt.Printf("\n   the whole pool: %s Fetch+Unpin hits on 2,048 resident pages,\n", commas(poolOps))
	fmt.Print("   millions per second\n\n")
	fmt.Printf("   %-12s %12s %12s %8s\n", "goroutines", "1 stripe", "64 stripes", "ratio")
	fmt.Println("   " + line(48))
	for _, g := range []int{1, 2, 4, 8} {
		one := best(func() time.Duration { return poolRun(1, g, poolOps) })
		many := best(func() time.Duration { return poolRun(64, g, poolOps) })
		fmt.Printf("   %-12d %12.2f %12.2f %7.1fx\n", g, mops(poolOps, one), mops(poolOps, many), one.Seconds()/many.Seconds())
	}
	fmt.Print("\n   every Fetch and Unpin also takes the pool's one replacer mutex (LRU\n")
	fmt.Print("   order and pin counts), so striping the page table alone moves the\n")
	fmt.Print("   queue to that mutex. Postgres avoids it with clock-sweep and atomic\n")
	fmt.Print("   pin counts in each buffer header.\n\n")
}

func mops(ops int, d time.Duration) float64 { return float64(ops) / d.Seconds() / 1e6 }
