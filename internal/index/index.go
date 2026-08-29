package index

import "github.com/suhailopensource/scute/internal/core"

type Iterator interface {
	Next() bool
	Key() []byte
	RowID() core.RowID
	Err() error
	Close() error
}

type Index interface {
	Get(key []byte) (core.RowID, error)
	Put(key []byte, rid core.RowID) error
	Delete(key []byte) error

	Scan(from, to []byte) (Iterator, error)
	Close() error
}
