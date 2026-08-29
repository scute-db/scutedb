package core

import "errors"

type PageID uint32

type RowID struct {
	Page PageID
	Slot uint16
}

var (
	ErrNotFound  = errors.New("scutedb: not found")
	ErrShortPage = errors.New("scutedb: short page read")
	ErrCorrupt   = errors.New("scutedb: corrupt data")
)
