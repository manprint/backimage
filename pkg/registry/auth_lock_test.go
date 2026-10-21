package registry

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"testing"
)

// storeHelperEnv makes this test binary act as a separate `backimage login`:
// it stores accounts into the file the variable names.
const storeHelperEnv = "BACKIMAGE_TEST_STORE_HELPER"

// Put and Delete read the file, change it and rename a new copy over it. Two
// concurrent logins each renamed their copy over the other's, and one
// account was lost without an error.
func TestConcurrentLoginsKeepEveryAccount(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.json")
	const writers, each = 16, 8
	var wg sync.WaitGroup
	errs := make(chan error, writers*each)
	for w := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s, err := NewStore(path)
			if err != nil {
				errs <- err
				return
			}
			for i := range each {
				errs <- s.Put(Credentials{Registry: "registry.test", Username: fmt.Sprintf("user-%d-%d", w, i), Secret: "pw"})
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	assertAccounts(t, path, writers*each)
}

// The same race between processes, which is what two logins in two
// terminals are.
func TestConcurrentLoginProcessesKeepEveryAccount(t *testing.T) {
	if path := os.Getenv(storeHelperEnv); path != "" {
		s, err := NewStore(path)
		if err != nil {
			t.Fatal(err)
		}
		id := os.Getenv(storeHelperEnv + "_ID")
		for i := range 10 {
			if err := s.Put(Credentials{Registry: "registry.test", Username: "proc-" + id + "-" + strconv.Itoa(i), Secret: "pw"}); err != nil {
				t.Fatal(err)
			}
		}
		return
	}
	path := filepath.Join(t.TempDir(), "auth.json")
	const procs = 6
	cmds := make([]*exec.Cmd, procs)
	for p := range procs {
		cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestConcurrentLoginProcessesKeepEveryAccount$")
		cmd.Env = append(os.Environ(), storeHelperEnv+"="+path, storeHelperEnv+"_ID="+strconv.Itoa(p))
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		cmds[p] = cmd
	}
	for _, cmd := range cmds {
		if err := cmd.Wait(); err != nil {
			t.Fatalf("store helper: %v", err)
		}
	}
	assertAccounts(t, path, procs*10)
}

func assertAccounts(t *testing.T, path string, want int) {
	t.Helper()
	s, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	accounts, err := s.Accounts()
	if err != nil {
		t.Fatal(err)
	}
	if len(accounts) != want {
		t.Fatalf("store holds %d accounts after %d concurrent logins", len(accounts), want)
	}
	// Windows has no mode bits to check: os.Stat reports 0666 for any
	// writable file.
	if runtime.GOOS == "windows" {
		return
	}
	if st, err := os.Stat(path + ".lock"); err == nil && st.Mode().Perm()&0o77 != 0 {
		t.Fatalf("lock file mode %04o, want no group/other access", st.Mode().Perm())
	}
}
