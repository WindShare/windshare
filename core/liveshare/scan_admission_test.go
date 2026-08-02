package liveshare

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/windshare/windshare/core/catalog"
)

type identityPreservingDirectoryScanner struct {
	calls uint32
}

func (scanner *identityPreservingDirectoryScanner) ScanDirectory(
	context.Context,
	catalog.ScanRequest,
) (catalog.ScanResult, error) {
	scanner.calls++
	return catalog.ScanResult{OmittedCount: 3}, nil
}

func TestDirectoryScannerWithNilAdmissionPreservesProductionScanner(t *testing.T) {
	scanner := &identityPreservingDirectoryScanner{}
	selected := directoryScannerWithAdmission(scanner, nil)
	if selected != scanner {
		t.Fatalf("nil admission wrapped production scanner as %T", selected)
	}
	result, err := selected.ScanDirectory(context.Background(), catalog.ScanRequest{})
	if err != nil || result.OmittedCount != 3 || scanner.calls != 1 {
		t.Fatalf("production scan result=%#v calls=%d err=%v", result, scanner.calls, err)
	}
}

func TestDirectoryScanAdmissionIsDecisiveBeforeScannerInvocation(t *testing.T) {
	release := make(chan struct{})
	reached := make(chan struct{})
	var scannerCalls atomic.Uint32
	scanner := catalog.DirectoryScannerFunc(func(
		context.Context,
		catalog.ScanRequest,
	) (catalog.ScanResult, error) {
		scannerCalls.Add(1)
		return catalog.ScanResult{OmittedCount: 7}, nil
	})
	admission := DirectoryScanAdmissionFunc(func(ctx context.Context, _ catalog.ScanRequest) error {
		close(reached)
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return context.Cause(ctx)
		}
	})
	gated := directoryScannerWithAdmission(scanner, admission)
	result := make(chan catalog.ScanResult, 1)
	failure := make(chan error, 1)
	go func() {
		observed, err := gated.ScanDirectory(context.Background(), catalog.ScanRequest{})
		result <- observed
		failure <- err
	}()
	<-reached
	if calls := scannerCalls.Load(); calls != 0 {
		t.Fatalf("scanner ran before admission release: calls=%d", calls)
	}
	close(release)
	if err := <-failure; err != nil {
		t.Fatal(err)
	}
	if observed := <-result; observed.OmittedCount != 7 || scannerCalls.Load() != 1 {
		t.Fatalf("released scan result=%#v calls=%d", observed, scannerCalls.Load())
	}
}

func TestDirectoryScanAdmissionFailureNeverReachesScanner(t *testing.T) {
	want := errors.New("admission rejected")
	var scannerCalls atomic.Uint32
	gated := directoryScannerWithAdmission(
		catalog.DirectoryScannerFunc(func(context.Context, catalog.ScanRequest) (catalog.ScanResult, error) {
			scannerCalls.Add(1)
			return catalog.ScanResult{}, nil
		}),
		DirectoryScanAdmissionFunc(func(context.Context, catalog.ScanRequest) error { return want }),
	)
	if _, err := gated.ScanDirectory(context.Background(), catalog.ScanRequest{}); !errors.Is(err, want) {
		t.Fatalf("scan error=%v, want %v", err, want)
	}
	if calls := scannerCalls.Load(); calls != 0 {
		t.Fatalf("scanner ran after admission failure: calls=%d", calls)
	}
}
