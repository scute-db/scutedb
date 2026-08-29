package storage

import "github.com/suhailopensource/ScuteDB/internal/core"

type Iterator interface {
	Next() bool
	RowID() core.RowID
	Record() []byte
	Err() error
	Close() error
}

type Engine interface {
	Insert(rec []byte) (core.RowID, error)
	Read(rid core.RowID) ([]byte, error)

	Update(rid core.RowID, rec []byte) (core.RowID, error)
	Delete(rid core.RowID) error
	Scan() (Iterator, error)
	Sync() error
	Close() error
}
