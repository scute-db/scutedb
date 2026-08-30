package codec

import (
	"encoding/binary"
	"math"
)

func AppendUint64(dst []byte, v uint64) []byte { return AppendUvarint(dst, v) }

func Uint64(src []byte) (uint64, int, error) { return Uvarint(src) }

func AppendInt64(dst []byte, v int64) []byte { return AppendVarint(dst, v) }

func Int64(src []byte) (int64, int, error) { return Varint(src) }

func AppendBool(dst []byte, b bool) []byte {
	if b {
		return append(dst, 1)
	}
	return append(dst, 0)
}

func Bool(src []byte) (bool, int, error) {
	if len(src) < 1 {
		return false, 0, ErrTruncated
	}
	return src[0] != 0, 1, nil
}

func AppendFloat64(dst []byte, f float64) []byte {
	return binary.BigEndian.AppendUint64(dst, math.Float64bits(f))
}

func Float64(src []byte) (float64, int, error) {
	if len(src) < 8 {
		return 0, 0, ErrTruncated
	}
	return math.Float64frombits(binary.BigEndian.Uint64(src)), 8, nil
}

func AppendBytes(dst []byte, b []byte) []byte {
	dst = AppendUvarint(dst, uint64(len(b)))
	return append(dst, b...)
}

func Bytes(src []byte) ([]byte, int, error) {
	n, hdr, err := Uvarint(src)
	if err != nil {
		return nil, 0, err
	}
	end := hdr + int(n)
	if n > uint64(len(src)) || end > len(src) || end < hdr {
		return nil, 0, ErrTruncated
	}
	return src[hdr:end], end, nil
}

func AppendString(dst []byte, s string) []byte {
	dst = AppendUvarint(dst, uint64(len(s)))
	return append(dst, s...)
}

func String(src []byte) (string, int, error) {
	b, n, err := Bytes(src)
	if err != nil {
		return "", 0, err
	}
	return string(b), n, nil
}
