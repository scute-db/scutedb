package core

import "errors"

type PageID uint32

type RowID struct {
	Page PageID
	Slot uint16
}

var (
	ErrNotFound  = errors.New("scute: not found")
	ErrShortPage = errors.New("scute: short page read")
	ErrCorrupt   = errors.New("scute: corrupt data")
)
