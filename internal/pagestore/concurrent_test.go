package pagestore

import (
	"encoding/binary"
	"errors"
	"path/filepath"
	"sync"
	"testing"

	"github.com/scute-db/scutedb/internal/core"
	"github.com/scute-db/scutedb/internal/page"
)

func stampPage(p page.Page, g, i int) {
	binary.BigEndian.PutUint32(p[page.HeaderSize:], uint32(g))
	binary.BigEndian.PutUint32(p[page.HeaderSize+4:], uint32(i))
}

func stampOf(p page.Page) (int, int) {
	return int(binary.BigEndian.Uint32(p[page.HeaderSize:])), int(binary.BigEndian.Uint32(p[page.HeaderSize+4:]))
}

func TestManyGoroutinesCanAllocateWriteReadFreeAndCheckpointAtOnce(t *testing.T) {
	const goroutines = 8
	per := 150
	if testing.Short() {
		per = 40
	}
	path := filepath.Join(t.TempDir(), "concurrent.db")
	s, err := Create(path)
	if err != nil {
		t.Fatal(err)
	}
	type owned struct {
		id   core.PageID
		g, i int
	}
	kept := make([][]owned, goroutines)
	freed := make([][]core.PageID, goroutines)
	var wg sync.WaitGroup
	done := make(chan struct{})
	checkpoints := make(chan error, 1)
	go func() {
		n := 0
		for {
			select {
			case <-done:
				checkpoints <- nil
				return
			default:
			}
			if n < 10 {
				if err := s.Checkpoint(); err != nil {
					checkpoints <- err
					return
				}
				n++
			}
			_ = s.Meta()
			_ = s.Stats()
			_ = s.Describe()
			_ = s.DescribeFreeList()
			_ = s.FreePages()
			_ = s.PendingPages()
			_ = s.ListPages()
			_ = s.Dirty()
			_ = s.Durable()
			_ = s.Root()
		}
	}()
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < per; i++ {
				p, err := s.Allocate(page.KindHeap)
				if err != nil {
					t.Error(err)
					return
				}
				stampPage(p, g, i)
				if err := s.Write(p); err != nil {
					t.Error(err)
					return
				}
				back, err := s.Read(p.ID())
				if err != nil {
					t.Error(err)
					return
				}
				if bg, bi := stampOf(back); bg != g || bi != i {
					t.Errorf("goroutine %d wrote page %d as (%d, %d) and read back (%d, %d)", g, p.ID(), g, i, bg, bi)
					return
				}
				if i%3 == 0 {
					if err := s.Free(p.ID()); err != nil {
						t.Error(err)
						return
					}
					freed[g] = append(freed[g], p.ID())
					continue
				}
				kept[g] = append(kept[g], owned{p.ID(), g, i})
			}
		}(g)
	}
	wg.Wait()
	close(done)
	if err := <-checkpoints; err != nil {
		t.Fatal(err)
	}
	if t.Failed() {
		return
	}
	seen := map[core.PageID]bool{}
	for g := range kept {
		for _, o := range kept[g] {
			if seen[o.id] {
				t.Fatalf("page %d was handed to two owners", o.id)
			}
			seen[o.id] = true
		}
	}
	if err := s.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	if err := s.Verify(); err != nil {
		t.Fatal(err)
	}
	if err := s.SetRoot(kept[0][0].id); err != nil {
		t.Fatal(err)
	}
	if err := s.Sync(); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Verify(); err != nil {
		t.Fatal(err)
	}
	if s.Root() != kept[0][0].id {
		t.Fatalf("root %d after reopen, want %d", s.Root(), kept[0][0].id)
	}
	for g := range kept {
		for _, o := range kept[g] {
			p, err := s.Read(o.id)
			if err != nil {
				t.Fatal(err)
			}
			if bg, bi := stampOf(p); bg != o.g || bi != o.i {
				t.Fatalf("page %d holds (%d, %d), want (%d, %d)", o.id, bg, bi, o.g, o.i)
			}
		}
		for _, id := range freed[g] {
			if seen[id] {
				continue
			}
			if _, err := s.Read(id); !errors.Is(err, ErrNotAllocated) {
				t.Fatalf("freed page %d reads back with %v", id, err)
			}
		}
	}
	if _, err := s.DescribeMeta(); err != nil {
		t.Fatal(err)
	}
	if err := s.Discard(); err != nil {
		t.Fatal(err)
	}
}
