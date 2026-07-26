//go:build windows

package osfs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/windshare/windshare/core/osfs/internal/resumestate"
)

func TestWindowsV3FreshSelectedDirectoryMaterializationRetainsPlacementGuard(t *testing.T) {
	base := windowsV3NativeTestTempDir(t)
	externalPath := filepath.Join(base, "external")
	rootPath := filepath.Join(externalPath, "output")
	rootDecoyPath := filepath.Join(externalPath, "output-decoy")
	externalDecoyRootPath := filepath.Join(base, "external-decoy", "output")
	for _, path := range []string{rootPath, rootDecoyPath, externalDecoyRootPath} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}

	gate := newWindowsV3FreshAdmissionGuardGate()
	t.Cleanup(gate.unblock)
	var guardedPlatform *windowsV3FreshAdmissionHoldPlatform
	authority := v3RecoveryAuthority(t, rootPath, nil)
	authority.platformFactory = func(path string, create bool) (outputV3Platform, error) {
		platform, err := openOutputV3Platform(path, create)
		if err != nil {
			return nil, err
		}
		guardedPlatform = &windowsV3FreshAdmissionHoldPlatform{
			outputV3Platform: platform,
			gate:             gate,
		}
		return guardedPlatform, nil
	}
	selection := outputV3DirectoryAuthoritySelection(t, "scoped")
	type openResult struct {
		opened v3OpenedSelection
		err    error
	}
	result := make(chan openResult, 1)
	go func() {
		opened, err := v3OpenSelection(context.Background(), authority, selection)
		result <- openResult{opened: opened, err: err}
	}()

	select {
	case <-gate.entered:
	case <-time.After(windowsV3OperationHoldTimeout):
		gate.unblock()
		t.Fatal("fresh admission did not reach the guarded pre-materialization cut")
	}
	if guardedPlatform == nil {
		t.Error("guarded platform was not installed before the admission cut")
	} else if guardedPlatform.guardAcquires.Load() != 1 || guardedPlatform.guardCloses.Load() != 0 {
		t.Errorf(
			"pre-materialization guard lifecycle = (platform=%v, acquires=%d, closes=%d)",
			guardedPlatform,
			guardedPlatform.guardAcquires.Load(),
			guardedPlatform.guardCloses.Load(),
		)
	}

	rootMovedPath := filepath.Join(externalPath, "output-displaced")
	externalMovedPath := filepath.Join(base, "external-displaced")
	rootMoveErr := windowsResumeAttemptPinnedMove(rootPath, rootMovedPath)
	externalMoveErr := windowsResumeAttemptPinnedMove(externalPath, externalMovedPath)
	if !v3RecoveryIsBlockedAncestorReplacement(rootMoveErr) {
		t.Errorf("output-root displacement during fresh admission = %v, want placement denial", rootMoveErr)
	}
	if !v3RecoveryIsBlockedAncestorReplacement(externalMoveErr) {
		t.Errorf("external-parent displacement during fresh admission = %v, want placement denial", externalMoveErr)
	}
	for _, candidate := range []string{rootPath, rootDecoyPath, externalDecoyRootPath} {
		if _, err := os.Stat(filepath.Join(candidate, "scoped")); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("pre-materialization selected directory under %q = %v", candidate, err)
		}
	}
	if _, err := os.Stat(filepath.Join(rootPath, resumestate.ControlDirectoryName)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("pre-materialization admission wrote resume state: %v", err)
	}

	gate.unblock()
	var completed openResult
	select {
	case completed = <-result:
	case <-time.After(windowsV3OperationHoldTimeout):
		t.Fatal("fresh admission did not finish after releasing the placement guard")
	}
	if completed.err != nil || completed.opened.Session == nil {
		t.Fatalf("guarded fresh admission = (%+v, %v), want created session", completed.opened, completed.err)
	}
	if guardedPlatform.guardAcquires.Load() != 1 || guardedPlatform.guardCloses.Load() != 1 {
		t.Fatalf(
			"fresh admission guard lifecycle = (%d acquires, %d closes), want (1, 1)",
			guardedPlatform.guardAcquires.Load(), guardedPlatform.guardCloses.Load(),
		)
	}
	if info, err := os.Stat(filepath.Join(rootPath, "scoped")); err != nil || !info.IsDir() {
		t.Fatalf("canonical selected directory = (%v, %v)", info, err)
	}
	for _, decoy := range []string{rootDecoyPath, externalDecoyRootPath} {
		if _, err := os.Stat(filepath.Join(decoy, "scoped")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("selected directory escaped into decoy root %q: %v", decoy, err)
		}
	}
	v3RecoveryCloseSession(t, completed.opened.Session)

	if err := os.Rename(rootPath, rootMovedPath); err != nil {
		t.Fatalf("output-root displacement remained blocked after guard cleanup: %v", err)
	}
	if err := os.Rename(rootMovedPath, rootPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(externalPath, externalMovedPath); err != nil {
		t.Fatalf("external-parent displacement remained blocked after guard cleanup: %v", err)
	}
	if err := os.Rename(externalMovedPath, externalPath); err != nil {
		t.Fatal(err)
	}
}

type windowsV3FreshAdmissionGuardGate struct {
	entered     chan struct{}
	release     chan struct{}
	enterOnce   sync.Once
	releaseOnce sync.Once
}

func newWindowsV3FreshAdmissionGuardGate() *windowsV3FreshAdmissionGuardGate {
	return &windowsV3FreshAdmissionGuardGate{
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
}

func (gate *windowsV3FreshAdmissionGuardGate) hold() {
	gate.enterOnce.Do(func() { close(gate.entered) })
	<-gate.release
}

func (gate *windowsV3FreshAdmissionGuardGate) unblock() {
	gate.releaseOnce.Do(func() { close(gate.release) })
}

type windowsV3FreshAdmissionHoldPlatform struct {
	outputV3Platform
	gate          *windowsV3FreshAdmissionGuardGate
	guardAcquires atomic.Uint32
	guardCloses   atomic.Uint32
}

func (platform *windowsV3FreshAdmissionHoldPlatform) AcquirePublicOperationGuard() (
	outputV3PublicOperationGuard,
	error,
) {
	guard, err := platform.outputV3Platform.AcquirePublicOperationGuard()
	if err != nil {
		return nil, err
	}
	platform.guardAcquires.Add(1)
	platform.gate.hold()
	return &windowsV3FreshAdmissionCountedGuard{
		outputV3PublicOperationGuard: guard,
		closes:                       &platform.guardCloses,
	}, nil
}

type windowsV3FreshAdmissionCountedGuard struct {
	outputV3PublicOperationGuard
	closes *atomic.Uint32
}

func (guard *windowsV3FreshAdmissionCountedGuard) Close() error {
	if guard == nil || guard.outputV3PublicOperationGuard == nil {
		return nil
	}
	err := guard.outputV3PublicOperationGuard.Close()
	guard.outputV3PublicOperationGuard = nil
	guard.closes.Add(1)
	return err
}
