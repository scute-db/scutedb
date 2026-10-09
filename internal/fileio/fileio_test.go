package fileio

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
)

func TestSyncDirToleratesOnlyFilesystemsThatCannotDoIt(t *testing.T) {
	dir := t.TempDir()
	orig := syncFile
	defer func() { syncFile = orig }()

	cases := []struct {
		errno syscall.Errno
		ok    bool
		why   string
	}{
		{0, true, "success"},
		{syscall.EINVAL, true, "some NFS and FUSE filesystems cannot fsync a directory at all"},
		{syscall.ENOTSUP, true, "the filesystem does not support it"},
		{syscall.EOPNOTSUPP, true, "the filesystem does not support it"},
		{syscall.EIO, false, "a real I/O error: the directory entry may not be on disk"},
		{syscall.ENOSPC, false, "the disk is full"},
		{syscall.EACCES, false, "a permission problem is not the filesystem lacking the feature"},
	}
	for _, c := range cases {
		errno := c.errno
		syncFile = func(f *os.File) error {
			if errno == 0 {
				return nil
			}
			return &os.PathError{Op: "sync", Path: f.Name(), Err: errno}
		}
		err := SyncDir(dir)
		if (err == nil) != c.ok {
			t.Errorf("fsync of a directory failing with %v (%s): SyncDir returned %v", errno, c.why, err)
		}
		if !c.ok && !errors.Is(err, errno) {
			t.Errorf("SyncDir lost the cause: got %v, want %v in the chain", err, errno)
		}
	}
}

func TestSyncParentFollowsASymlinkToTheRealDirectory(t *testing.T) {
	linkDir, realDir := t.TempDir(), t.TempDir()
	real := filepath.Join(realDir, "f")
	if err := os.WriteFile(real, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(linkDir, "f")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}

	orig := syncFile
	defer func() { syncFile = orig }()
	var synced []string
	syncFile = func(f *os.File) error {
		synced = append(synced, f.Name())
		return nil
	}
	if err := SyncParent(link); err != nil {
		t.Fatal(err)
	}

	resolved, err := filepath.EvalSymlinks(realDir)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, d := range synced {
		if r, err := filepath.EvalSymlinks(d); err == nil && r == resolved {
			found = true
		}
	}
	if !found {
		t.Errorf("SyncParent(%s) fsynced %v, never the directory %s that holds the file's entry", link, synced, realDir)
	}
}

func TestRawDirSplitsWithoutCleaning(t *testing.T) {
	cases := map[string]string{
		"a/b/c":          "a/b",
		"c":              ".",
		"/c":             "/",
		"a/link/../c/x":  "a/link/../c",
		"./x":            ".",
		"a//x":           "a/",
		"/tmp/a/../b.db": "/tmp/a/..",
	}
	for in, want := range cases {
		if got := rawDir(in); got != want {
			t.Errorf("rawDir(%q) = %q, want %q; cleaning here would change what the kernel resolves", in, got, want)
		}
	}
}

func TestOpenExistingRefusesAMissingFileAndDoesNotCreateIt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing.db")
	if _, err := OpenExisting(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("OpenExisting of a missing file gave %v, want os.ErrNotExist", err)
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("OpenExisting created the file: %v", err)
	}
	f, err := Open(path)
	if err != nil {
		t.Fatalf("Open, unlike OpenExisting, creates a missing file: %v", err)
	}
	f.Close()
}

func TestSizeAfterCloseFails(t *testing.T) {
	f, err := Open(filepath.Join(t.TempDir(), "f"))
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	if _, err := f.Size(); err == nil {
		t.Error("Size of a closed file returned no error")
	}
}

func TestSyncParentGivesUpOnASymlinkLoop(t *testing.T) {
	dir := t.TempDir()
	if err := os.Symlink("b", filepath.Join(dir, "a")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("a", filepath.Join(dir, "b")); err != nil {
		t.Fatal(err)
	}
	if err := SyncParent(filepath.Join(dir, "a")); !errors.Is(err, syscall.ELOOP) {
		t.Fatalf("SyncParent on a symlink loop gave %v, want ELOOP", err)
	}
}

func TestSyncParentOfAMissingPathFails(t *testing.T) {
	if err := SyncParent(filepath.Join(t.TempDir(), "nope", "x.db")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("SyncParent of a missing path gave %v, want os.ErrNotExist", err)
	}
}

func TestSyncDirOfAMissingDirectoryFails(t *testing.T) {
	if err := SyncDir(filepath.Join(t.TempDir(), "nope")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("SyncDir of a missing directory gave %v, want os.ErrNotExist", err)
	}
}

func TestSyncParentFsyncsTheDirectoryOfAPlainFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "plain.db")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	orig := syncFile
	defer func() { syncFile = orig }()
	var synced []string
	syncFile = func(f *os.File) error {
		synced = append(synced, f.Name())
		return nil
	}
	if err := SyncParent(path); err != nil {
		t.Fatal(err)
	}
	if len(synced) != 1 || synced[0] != dir {
		t.Fatalf("SyncParent of a plain file fsynced %v, want exactly [%s]", synced, dir)
	}
}

func TestOpenInAMissingDirectoryFails(t *testing.T) {
	if _, err := Open(filepath.Join(t.TempDir(), "missing", "f")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("Open in a missing directory gave %v, want os.ErrNotExist", err)
	}
}

func TestSyncParentResolvesARelativeTargetFromTheLinksDirectory(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "sub")
	if err := os.Mkdir(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "f"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink("sub/f", link); err != nil {
		t.Fatal(err)
	}
	orig := syncFile
	defer func() { syncFile = orig }()
	var synced []string
	syncFile = func(f *os.File) error {
		synced = append(synced, f.Name())
		return nil
	}
	if err := SyncParent(link); err != nil {
		t.Fatalf("SyncParent through a relative symlink: %v", err)
	}
	if len(synced) != 1 || synced[0] != filepath.Join(dir, "sub") {
		t.Fatalf("SyncParent fsynced %v, want exactly [%s]: a relative target resolves from the link's own directory", synced, sub)
	}
}

func TestSyncParentFollowsExactlyMaxLinksSymlinksAndNoMore(t *testing.T) {
	for _, n := range []int{maxLinks, maxLinks + 1} {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "f"), nil, 0o644); err != nil {
			t.Fatal(err)
		}
		target := "f"
		for i := n; i >= 1; i-- {
			name := "l" + strconv.Itoa(i)
			if err := os.Symlink(target, filepath.Join(dir, name)); err != nil {
				t.Fatal(err)
			}
			target = name
		}
		err := SyncParent(filepath.Join(dir, "l1"))
		if n <= maxLinks && err != nil {
			t.Errorf("a chain of %d symlinks is within the limit of %d but gave %v", n, maxLinks, err)
		}
		if n > maxLinks && !errors.Is(err, syscall.ELOOP) {
			t.Errorf("a chain of %d symlinks is past the limit of %d but gave %v, want ELOOP", n, maxLinks, err)
		}
	}
}
