package nullbits

type Bool3 uint8

const (
	False Bool3 = iota
	True
	Unknown
)

func (b Bool3) norm() Bool3 {
	if b > Unknown {
		return Unknown
	}
	return b
}

func (b Bool3) String() string {
	switch b {
	case False:
		return "FALSE"
	case True:
		return "TRUE"
	default:
		return "UNKNOWN"
	}
}

func (b Bool3) Not() Bool3 {
	switch b.norm() {
	case True:
		return False
	case False:
		return True
	default:
		return Unknown
	}
}

func (b Bool3) And(o Bool3) Bool3 {
	b, o = b.norm(), o.norm()
	if b == False || o == False {
		return False
	}
	if b == Unknown || o == Unknown {
		return Unknown
	}
	return True
}

func (b Bool3) Or(o Bool3) Bool3 {
	b, o = b.norm(), o.norm()
	if b == True || o == True {
		return True
	}
	if b == Unknown || o == Unknown {
		return Unknown
	}
	return False
}

func (b Bool3) IsTrue() bool { return b.norm() == True }

func FromBool(v bool) Bool3 {
	if v {
		return True
	}
	return False
}

func EqualInt64(a, b *int64) Bool3 {
	if a == nil || b == nil {
		return Unknown
	}
	return FromBool(*a == *b)
}

func LessInt64(a, b *int64) Bool3 {
	if a == nil || b == nil {
		return Unknown
	}
	return FromBool(*a < *b)
}

func IsNull(v *int64) Bool3 { return FromBool(v == nil) }
