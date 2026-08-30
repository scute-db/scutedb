package naive

import (
	"bufio"
	"os"
	"strings"

	"github.com/scute-db/scutedb/internal/core"
)

type DB struct {
	path string
	f    *os.File
	w    *bufio.Writer
}

func Open(path string) (*DB, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}

	return &DB{path: path, f: f, w: bufio.NewWriterSize(f, 64*1024)}, nil
}

func (db *DB) Put(key, value string) error {
	_, err := db.w.WriteString(key + "\t" + value + "\n")
	return err
}

func (db *DB) Flush() error { return db.w.Flush() }

func (db *DB) Get(key string) (string, error) {
	if err := db.Flush(); err != nil {
		return "", err
	}
	f, err := os.Open(db.path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1024*1024), 1024*1024)
	found := false
	var last string
	for sc.Scan() {
		line := sc.Text()
		tab := strings.IndexByte(line, '\t')
		if tab < 0 {
			continue
		}
		if line[:tab] == key {
			last, found = line[tab+1:], true
		}
	}
	if err := sc.Err(); err != nil {
		return "", err
	}
	if !found || last == "" {
		return "", core.ErrNotFound
	}
	return last, nil
}

func (db *DB) Stats() (lines, live, torn int, bytes int64, err error) {
	if err = db.Flush(); err != nil {
		return
	}
	st, err := os.Stat(db.path)
	if err != nil {
		return
	}
	bytes = st.Size()

	f, err := os.Open(db.path)
	if err != nil {
		return
	}
	defer f.Close()

	seen := map[string]bool{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1024*1024), 1024*1024)
	for sc.Scan() {
		lines++
		line := sc.Text()
		tab := strings.IndexByte(line, '\t')
		if tab < 0 {
			torn++
			continue
		}
		seen[line[:tab]] = true
	}
	live = len(seen)
	err = sc.Err()
	return
}

func (db *DB) Compact() (written int64, err error) {
	if err = db.Flush(); err != nil {
		return 0, err
	}
	f, err := os.Open(db.path)
	if err != nil {
		return 0, err
	}
	order := []string{}
	latest := map[string]string{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1024*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		tab := strings.IndexByte(line, '\t')
		if tab < 0 {
			continue
		}
		k := line[:tab]
		if _, ok := latest[k]; !ok {
			order = append(order, k)
		}
		latest[k] = line[tab+1:]
	}
	f.Close()
	if err = sc.Err(); err != nil {
		return 0, err
	}

	tmp := db.path + ".compact"
	out, err := os.Create(tmp)
	if err != nil {
		return 0, err
	}
	w := bufio.NewWriterSize(out, 256*1024)
	for _, k := range order {
		v := latest[k]
		if v == "" {
			continue
		}
		n, werr := w.WriteString(k + "\t" + v + "\n")
		if werr != nil {
			out.Close()
			return written, werr
		}
		written += int64(n)
	}
	if err = w.Flush(); err != nil {
		out.Close()
		return written, err
	}
	out.Close()

	db.f.Close()
	if err = os.Rename(tmp, db.path); err != nil {
		return written, err
	}
	f2, err := os.OpenFile(db.path, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		return written, err
	}
	db.f, db.w = f2, bufio.NewWriterSize(f2, 64*1024)
	return written, nil
}

func (db *DB) Close() error {
	if err := db.w.Flush(); err != nil {
		db.f.Close()
		return err
	}
	return db.f.Close()
}
