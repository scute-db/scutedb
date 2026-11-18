package pagestore

import (
	"errors"
	"path/filepath"
	"sync"
	"testing"

	"github.com/scute-db/scutedb/internal/page"
)

func TestEveryStoreMethodIsSafeAlongsideAllocateFreeAndCheckpoint(t *testing.T) {
	s, err := Create(filepath.Join(t.TempDir(), "all.db"))
	if err != nil {
		t.Fatal(err)
	}
	p, err := s.Allocate(page.KindHeap)
	if err != nil {
		t.Fatal(err)
	}
	root := p.ID()
	stop := make(chan struct{})
	readers := map[string]func() error{
		"SetRoot":      func() error { return s.SetRoot(root) },
		"Sync":         s.Sync,
		"Verify":       s.Verify,
		"DescribeMeta": func() error { _, err := s.DescribeMeta(); return err },
		"Durable":      func() error { _ = s.Durable(); return nil },
		"Root":         func() error { _ = s.Root(); return nil },
		"ListPages":    func() error { _ = s.ListPages(); return nil },
	}
	var rwg sync.WaitGroup
	for name, call := range readers {
		rwg.Add(1)
		go func(name string, call func() error) {
			defer rwg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				if err := call(); err != nil {
					t.Errorf("%s: %v", name, err)
					return
				}
			}
		}(name, call)
	}
	for i := 0; i < 60; i++ {
		q, err := s.Allocate(page.KindHeap)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.Write(q); err != nil {
			t.Fatal(err)
		}
		if i%2 == 0 {
			if err := s.Free(q.ID()); err != nil {
				t.Fatal(err)
			}
		}
		if err := s.Checkpoint(); err != nil {
			t.Fatal(err)
		}
	}
	close(stop)
	rwg.Wait()
	var wg sync.WaitGroup
	wg.Add(5)
	errs := make(chan error, 5)
	go func() { defer wg.Done(); errs <- s.Close() }()
	go func() { defer wg.Done(); errs <- s.Discard() }()
	go func() {
		defer wg.Done()
		for {
			if _, err := s.DescribeMeta(); err != nil {
				if errors.Is(err, ErrClosed) {
					err = nil
				}
				errs <- err
				return
			}
		}
	}()
	go func() {
		defer wg.Done()
		for {
			if err := s.Sync(); err != nil {
				if errors.Is(err, ErrClosed) {
					err = nil
				}
				errs <- err
				return
			}
		}
	}()
	go func() {
		defer wg.Done()
		_, err := s.Allocate(page.KindHeap)
		if errors.Is(err, ErrClosed) {
			err = nil
		}
		errs <- err
	}()
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestDescribeMetaOnAStoreBeingClosedEitherDescribesItOrSaysItIsClosed(t *testing.T) {
	for i := 0; i < 8; i++ {
		s, err := Create(filepath.Join(t.TempDir(), "closing.db"))
		if err != nil {
			t.Fatal(err)
		}
		if err := s.Checkpoint(); err != nil {
			t.Fatal(err)
		}
		end := s.Close
		if i%2 == 1 {
			end = s.Discard
		}
		described := make(chan error, 1)
		closed := make(chan error, 1)
		go func() {
			_, err := s.DescribeMeta()
			described <- err
		}()
		go func() { closed <- end() }()
		if err := <-closed; err != nil {
			t.Fatal(err)
		}
		if err := <-described; err != nil && !errors.Is(err, ErrClosed) {
			t.Fatalf("DescribeMeta while the store closed gave %v, want a description or ErrClosed", err)
		}
	}
}
