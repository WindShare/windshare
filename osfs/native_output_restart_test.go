//go:build windows || linux || darwin

package osfs

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/osfs/internal/outputcap"
)

const (
	nativeTransferChild  = "WINDSHARE_OUTPUT_TRANSFER_CHILD"
	nativeTransferIntent = "WINDSHARE_OUTPUT_TRANSFER_INTENT"
	nativeTransferSeed   = 0x31
)

func TestNativeOutputProcessRestartKeepsRanges(t *testing.T) {
	ctx := context.Background()
	if os.Getenv(nativeTransferChild) == "1" {
		root := os.Getenv(nativeOutputCrashRootEnvironment)
		output, operation, intent := openPublicOrdinaryOperation(t, root, nativeTransferSeed)
		session, err := output.OpenOperation(ctx, operation)
		if err != nil {
			t.Fatal(err)
		}
		admission := admitRetainedCandidateRoot(t, session, intent, nativeTransferSeed+1)
		file := nativeDirectTreeTestFile(t, session, intent, nativeTransferSeed+2, "resume.bin", 8, admission)
		start, err := session.BeginFile(ctx, file)
		if err != nil {
			t.Fatal(err)
		}
		transaction, _, ok := start.Transaction()
		if !ok {
			t.Fatal("initial file did not start")
		}
		if err := transaction.WriteRange(ctx, 0, []byte("part")); err != nil {
			t.Fatal(err)
		}
		if _, err := transaction.Checkpoint(ctx); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(os.Getenv(nativeTransferIntent), intent.CanonicalBytes(), 0o600); err != nil {
			t.Fatal(err)
		}
		signalNativeOutputCrashCut(t, os.Getenv(nativeOutputCrashReadyEnvironment))
		time.Sleep(nativeOutputCrashChildMaximumWait)
		t.Fatal("parent did not kill child")
	}
	base := t.TempDir()
	root := filepath.Join(base, "output")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	preflight, err := openNativeOutputPlatform(root, false)
	if err != nil {
		t.Fatal(err)
	}
	source, ok := preflight.(interface {
		DestinationCapabilities() (outputcap.DestinationCapabilities, error)
	})
	if !ok {
		t.Fatal("missing destination capabilities")
	}
	caps, err := source.DestinationCapabilities()
	_ = preflight.Close()
	if err != nil {
		t.Fatal(err)
	}
	if mode, err := outputcap.SelectExecutionMode(caps); err != nil || mode != outputcap.ExecutionResumable {
		t.Skipf("destination lacks restart evidence: %v", err)
	}
	ready := filepath.Join(base, "ready")
	intentPath := filepath.Join(base, "intent")
	killNativeOutputChildAfterReady(t, ready, []string{
		nativeTransferChild + "=1", nativeOutputCrashRootEnvironment + "=" + root,
		nativeOutputCrashReadyEnvironment + "=" + ready, nativeTransferIntent + "=" + intentPath,
	})
	encoded, err := os.ReadFile(intentPath)
	if err != nil {
		t.Fatal(err)
	}
	intent, err := transfer.DecodeReceiveIntent(encoded)
	if err != nil {
		t.Fatal(err)
	}
	output, err := NewFilesystemOutputAuthority(FilesystemOutputAuthorityConfig{RootPath: root})
	if err != nil {
		t.Fatal(err)
	}
	defer output.Close()
	if _, err := output.BindDestination(ctx); err != nil {
		t.Fatal(err)
	}
	lookup, err := output.LookupActive(ctx, intent.SelectionSpec())
	if err != nil || lookup.Kind() != FilesystemOutputLookupReopened {
		t.Fatalf("lookup=%v %v", lookup.Kind(), err)
	}
	session, err := output.OpenOperation(ctx, lookup.Operation())
	if err != nil {
		t.Fatal(err)
	}
	admission := admitRetainedCandidateRoot(t, session, intent, nativeTransferSeed+1)
	file := nativeDirectTreeTestFile(t, session, intent, nativeTransferSeed+2, "resume.bin", 8, admission)
	start, err := session.BeginFile(ctx, file)
	if err != nil {
		t.Fatal(err)
	}
	transaction, progress, ok := start.Transaction()
	if !ok || len(progress.Ranges().Ranges()) != 1 || progress.Ranges().Ranges()[0].End != 4 {
		t.Fatalf("restart lost authenticated progress: %+v", progress.Ranges().Ranges())
	}
	if err := transaction.WriteRange(ctx, 4, []byte("done")); err != nil {
		t.Fatal(err)
	}
	if settled, err := transaction.Commit(ctx); err != nil || settled.Kind() != transfer.FilePublished {
		t.Fatalf("commit=%v %v", settled.Kind(), err)
	}
	if _, err := session.FinalizeDirectory(ctx, admission); err != nil {
		t.Fatal(err)
	}
	if _, err := session.FinalizeTree(ctx, transfer.DirectTreeOutcomeSuccess); err != nil {
		t.Fatal(err)
	}
	final := filepath.Join(nativeDirectTreeResultRoot(t, root, intent), "resume.bin")
	data, err := os.ReadFile(final)
	if err != nil || string(data) != "partdone" {
		t.Fatalf("resumed content=%q %v", data, err)
	}
}
