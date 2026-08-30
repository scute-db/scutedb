package codec

import "errors"

const MaxVarintLen64 = 10

var (
	ErrTruncated = errors.New("scutedb/codec: buffer ended mid-value")
	ErrOverflow  = errors.New("scutedb/codec: varint overflows 64 bits")
)

func AppendUvarint(dst []byte, v uint64) []byte {
	for v >= 0x80 {
		dst = append(dst, byte(v)|0x80)
		v >>= 7
	}
	return append(dst, byte(v))
}

func Uvarint(src []byte) (uint64, int, error) {
	var x uint64
	var s uint
	for i := 0; i < len(src); i++ {
		b := src[i]
		if i == MaxVarintLen64-1 && b > 1 {
			return 0, 0, ErrOverflow
		}
		if b < 0x80 {
			return x | uint64(b)<<s, i + 1, nil
		}
		x |= uint64(b&0x7f) << s
		s += 7
	}
	return 0, 0, ErrTruncated
}

func UvarintLen(v uint64) int {
	n := 1
	for v >= 0x80 {
		v >>= 7
		n++
	}
	return n
}

func zigzag(v int64) uint64 { return uint64(v<<1) ^ uint64(v>>63) }

func unzigzag(u uint64) int64 { return int64(u>>1) ^ -int64(u&1) }

func AppendVarint(dst []byte, v int64) []byte { return AppendUvarint(dst, zigzag(v)) }

func Varint(src []byte) (int64, int, error) {
	u, n, err := Uvarint(src)
	if err != nil {
		return 0, 0, err
	}
	return unzigzag(u), n, nil
}
