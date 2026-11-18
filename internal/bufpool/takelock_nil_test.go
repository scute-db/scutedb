package bufpool

import (
	"errors"
	"fmt"
	"sync"
	"testing"
)

func takeLockCatchingPanics(m *MTR, l sync.Locker) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v", r)
		}
	}()
	return m.TakeLock(l)
}

func TestTakeLockRefusesANilPointerLockWithErrBadLockInsteadOfPanicking(t *testing.T) {
	p := mustPool(t, NewMemSource(), Options{Frames: 1})
	m := p.Begin()
	var mu *sync.Mutex
	var rw *sync.RWMutex
	for _, bad := range []struct {
		name string
		l    sync.Locker
	}{{"a nil *sync.Mutex", mu}, {"a nil *sync.RWMutex", rw}} {
		if err := takeLockCatchingPanics(m, bad.l); !errors.Is(err, ErrBadLock) {
			t.Errorf("TakeLock(%s) gave %v, want ErrBadLock", bad.name, err)
		}
		if m.HasLock(bad.l) {
			t.Errorf("the MTR claims to hold %s", bad.name)
		}
	}
	if err := m.Abort(); err != nil {
		t.Fatalf("Abort after the refused locks gave %v", err)
	}
}
