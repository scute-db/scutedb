package nullbits

import "strings"

type Bitmap []byte

func Size(fields int) int {
	if fields <= 0 {
		return 0
	}
	return (fields + 7) / 8
}

func New(fields int) Bitmap { return make(Bitmap, Size(fields)) }

func (b Bitmap) Fields() int { return len(b) * 8 }

func (b Bitmap) SetNull(i int) {
	if i < 0 || i >= b.Fields() {
		return
	}
	b[i/8] |= 1 << (uint(i) % 8)
}

func (b Bitmap) SetPresent(i int) {
	if i < 0 || i >= b.Fields() {
		return
	}
	b[i/8] &^= 1 << (uint(i) % 8)
}

func (b Bitmap) IsNull(i int) bool {
	if i < 0 || i >= b.Fields() {
		return false
	}
	return b[i/8]&(1<<(uint(i)%8)) != 0
}

func (b Bitmap) Any(fields int) bool {
	for i := 0; i < fields && i < b.Fields(); i++ {
		if b.IsNull(i) {
			return true
		}
	}
	return false
}

func (b Bitmap) Count(fields int) int {
	n := 0
	for i := 0; i < fields && i < b.Fields(); i++ {
		if b.IsNull(i) {
			n++
		}
	}
	return n
}

func (b Bitmap) String() string {
	var sb strings.Builder
	for i := 0; i < b.Fields(); i++ {
		if i > 0 && i%8 == 0 {
			sb.WriteByte(' ')
		}
		if b.IsNull(i) {
			sb.WriteByte('N')
		} else {
			sb.WriteByte('.')
		}
	}
	return sb.String()
}

func (b Bitmap) Describe(fields int) string {
	var sb strings.Builder
	for i := 0; i < b.Fields(); i++ {
		if i > 0 && i%8 == 0 {
			sb.WriteByte(' ')
		}
		switch {
		case i >= fields:
			sb.WriteByte('-')
		case b.IsNull(i):
			sb.WriteByte('N')
		default:
			sb.WriteByte('.')
		}
	}
	return sb.String()
}
