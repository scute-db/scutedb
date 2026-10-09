package pagestore

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/scute-db/scutedb/internal/page"
)

const killNineChildEnv = "SCUTEDB_PAGESTORE_KILL_CHILD"

func TestKillNineChildLoopsUntilKilled(t *testing.T) {
	path := os.Getenv(killNineChildEnv)
	if path == "" {
		t.Skip("runs only as the child process of TestARealKillNineAtAnyMomentAlwaysRecovers")
	}
	s, err := Create(path)
	if err != nil {
		os.Exit(3)
	}
	fmt.Println(s.Durable().Checkpoint)
	for {
		p, err := s.Allocate(page.KindBTreeLeaf)
		if err != nil {
			os.Exit(3)
		}
		binary.BigEndian.PutUint32(p[page.HeaderSize:], s.Durable().Checkpoint+1)
		if err := s.Write(p); err != nil {
			os.Exit(3)
		}
		old := s.Root()
		if err := s.SetRoot(p.ID()); err != nil {
			os.Exit(3)
		}
		if old != NoPage {
			if err := s.Free(old); err != nil {
				os.Exit(3)
			}
		}
		if err := s.Checkpoint(); err != nil {
			os.Exit(3)
		}
		fmt.Println(s.Durable().Checkpoint)
	}
}

func TestARealKillNineAtAnyMomentAlwaysRecovers(t *testing.T) {
	if os.Getenv(killNineChildEnv) != "" {
		t.Skip("this is the child")
	}
	if testing.Short() {
		t.Skip("starts real child processes")
	}
	const runs = 12
	recovered := 0
	for run := 0; run < runs; run++ {
		path := filepath.Join(t.TempDir(), "kill.db")
		cmd := exec.Command(os.Args[0], "-test.run=^TestKillNineChildLoopsUntilKilled$", "-test.count=1")
		cmd.Env = append(os.Environ(), killNineChildEnv+"="+path)
		out, err := cmd.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		told := make(chan uint32, 1)
		go func() {
			last := uint32(0)
			sc := bufio.NewScanner(out)
			for sc.Scan() {
				if v, err := strconv.ParseUint(sc.Text(), 10, 32); err == nil {
					last = uint32(v)
				}
			}
			told <- last
		}()
		time.Sleep(time.Duration(15+run*11) * time.Millisecond)
		if err := cmd.Process.Kill(); err != nil {
			t.Fatal(err)
		}
		cmd.Wait()
		last := <-told

		s, err := Open(path)
		if errors.Is(err, fs.ErrNotExist) {
			if last != 0 {
				t.Fatalf("run %d: the child reported checkpoint %d, but the file does not exist", run, last)
			}
			continue
		}
		if err != nil {
			t.Fatalf("run %d: after a kill -9 the file does not open: %v", run, err)
		}
		if err := s.Verify(); err != nil {
			t.Fatalf("run %d: after a kill -9 the file does not verify: %v", run, err)
		}
		gen := s.Durable().Checkpoint
		if gen < last || gen > last+1 {
			t.Fatalf("run %d: the child was told checkpoint %d was durable, the file says %d; "+
				"it may be one ahead, never behind", run, last, gen)
		}
		if s.Root() != NoPage {
			p, err := s.Read(s.Root())
			if err != nil {
				t.Fatalf("run %d: reading the root: %v", run, err)
			}
			if stamp := binary.BigEndian.Uint32(p[page.HeaderSize:]); stamp != gen {
				t.Fatalf("run %d: the root holds the stamp of checkpoint %d, but the file is at checkpoint %d", run, stamp, gen)
			}
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		recovered++
	}
	t.Logf("%d of %d killed processes left a file that reopened to a consistent checkpoint; the rest died before creating one", recovered, runs)
}
