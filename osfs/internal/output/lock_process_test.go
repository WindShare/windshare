//go:build windows || linux || darwin

package output

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

const (
	taskLockChildRoot      = "WINDSHARE_OUTPUT_LOCK_CHILD_ROOT"
	taskLockTestName       = "task.lock"
	taskLockReadyName      = "child.ready"
	taskLockProcessTimeout = 20 * time.Second
	taskLockPollInterval   = 10 * time.Millisecond
)

// A same-process lock test cannot prove that a crashed receiver releases the
// kernel lock while preserving the file needed by its replacement process.
func TestTaskLockIsProcessExclusiveAndRecoversAfterExit(t *testing.T) {
	if path := os.Getenv(taskLockChildRoot); path != "" {
		root, _, err := OpenContainer(path, false)
		if err != nil {
			t.Fatal(err)
		}
		defer root.Close()
		held, _, err := acquireLockFile(root, taskLockTestName, false, false)
		if err != nil {
			t.Fatal(err)
		}
		defer held.Close()
		if err := os.WriteFile(filepath.Join(path, taskLockReadyName), []byte("ready"), 0o600); err != nil {
			t.Fatal(err)
		}
		time.Sleep(taskLockProcessTimeout)
		t.Fatal("parent did not terminate lock holder")
	}

	path := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), taskLockProcessTimeout)
	defer cancel()
	child := exec.CommandContext(ctx, os.Args[0], "-test.run=^"+t.Name()+"$", "-test.count=1")
	child.Env = append(os.Environ(), taskLockChildRoot+"="+path)
	// Release fixtures may run without device nodes in a filesystem chroot.
	child.Stdin = bytes.NewReader(nil)
	var out bytes.Buffer
	child.Stdout, child.Stderr = &out, &out
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- child.Wait() }()
	waited := false
	defer func() {
		if !waited {
			_ = child.Process.Kill()
			<-done
		}
	}()
	ticker := time.NewTicker(taskLockPollInterval)
	defer ticker.Stop()
ready:
	for {
		select {
		case err := <-done:
			waited = true
			t.Fatalf("lock holder exited before readiness: %v\n%s", err, out.String())
		case <-ctx.Done():
			t.Fatal("lock holder did not become ready")
		case <-ticker.C:
			if _, err := os.Stat(filepath.Join(path, taskLockReadyName)); err == nil {
				break ready
			} else if !errors.Is(err, os.ErrNotExist) {
				t.Fatal(err)
			}
		}
	}
	root, _, err := OpenContainer(path, false)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if held, _, err := acquireLockFile(root, taskLockTestName, true, false); !errors.Is(err, ErrNamespaceLockBusy) {
		if held != nil {
			_ = held.Close()
		}
		t.Fatalf("another process acquired the task lock: %v", err)
	}
	if err := child.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	err = <-done
	waited = true
	if err == nil {
		t.Fatal("lock holder exited normally")
	}
	recovered, created, err := acquireLockFile(root, taskLockTestName, true, false)
	if err != nil {
		t.Fatalf("process exit left the task locked: %v", err)
	}
	defer recovered.Close()
	if created {
		t.Fatal("recovery replaced the existing lock file")
	}
}
