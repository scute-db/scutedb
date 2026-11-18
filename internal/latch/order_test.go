package latch

import (
	"sync"
	"testing"
)

func TestLockInOrderLeavesTheCallersListInItsOwnOrder(t *testing.T) {
	var a, b, c sync.Mutex
	list := []Ordered{{ID: 3, Lock: &a}, {ID: 1, Lock: &b}, {ID: 2, Lock: &c}}
	unlock, err := LockInOrder(list...)
	if err != nil {
		t.Fatal(err)
	}
	unlock()
	if list[0].ID != 3 || list[1].ID != 1 || list[2].ID != 2 {
		t.Fatalf("LockInOrder sorted the caller's slice in place: ids %d %d %d, want 3 1 2", list[0].ID, list[1].ID, list[2].ID)
	}
}
