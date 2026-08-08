package sessionruntime

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/windshare/windshare/core/internal/testoutputroot"
	"github.com/windshare/windshare/core/osfs"
	"github.com/windshare/windshare/core/transfer"
)

func TestCompositeRuntimeTransferJobPublishesDurableFilesystemOutput(t *testing.T) {
	fixture := newVerticalFixture(t)
	close(fixture.scanGate)
	sender, receiver := connectVerticalPair(t, fixture.senderFactory, fixture.receiverFactory)
	defer sender.Close()
	defer receiver.Close()
	rules, err := transfer.NewSelectionRules(true, nil)
	if err != nil {
		t.Fatal(err)
	}
	outputFixture := testoutputroot.New(t)
	outputRoot := outputFixture.RootPath
	authority, err := osfs.NewFilesystemOutputAuthority(osfs.FilesystemOutputAuthorityConfig{
		RootPath: outputRoot, CreateRoot: outputFixture.CreateRoot,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, statErr := os.Stat(outputRoot); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("output authority created its root before terminal selection: %v", statErr)
	}
	intent, err := transfer.NewPathTransferIntent(
		receiver.Descriptor().ShareInstance(), receiver.Descriptor().SyntheticRoot(),
		rules, outputRoot, transfer.NativeFilesystemOutputBackendID, transfer.OutputNativeTree,
	)
	if err != nil {
		t.Fatal(err)
	}
	jobID, err := transfer.NewTransferJobID()
	if err != nil {
		t.Fatal(err)
	}
	job, err := receiver.NewTransferJob(intent, jobID, authority, nil)
	if err != nil {
		t.Fatal(err)
	}
	result := job.Run(context.Background())
	if result.Outcome != transfer.JobSucceeded || result.Settlement.Kind() != transfer.JobClosed ||
		result.SucceededFiles != 1 || result.TerminationCause != nil ||
		result.TransferJobID != jobID || result.IntentDigest != intent.Digest() || result.TransferIntent.IsZero() {
		t.Fatalf("transfer result = %+v", result)
	}
	written, err := os.ReadFile(filepath.Join(outputRoot, "folder", "file.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(written, fixture.fileData) {
		t.Fatal("durably published file changed bytes")
	}
	if fixture.contentStore.blockReads.Load() != 3 {
		t.Fatalf("durable job block reads = %d", fixture.contentStore.blockReads.Load())
	}
}
