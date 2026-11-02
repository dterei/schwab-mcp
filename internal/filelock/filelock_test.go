package filelock

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// TestMain lets the test binary hold a lock as another process, when a test
// runs it with FILELOCK_TEST_HOLD set to the lock file. It says "locked" once
// it has the lock and keeps it until its stdin closes. `go test` never sets
// the variable.
func TestMain(m *testing.M) {
	if path := os.Getenv("FILELOCK_TEST_HOLD"); path != "" {
		l, err := Acquire(context.Background(), path, time.Minute)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Println("locked")
		_, _ = io.Copy(io.Discard, os.Stdin)
		l.Release()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// holdElsewhere has another process take the lock on path, and returns a
// function that makes it let go.
func holdElsewhere(t *testing.T, path string) (release func()) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), "FILELOCK_TEST_HOLD="+path)
	cmd.Stderr = os.Stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if line, err := bufio.NewReader(stdout).ReadString('\n'); err != nil || line != "locked\n" {
		t.Fatalf("the other process said %q, %v", line, err)
	}
	return func() {
		stdin.Close()
		if err := cmd.Wait(); err != nil {
			t.Errorf("the other process: %v", err)
		}
	}
}

func skipUnsupported(t *testing.T) {
	t.Helper()
	if !Supported {
		t.Skip("no file locks on " + runtime.GOOS)
	}
}

func TestLockKeepsOutOtherOpens(t *testing.T) {
	skipUnsupported(t)
	path := filepath.Join(t.TempDir(), "sub", "x.lock")
	l, ok, err := TryAcquire(path)
	if err != nil || !ok {
		t.Fatalf("TryAcquire on a new file: %v, %v", ok, err)
	}
	if runtime.GOOS != "windows" {
		for p, want := range map[string]os.FileMode{path: 0o600, filepath.Dir(path): 0o700} {
			if info, err := os.Stat(p); err != nil || info.Mode().Perm() != want {
				t.Errorf("%s: %v, %v; want mode %v", p, info.Mode().Perm(), err, want)
			}
		}
	}
	// Another open in this process cannot take it.
	if _, ok, err := TryAcquire(path); err != nil || ok {
		t.Errorf("a second TryAcquire in the same process: %v, %v; want false", ok, err)
	}
	l.Release()
	l2, ok, err := TryAcquire(path)
	if err != nil || !ok {
		t.Fatalf("TryAcquire after Release: %v, %v", ok, err)
	}
	l2.Release()
}

func TestLockKeepsOutOtherProcesses(t *testing.T) {
	skipUnsupported(t)
	path := filepath.Join(t.TempDir(), "x.lock")
	release := holdElsewhere(t, path)
	if _, ok, err := TryAcquire(path); err != nil || ok {
		t.Errorf("TryAcquire while another process holds it: %v, %v; want false", ok, err)
	}

	start := time.Now()
	if _, err := Acquire(t.Context(), path, 100*time.Millisecond); !errors.Is(err, ErrTimeout) {
		t.Errorf("Acquire past its wait: %v, want ErrTimeout", err)
	} else if waited := time.Since(start); waited < 100*time.Millisecond || waited > 5*time.Second {
		t.Errorf("Acquire gave up after %v, want about 100ms", waited)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	if _, err := Acquire(ctx, path, time.Minute); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Acquire past the context's deadline: %v, want context.DeadlineExceeded", err)
	}

	// Once the other process lets go, a waiting Acquire gets the lock.
	got := make(chan error, 1)
	go func() {
		l, err := Acquire(t.Context(), path, time.Minute)
		if err == nil {
			l.Release()
		}
		got <- err
	}()
	time.Sleep(50 * time.Millisecond)
	release()
	select {
	case err := <-got:
		if err != nil {
			t.Errorf("Acquire after the other process let go: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Acquire never got the lock")
	}
}

// A process that dies holding the lock leaves nothing behind.
func TestLockOfADeadProcessIsFree(t *testing.T) {
	skipUnsupported(t)
	path := filepath.Join(t.TempDir(), "x.lock")
	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), "FILELOCK_TEST_HOLD="+path)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cmd.StdinPipe(); err != nil { // never closed: the process holds on until killed
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if line, err := bufio.NewReader(stdout).ReadString('\n'); err != nil || line != "locked\n" {
		t.Fatalf("the other process said %q, %v", line, err)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	l, err := Acquire(t.Context(), path, 10*time.Second)
	if err != nil {
		t.Fatalf("the lock of a killed process: %v", err)
	}
	l.Release()
}

// A lock file removed while a process waits on it, and created again by
// another, does not let two processes in at once.
func TestLockFileReplaced(t *testing.T) {
	skipUnsupported(t)
	if runtime.GOOS == "windows" {
		t.Skip("Windows does not let a file that is open be removed")
	}
	path := filepath.Join(t.TempDir(), "x.lock")
	first, err := Acquire(t.Context(), path, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	// The waiter's TryAcquire keeps opening the old file until it is gone.
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	third, err := Acquire(t.Context(), path, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	// The first holder still has the old file locked, and now the only way
	// to reach it is through f: locking f must not count.
	if ok, err := tryLock(f); err != nil || ok {
		t.Fatalf("locked the old file through another open: %v, %v", ok, err)
	}
	first.Release()
	if ok, err := tryLock(f); err != nil || !ok {
		t.Fatalf("the old file, once free: %v, %v", ok, err)
	}
	unlock(f)
	if sameFile(f, path) {
		t.Error("the old file still counts as the lock file")
	}
	if _, ok, err := TryAcquire(path); err != nil || ok {
		t.Errorf("TryAcquire while the new file is held: %v, %v; want false", ok, err)
	}
	third.Release()
}

// A lock file that this user cannot open, such as one that root made, gets
// an error that says what to do.
func TestLockFileOfAnotherUser(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs Unix permissions, and a user that they apply to")
	}
	path := filepath.Join(t.TempDir(), "x.lock")
	if err := os.WriteFile(path, nil, 0o000); err != nil {
		t.Fatal(err)
	}
	_, ok, err := TryAcquire(path)
	if ok || !errors.Is(err, fs.ErrPermission) || !strings.Contains(err.Error(), "remove it while nothing is using it") {
		t.Errorf("TryAcquire on a lock file this user cannot open: %v, %v; want a permission error that says what to do", ok, err)
	}
}
