package pagestore

import (
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/scute-db/scutedb/internal/core"
	"github.com/scute-db/scutedb/internal/page"
)

func describeStore(t *testing.T) *Store {
	t.Helper()
	s := mustCreate(t, tmpPath(t))
	for i := 0; i < 8; i++ {
		mustAlloc(t, s)
	}
	if err := s.SetRoot(1); err != nil {
		t.Fatal(err)
	}
	for _, id := range []core.PageID{3, 4, 5} {
		if err := s.Free(id); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	if err := s.Free(7); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestDescribeCountsEveryPageInExactlyOneState(t *testing.T) {
	s := describeStore(t)
	defer s.Close()
	out := s.Describe()
	m := s.Meta()
	inUse := int(m.NextPage) - 1 - len(s.FreePages()) - len(s.PendingPages()) - len(s.ListPages())
	for _, want := range []string{
		"handed out so far",
		"in use", "free, reusable now", "freed, waiting on a checkpoint", "holding the free list",
		"root = page 1",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("Describe is missing %q\n%s", want, out)
		}
	}
	if inUse <= 0 || !strings.Contains(out, "  in use") {
		t.Fatalf("expected some pages in use, got %d\n%s", inUse, out)
	}
}

func TestDescribeFreeListNamesEachKindOfFreePage(t *testing.T) {
	s := describeStore(t)
	defer s.Close()
	out := s.DescribeFreeList()
	if !strings.Contains(out, "pending, after checkpoint  7") {
		t.Errorf("the pending page 7 is not listed:\n%s", out)
	}
	for _, id := range s.FreePages() {
		if !strings.Contains(out, " "+strconv.Itoa(int(id))) {
			t.Errorf("free page %d is not listed:\n%s", id, out)
		}
	}
	if joinIDs(nil) != "none" {
		t.Errorf("an empty list prints as %q, want none", joinIDs(nil))
	}
}

func TestDescribeMetaOfAPoisonedStoreReturnsTheError(t *testing.T) {
	mf := newMemFile()
	s, err := create(page.NewFile(mf))
	if err != nil {
		t.Fatal(err)
	}
	mf.failAtSync = mf.syncs + 1
	if err := s.Sync(); !errors.Is(err, ErrPoisoned) {
		t.Fatalf("want ErrPoisoned, got %v", err)
	}
	if _, err := s.DescribeMeta(); !errors.Is(err, ErrPoisoned) {
		t.Errorf("DescribeMeta on a poisoned store gave %v, want ErrPoisoned", err)
	}
}

func TestDescribeOfAPageThatIsNotAMetaPageSaysSo(t *testing.T) {
	out := describeMetaPage(page.New(0, page.KindHeap))
	if !strings.Contains(out, "not a readable meta page") {
		t.Errorf("describing a heap page as a meta page should say it is unreadable:\n%s", out)
	}
}
