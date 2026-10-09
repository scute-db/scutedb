package pagestore

import (
	"encoding/binary"
	"errors"
	"sort"
	"testing"

	"github.com/scute-db/scutedb/internal/core"
	"github.com/scute-db/scutedb/internal/page"
)

type model struct {
	inUse   map[core.PageID]uint64
	root    core.PageID
	gen     uint32
	dInUse  map[core.PageID]uint64
	dRoot   core.PageID
	nextTag uint64
}

func newModel(gen uint32) *model {
	return &model{inUse: map[core.PageID]uint64{}, dInUse: map[core.PageID]uint64{}, gen: gen}
}

func clone(m map[core.PageID]uint64) map[core.PageID]uint64 {
	out := make(map[core.PageID]uint64, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func (m *model) commit(gen uint32) {
	m.dInUse = clone(m.inUse)
	m.dRoot = m.root
	m.gen = gen
}

func (m *model) revert() {
	m.inUse = clone(m.dInUse)
	m.root = m.dRoot
}

func (m *model) sortedInUse() []core.PageID {
	ids := make([]core.PageID, 0, len(m.inUse))
	for id := range m.inUse {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(a, b int) bool { return ids[a] < ids[b] })
	return ids
}

func checkAgainstModel(t *testing.T, step int, s *Store, m *model) {
	t.Helper()
	if err := s.Verify(); err != nil {
		t.Fatalf("step %d: Verify: %v", step, err)
	}
	if s.Root() != m.root {
		t.Fatalf("step %d: root is %d, model says %d", step, s.Root(), m.root)
	}
	for id, tag := range m.inUse {
		p, err := s.Read(id)
		if err != nil {
			t.Fatalf("step %d: reading in-use page %d: %v", step, id, err)
		}
		if got := binary.BigEndian.Uint64(p[page.HeaderSize:]); got != tag {
			t.Fatalf("step %d: page %d holds tag %d, model wrote %d", step, id, got, tag)
		}
	}

	owner := make(map[core.PageID]string, int(s.Meta().NextPage))
	claim := func(ids []core.PageID, as string) {
		for _, id := range ids {
			if prev, ok := owner[id]; ok {
				t.Fatalf("step %d: page %d is both %s and %s", step, id, prev, as)
			}
			owner[id] = as
		}
	}
	claim(m.sortedInUse(), "in use")
	claim(s.FreePages(), "free")
	claim(s.PendingPages(), "pending")
	claim(s.ListPages(), "a free-list page")
	for id := core.PageID(1); id < s.Meta().NextPage; id++ {
		if _, ok := owner[id]; !ok {
			t.Fatalf("step %d: page %d was handed out and is now in no state at all, so it leaked", step, id)
		}
	}
	if len(owner) != int(s.Meta().NextPage)-1 {
		t.Fatalf("step %d: %d pages accounted for, %d handed out", step, len(owner), s.Meta().NextPage-1)
	}
}

func FuzzCrashAnywhereKeepsTheLastCheckpoint(f *testing.F) {
	f.Add([]byte{0, 0, 0, 3, 0, 2, 1, 4, 0, 0, 2, 2, 7, 5})
	f.Add([]byte{0, 0, 3, 0, 4, 0, 2, 0, 4, 8, 1, 0, 6, 0})
	f.Add([]byte{0, 1, 0, 1, 0, 3, 1, 2, 3, 2, 5, 8, 2, 0, 2, 1, 4, 7, 0xAA})
	f.Add([]byte{0, 0, 0, 0, 2, 0, 2, 1, 2, 2, 8, 0, 0, 0, 8, 1, 8, 2, 7, 0x55})
	f.Add([]byte{0, 0, 3, 0, 4, 0, 1, 9, 0x0F, 0, 4, 9, 0x80, 7, 0xFF})

	f.Fuzz(func(t *testing.T, ops []byte) {
		if len(ops) > 400 {
			ops = ops[:400]
		}
		mf := newMemFile()
		s, err := create(page.NewFile(mf))
		if err != nil {
			t.Fatal(err)
		}
		m := newModel(s.Durable().Checkpoint)

		reopen := func(step int) {
			back, _, err := openFile(mf)
			if err != nil {
				t.Fatalf("step %d: reopening: %v", step, err)
			}
			s = back
		}
		subset := func(seed byte) func(int) bool {
			return func(i int) bool { return (uint(seed)>>(uint(i)%8))&1 == 1 }
		}

		for i := 0; i < len(ops); i++ {
			var arg byte
			if i+1 < len(ops) {
				arg = ops[i+1]
			}

			switch ops[i] % 10 {
			case 0, 1:
				p, err := s.Allocate(page.KindHeap)
				if err != nil {
					t.Fatalf("step %d: Allocate: %v", i, err)
				}
				if _, dup := m.inUse[p.ID()]; dup {
					t.Fatalf("step %d: page %d was handed out while still in use", i, p.ID())
				}
				if _, durable := m.dInUse[p.ID()]; durable {
					t.Fatalf("step %d: page %d was handed out while the last checkpoint still depends on it", i, p.ID())
				}
				m.nextTag++
				binary.BigEndian.PutUint64(p[page.HeaderSize:], m.nextTag)
				if err := s.Write(p); err != nil {
					t.Fatalf("step %d: Write: %v", i, err)
				}
				m.inUse[p.ID()] = m.nextTag

			case 2:
				ids := m.sortedInUse()
				if len(ids) == 0 {
					continue
				}
				id := ids[int(arg)%len(ids)]
				err := s.Free(id)
				if id == m.root {
					if err == nil {
						t.Fatalf("step %d: freeing the root %d was allowed", i, id)
					}
					continue
				}
				if err != nil {
					t.Fatalf("step %d: Free(%d): %v", i, id, err)
				}
				delete(m.inUse, id)

			case 3:
				ids := m.sortedInUse()
				if len(ids) == 0 {
					continue
				}
				id := ids[int(arg)%len(ids)]
				if err := s.SetRoot(id); err != nil {
					t.Fatalf("step %d: SetRoot(%d): %v", i, id, err)
				}
				m.root = id

			case 4:
				if err := s.Checkpoint(); err != nil {
					t.Fatalf("step %d: Checkpoint: %v", i, err)
				}
				m.commit(s.Durable().Checkpoint)

			case 5:
				if err := s.Close(); err != nil {
					t.Fatalf("step %d: Close: %v", i, err)
				}
				mf.closed = false
				reopen(i)
				m.commit(s.Durable().Checkpoint)

			case 6:
				if err := s.Discard(); err != nil {
					t.Fatal(err)
				}
				mf.restartEvicting(subset(arg))
				reopen(i)
				m.revert()
				if s.Durable().Checkpoint != m.gen {
					t.Fatalf("step %d: after a process crash the generation is %d, want %d",
						i, s.Durable().Checkpoint, m.gen)
				}

			case 7:
				if err := s.Discard(); err != nil {
					t.Fatal(err)
				}
				mf.powerLoss(subset(arg))
				reopen(i)
				m.revert()
				if s.Durable().Checkpoint != m.gen {
					t.Fatalf("step %d: after a power loss the generation is %d, want %d",
						i, s.Durable().Checkpoint, m.gen)
				}

			case 8:
				before := m.gen
				mf.failAtSync = mf.syncs + 1 + int(arg%3)
				mf.failKeep = subset(arg >> 2)
				err := s.Checkpoint()
				mf.failAtSync, mf.failKeep = 0, nil
				if err == nil {
					m.commit(s.Durable().Checkpoint)
					break
				}
				if err := s.Discard(); err != nil {
					t.Fatal(err)
				}
				if arg&0x80 != 0 {
					mf.powerLoss(subset(arg >> 3))
				} else {
					mf.restartEvicting(subset(arg >> 1))
				}
				reopen(i)
				switch got := s.Durable().Checkpoint; got {
				case before:
					m.revert()
				case before + 1:
					m.commit(got)
				default:
					t.Fatalf("step %d: a checkpoint that failed mid-way recovered to generation %d; "+
						"only %d (before) or %d (after) are possible", i, got, before, before+1)
				}

			case 9:
				mf.failAtSync = mf.syncs + 1
				mf.failKeep = subset(arg)
				err := s.Sync()
				mf.failAtSync, mf.failKeep = 0, nil
				if err == nil {
					break
				}
				if errors.Is(err, ErrPoisoned) {
					if err := s.Discard(); err != nil {
						t.Fatal(err)
					}
					if arg&0x80 != 0 {
						mf.powerLoss(subset(arg >> 3))
					} else {
						mf.restartEvicting(subset(arg >> 1))
					}
					reopen(i)
					m.revert()
					break
				}
				if err := s.Checkpoint(); err != nil {
					t.Fatalf("step %d: Checkpoint after an unpoisoned Sync failure: %v", i, err)
				}
				m.commit(s.Durable().Checkpoint)
				if err := s.Discard(); err != nil {
					t.Fatal(err)
				}
				mf.keepNothing()
				reopen(i)
			}
			checkAgainstModel(t, i, s, m)
		}
	})
}
