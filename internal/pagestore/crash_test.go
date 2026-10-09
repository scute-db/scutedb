package pagestore

import (
	"bytes"
	"testing"

	"github.com/scute-db/scutedb/internal/page"
)

func TestFreeAfterACheckpointMustNotCorruptTheDurableTree(t *testing.T) {
	path := tmpPath(t)
	s := mustCreate(t, path)

	old := mustAlloc(t, s)
	copy(old[page.HeaderSize:], []byte("committed"))
	if err := s.Write(old); err != nil {
		t.Fatal(err)
	}
	if err := s.SetRoot(old.ID()); err != nil {
		t.Fatal(err)
	}
	if err := s.Checkpoint(); err != nil {
		t.Fatal(err)
	}

	replacement := mustAlloc(t, s)
	if err := s.SetRoot(replacement.ID()); err != nil {
		t.Fatal(err)
	}
	if err := s.Free(old.ID()); err != nil {
		t.Fatal(err)
	}
	if err := s.Discard(); err != nil {
		t.Fatal(err)
	}

	back := mustOpen(t, path)
	defer back.Close()
	if back.Root() != old.ID() {
		t.Fatalf("after the crash the root is %d, want the checkpointed root %d", back.Root(), old.ID())
	}
	got, err := back.Read(back.Root())
	if err != nil {
		t.Fatalf("reading the checkpointed root after a crash: %v", err)
	}
	if !bytes.HasPrefix(got[page.HeaderSize:], []byte("committed")) {
		t.Fatalf("the checkpointed root was overwritten before the checkpoint that freed it: kind %s, bytes % 02X",
			got.Kind(), got[page.HeaderSize:page.HeaderSize+9])
	}
}

func TestReuseAfterACheckpointMustNotCorruptTheDurableFreeList(t *testing.T) {
	path := tmpPath(t)
	s := mustCreate(t, path)

	a := mustAlloc(t, s)
	b := mustAlloc(t, s)
	mustAlloc(t, s)
	if err := s.Free(a.ID()); err != nil {
		t.Fatal(err)
	}
	if err := s.Free(b.ID()); err != nil {
		t.Fatal(err)
	}
	if err := s.Checkpoint(); err != nil {
		t.Fatal(err)
	}

	reused := mustAlloc(t, s)
	copy(reused[page.HeaderSize:], []byte("new data"))
	if err := s.Write(reused); err != nil {
		t.Fatal(err)
	}
	if err := s.Discard(); err != nil {
		t.Fatal(err)
	}

	back := mustOpen(t, path)
	defer back.Close()
	if err := back.Verify(); err != nil {
		t.Fatalf("a crash after reusing a free page left the durable free list corrupt: %v", err)
	}
	if back.Meta().FreeCount != 2 {
		t.Errorf("after the crash the free count is %d, want the checkpointed 2", back.Meta().FreeCount)
	}
}
