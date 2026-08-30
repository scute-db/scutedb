package nullbits

import "testing"

func TestPaddingBitsDoNotCountAsFields(t *testing.T) {
	b := New(5)
	b.SetNull(7)
	if got := b.Count(5); got != 0 {
		t.Fatalf("Count(5) = %d, want 0: a padding bit was counted as a null field", got)
	}
	if b.Any(5) {
		t.Fatal("Any(5) reported a null when only a padding bit is set")
	}
	if got := b.Count(8); got != 1 {
		t.Fatalf("Count(8) = %d, want 1", got)
	}
}

func TestBool3RejectsGarbageConsistently(t *testing.T) {
	g := Bool3(99)
	if g.String() != "UNKNOWN" {
		t.Fatalf("Bool3(99).String() = %v, want UNKNOWN", g)
	}
	if got := g.And(True); got != Unknown {
		t.Fatalf("Bool3(99) AND TRUE = %v, want UNKNOWN", got)
	}
	if got := g.Or(False); got != Unknown {
		t.Fatalf("Bool3(99) OR FALSE = %v, want UNKNOWN", got)
	}
	if got := g.Not(); got != Unknown {
		t.Fatalf("NOT Bool3(99) = %v, want UNKNOWN", got)
	}
	if g.IsTrue() {
		t.Fatal("Bool3(99).IsTrue() reported true")
	}
}
