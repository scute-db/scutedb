package nullbits

import "testing"

func TestNotTable(t *testing.T) {
	cases := map[Bool3]Bool3{True: False, False: True, Unknown: Unknown}
	for in, want := range cases {
		if got := in.Not(); got != want {
			t.Fatalf("NOT %v = %v, want %v", in, got, want)
		}
	}
}

func TestAndTable(t *testing.T) {
	all := []Bool3{True, False, Unknown}
	want := map[[2]Bool3]Bool3{
		{True, True}: True, {True, False}: False, {True, Unknown}: Unknown,
		{False, True}: False, {False, False}: False, {False, Unknown}: False,
		{Unknown, True}: Unknown, {Unknown, False}: False, {Unknown, Unknown}: Unknown,
	}
	for _, a := range all {
		for _, b := range all {
			if got := a.And(b); got != want[[2]Bool3{a, b}] {
				t.Fatalf("%v AND %v = %v, want %v", a, b, got, want[[2]Bool3{a, b}])
			}
		}
	}
}

func TestOrTable(t *testing.T) {
	all := []Bool3{True, False, Unknown}
	want := map[[2]Bool3]Bool3{
		{True, True}: True, {True, False}: True, {True, Unknown}: True,
		{False, True}: True, {False, False}: False, {False, Unknown}: Unknown,
		{Unknown, True}: True, {Unknown, False}: Unknown, {Unknown, Unknown}: Unknown,
	}
	for _, a := range all {
		for _, b := range all {
			if got := a.Or(b); got != want[[2]Bool3{a, b}] {
				t.Fatalf("%v OR %v = %v, want %v", a, b, got, want[[2]Bool3{a, b}])
			}
		}
	}
}

func TestNullEqualsNullIsUnknown(t *testing.T) {
	if got := EqualInt64(nil, nil); got != Unknown {
		t.Fatalf("NULL = NULL gave %v, want UNKNOWN", got)
	}
	v := int64(5)
	if got := EqualInt64(&v, nil); got != Unknown {
		t.Fatalf("5 = NULL gave %v, want UNKNOWN", got)
	}
	w := int64(5)
	if got := EqualInt64(&v, &w); got != True {
		t.Fatalf("5 = 5 gave %v, want TRUE", got)
	}
}

func TestExcludedMiddleDoesNotHold(t *testing.T) {
	age := (*int64)(nil)
	thirty := int64(30)
	eq := EqualInt64(age, &thirty)
	ne := eq.Not()
	either := eq.Or(ne)
	if either != Unknown {
		t.Fatalf("(x = 30) OR (x != 30) on NULL gave %v, want UNKNOWN", either)
	}
	if either.IsTrue() {
		t.Fatal("a WHERE clause would have kept this row")
	}
}
