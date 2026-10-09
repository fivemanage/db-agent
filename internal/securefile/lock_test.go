package securefile

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestLockHelperProcess(t *testing.T) {
	mode := os.Getenv("DB_AGENT_LOCK_HELPER")
	if mode == "" {
		return
	}
	unlock, err := TryLock(os.Getenv("DB_AGENT_LOCK_PATH"))
	if mode == "blocked" {
		if !errors.Is(err, ErrLocked) {
			os.Exit(2)
		}
		os.Exit(0)
	}
	if err != nil {
		os.Exit(3)
	}
	defer unlock()
	fmt.Fprintln(os.Stdout, "locked")
	// The parent terminates this child, testing automatic OS lock release.
	for {
		time.Sleep(time.Hour)
	}
}

func TestLockExcludesProcessesAndReleasesAfterCrash(t *testing.T) {
	path := filepath.Join(t.TempDir(), "run.lock")
	unlock, err := TryLock(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	command := func(mode string) *exec.Cmd {
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestLockHelperProcess$")
		cmd.Env = append(os.Environ(), "DB_AGENT_LOCK_HELPER="+mode, "DB_AGENT_LOCK_PATH="+path)
		return cmd
	}
	blocked := command("blocked")
	err = blocked.Run()
	unlock()
	if err != nil {
		t.Fatalf("second process obtained the lock: %v", err)
	}

	child := command("hold")
	stdout, err := child.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil || line != "locked\n" {
		child.Process.Kill()
		child.Wait()
		t.Fatalf("child didn't obtain lock: %v", err)
	}
	otherUnlock, err := TryLock(path)
	if err == nil {
		otherUnlock()
	}
	child.Process.Kill()
	child.Wait()
	if !errors.Is(err, ErrLocked) {
		t.Fatalf("parent overlapped child: %v", err)
	}
	unlock, err = TryLock(path)
	if err != nil {
		t.Fatalf("crashed process left a stale lock: %v", err)
	}
	unlock()
}
