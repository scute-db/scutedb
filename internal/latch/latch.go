package latch

import (
	"errors"
	"fmt"
	"sort"
	"sync"
)

const (
	MaxStripes = 1 << 16
	golden     = 0x9E3779B97F4A7C15
)

var ErrNilLock = errors.New("scutedb/latch: nil lock")

type stripe struct {
	sync.RWMutex
	_ [40]byte
}

type Striped struct {
	locks []stripe
	shift uint
}

func NewStriped(n int) *Striped {
	size, shift := 1, uint(64)
	for size < n && size < MaxStripes {
		size <<= 1
		shift--
	}
	return &Striped{locks: make([]stripe, size), shift: shift}
}

func (s *Striped) Stripes() int { return len(s.locks) }

func (s *Striped) Index(key uint64) int { return int((key * golden) >> s.shift) }

func (s *Striped) At(i int) *sync.RWMutex { return &s.locks[i&(len(s.locks)-1)].RWMutex }

func (s *Striped) For(key uint64) *sync.RWMutex { return &s.locks[s.Index(key)].RWMutex }

func (s *Striped) Lock(key uint64)    { s.For(key).Lock() }
func (s *Striped) Unlock(key uint64)  { s.For(key).Unlock() }
func (s *Striped) RLock(key uint64)   { s.For(key).RLock() }
func (s *Striped) RUnlock(key uint64) { s.For(key).RUnlock() }

func (s *Striped) LockKeys(keys ...uint64) func() {
	at := make([]int, len(keys))
	for i, k := range keys {
		at[i] = s.Index(k)
	}
	sort.Ints(at)
	var take []sync.Locker
	for i, a := range at {
		if i > 0 && a == at[i-1] {
			continue
		}
		take = append(take, &s.locks[a].RWMutex)
	}
	return lockAll(take)
}

type Ordered struct {
	ID   uint64
	Lock sync.Locker
}

func LockInOrder(locks ...Ordered) (func(), error) {
	sorted := append([]Ordered(nil), locks...)
	sort.SliceStable(sorted, func(a, b int) bool { return sorted[a].ID < sorted[b].ID })
	var take []sync.Locker
	for i, l := range sorted {
		if l.Lock == nil {
			return nil, fmt.Errorf("%w: id %d", ErrNilLock, l.ID)
		}
		if i > 0 && l.ID == sorted[i-1].ID {
			continue
		}
		take = append(take, l.Lock)
	}
	return lockAll(take), nil
}

func lockAll(take []sync.Locker) func() {
	for _, l := range take {
		l.Lock()
	}
	return func() {
		for i := len(take) - 1; i >= 0; i-- {
			take[i].Unlock()
		}
	}
}

type Mutex struct {
	once sync.Once
	ch   chan struct{}
}

func (m *Mutex) init() { m.once.Do(func() { m.ch = make(chan struct{}, 1) }) }

func (m *Mutex) Lock() {
	m.init()
	m.ch <- struct{}{}
}

func (m *Mutex) TryLock() bool {
	m.init()
	select {
	case m.ch <- struct{}{}:
		return true
	default:
		return false
	}
}

func (m *Mutex) LockOrCancel(cancel <-chan struct{}) bool {
	m.init()
	select {
	case m.ch <- struct{}{}:
		return true
	case <-cancel:
		return false
	}
}

func (m *Mutex) Unlock() {
	m.init()
	select {
	case <-m.ch:
	default:
	}
}
