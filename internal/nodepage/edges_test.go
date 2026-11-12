package nodepage

import (
	"bytes"
	"encoding/binary"
	"errors"
	"testing"
)

func TestInsertJustBeforeTheLastKeyStillChecksItsRightNeighbour(t *testing.T) {
	n := NewLeaf(1)
	for i, k := range []string{"b", "d", "f"} {
		if err := n.InsertLeaf(i, []byte(k), rid(i)); err != nil {
			t.Fatal(err)
		}
	}
	before := append([]byte(nil), n.Page...)
	for _, k := range []string{"f", "g"} {
		if err := n.InsertLeaf(2, []byte(k), rid(9)); !errors.Is(err, ErrOrder) {
			t.Fatalf("InsertLeaf(2, %q) before the last key %q gave %v, want ErrOrder", k, "f", err)
		}
	}
	if !bytes.Equal(n.Page, before) {
		t.Fatal("a refused insert changed the page")
	}
}

func TestRemoveRefusesASlotWithZeroLength(t *testing.T) {
	n := NewLeaf(1)
	for i := 0; i < 3; i++ {
		if err := n.InsertLeaf(i, key(i), rid(i)); err != nil {
			t.Fatal(err)
		}
	}
	binary.BigEndian.PutUint16(n.Page[n.slotOffset(1)+2:], 0)
	before := append([]byte(nil), n.Page...)
	if err := n.Remove(1); !errors.Is(err, ErrInvariant) {
		t.Fatalf("Remove of a zero-length slot gave %v, want ErrInvariant", err)
	}
	if !bytes.Equal(n.Page, before) {
		t.Fatal("a refused Remove changed the page")
	}
}
