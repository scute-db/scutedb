package codec

import (
	"encoding/binary"
	"math"
)

const signBit = uint64(1) << 63

func AppendKeyUint64(dst []byte, v uint64) []byte {
	return binary.BigEndian.AppendUint64(dst, v)
}

func KeyUint64(src []byte) (uint64, int, error) {
	if len(src) < 8 {
		return 0, 0, ErrTruncated
	}
	return binary.BigEndian.Uint64(src), 8, nil
}

func AppendKeyInt64(dst []byte, v int64) []byte {
	return binary.BigEndian.AppendUint64(dst, uint64(v)^signBit)
}

func KeyInt64(src []byte) (int64, int, error) {
	if len(src) < 8 {
		return 0, 0, ErrTruncated
	}
	return int64(binary.BigEndian.Uint64(src) ^ signBit), 8, nil
}

func AppendKeyFloat64(dst []byte, f float64) []byte {
	b := math.Float64bits(f)
	if b&signBit != 0 {
		b = ^b
	} else {
		b |= signBit
	}
	return binary.BigEndian.AppendUint64(dst, b)
}

func KeyFloat64(src []byte) (float64, int, error) {
	if len(src) < 8 {
		return 0, 0, ErrTruncated
	}
	b := binary.BigEndian.Uint64(src)
	if b&signBit != 0 {
		b &^= signBit
	} else {
		b = ^b
	}
	return math.Float64frombits(b), 8, nil
}

func AppendKeyString(dst []byte, s string) []byte { return append(dst, s...) }
