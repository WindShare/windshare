package receiversession

import (
	"bytes"
	"context"
	"errors"
	"github.com/windshare/windshare/core/internal/testoutputroot"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/receivecontract"
	"github.com/windshare/windshare/osfs"
	"os"
	"path/filepath"
	"testing"
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
	authority, err := osfs.NewOutputAuthority(osfs.OutputConfig{
		RootPath: outputRoot, CreateRoot: outputFixture.CreateRoot,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := authority.Close(); err != nil {
			t.Error(err)
		}
	})
	if _, statErr := os.Stat(outputRoot); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("output authority created its root before terminal selection: %v", statErr)
	}
	selection, err := transfer.NewSelectionSpec(
		receiver.Descriptor().ShareInstance(), receiver.Descriptor().SyntheticRoot(),
		rules,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := authority.BindDestination(context.Background()); err != nil {
		t.Fatal(err)
	}
	lookup, err := authority.SelectOperation(context.Background(), selection, receivecontract.AutomaticOperation())
	if err != nil || lookup.Kind() != osfs.OutputLookupCreateReady {
		t.Fatalf("native lookup = (%d, %v)", lookup.Kind(), err)
	}
	artifact, err := receivecontract.NewResultRootDirectoryTree(receivecontract.NewSyntheticSelectionResultRoot())
	if err != nil {
		t.Fatal(err)
	}
	operation, err := lookup.Create(context.Background(), artifact)
	if err != nil {
		t.Fatal(err)
	}
	intent, ok := operation.ReceiveIntent()
	if !ok {
		t.Fatal("native operation omitted receive intent")
	}
	jobID, err := transfer.NewTransferJobID()
	if err != nil {
		t.Fatal(err)
	}
	job, err := receiver.NewTransferJob(intent, jobID, operation, nil)
	if err != nil {
		t.Fatal(err)
	}
	result := job.Run(context.Background())
	if result.Outcome != transfer.DirectTreeOutcomeSuccess ||
		result.Settlement.Kind() != transfer.DirectTreeSettlementSuccess ||
		result.SucceededFiles != 1 || result.TerminationCause != nil ||
		result.TransferJobID != jobID || result.ReceiveIntentDigest != intent.Digest() || result.ReceiveIntent.IsZero() {
		t.Fatalf("transfer result = %+v", result)
	}
	destination, _ := intent.MaterializationPlan().DestinationReservation()
	written, err := os.ReadFile(filepath.Join(outputRoot, destination.PhysicalName(), "folder", "file.bin"))
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
