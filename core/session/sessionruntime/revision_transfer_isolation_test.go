package sessionruntime

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/session/contentflow"
	"github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/transfer"
)

type revisionTransferCommitter struct{}

func (revisionTransferCommitter) Commit(input catalog.PageCommitInput) (catalog.PageCommitment, error) {
	var commitment catalog.PageCommitment
	commitment[0] = input.DirectoryID.Bytes()[0]
	commitment[1] = byte(input.PageIndex + 1)
	return commitment, nil
}

type revisionTransferCatalog struct{ snapshot catalog.DirectorySnapshot }

func (source revisionTransferCatalog) OpenDirectoryPages(
	context.Context,
	catalog.DirectoryID,
) (catalog.DirectoryPageCursor, error) {
	return &revisionTransferCursor{snapshot: source.snapshot}, nil
}

type revisionTransferCursor struct {
	snapshot catalog.DirectorySnapshot
	index    uint32
}

func (cursor *revisionTransferCursor) Next(context.Context) (catalog.CatalogPage, bool, error) {
	page, exists := cursor.snapshot.Page(cursor.index)
	if exists {
		cursor.index++
	}
	return page, exists, nil
}
func (*revisionTransferCursor) Close() error { return nil }

type revisionTransferRevisions struct {
	opened   map[catalog.FileID]transfer.OpenedRevision
	failures map[catalog.FileID]error
}

func (source revisionTransferRevisions) OpenRevision(
	_ context.Context,
	file catalog.FileID,
) (transfer.OpenedRevision, error) {
	if err := source.failures[file]; err != nil {
		return transfer.OpenedRevision{}, err
	}
	return source.opened[file], nil
}
func (revisionTransferRevisions) ReleaseRevision(context.Context, content.LeaseID) error { return nil }

type revisionTransferRanges struct {
	failed  catalog.FileID
	failure error
}

func (source revisionTransferRanges) ReadRange(
	ctx context.Context,
	_ content.LeaseID,
	descriptor content.FileRevisionDescriptor,
	requested content.Range,
	sink transfer.RangeSink,
) error {
	if descriptor.FileID() == source.failed {
		return source.failure
	}
	return sink.WriteRange(ctx, requested.Offset, make([]byte, requested.Length()))
}

type revisionTransferOutput struct {
	backend      transfer.OutputBackendID
	session      transfer.OutputSessionID
	secret       [32]byte
	settlements  map[catalog.FileID]transfer.FileSettlementKind
	jobPauses    int
	jobCompletes int
}

func newRevisionTransferOutput(t *testing.T) *revisionTransferOutput {
	t.Helper()
	backend, err := transfer.NewOutputBackendID("test/sessionruntime-revision-isolation")
	if err != nil {
		t.Fatal(err)
	}
	return &revisionTransferOutput{
		backend: backend, session: id16[transfer.OutputSessionID](201),
		secret: [32]byte{1}, settlements: make(map[catalog.FileID]transfer.FileSettlementKind),
	}
}

func (output *revisionTransferOutput) OpenOutput(
	context.Context,
	transfer.TransferIntent,
) (transfer.OutputSession, error) {
	return output, nil
}
func (output *revisionTransferOutput) BackendID() transfer.OutputBackendID { return output.backend }
func (output *revisionTransferOutput) SessionID() transfer.OutputSessionID { return output.session }
func (*revisionTransferOutput) Capabilities() transfer.OutputCapabilities {
	capabilities, _ := transfer.NewOutputCapabilities(transfer.OutputCapabilities{
		Durability: transfer.DurabilityPowerLoss, Mode: transfer.OutputNativeTree,
		RandomWrite: true, FileFailureIsolation: true,
	})
	return capabilities
}
func (output *revisionTransferOutput) AdmitDirectory(
	_ context.Context,
	directory transfer.OutputDirectory,
) (transfer.DirectoryAdmission, error) {
	return transfer.NewDirectoryAdmissionWithSecret(output.secret[:], directory)
}
func (*revisionTransferOutput) FinalizeDirectory(context.Context, transfer.OutputDirectory) error {
	return nil
}
func (output *revisionTransferOutput) BeginFile(
	_ context.Context,
	file transfer.OutputFile,
) (transfer.FileStart, error) {
	digest := sha256.Sum256([]byte(file.Path))
	identity, err := transfer.OutputObjectIdentityFromBytes(digest[:])
	if err != nil {
		return transfer.FileStart{}, err
	}
	binding, err := transfer.BindOutputFileTarget(file.Target, identity)
	if err != nil {
		return transfer.FileStart{}, err
	}
	empty, _ := content.NewRangeSet(nil)
	checkpoint, err := transfer.VerifyDurableRanges(binding, 1, empty)
	if err != nil {
		return transfer.FileStart{}, err
	}
	transaction := &revisionTransferTransaction{
		output: output, binding: binding, checkpoint: checkpoint,
	}
	return transfer.NewFileTransactionStart(transaction, checkpoint)
}
func (output *revisionTransferOutput) PauseJob(
	context.Context,
	transfer.JobPauseReason,
) (transfer.JobSettlement, error) {
	output.jobPauses++
	return transfer.NewJobSettlement(transfer.JobPaused)
}
func (output *revisionTransferOutput) CompleteJob(
	context.Context,
	transfer.JobOutcome,
) (transfer.JobSettlement, error) {
	output.jobCompletes++
	return transfer.NewJobSettlement(transfer.JobClosed)
}

type revisionTransferTransaction struct {
	output     *revisionTransferOutput
	binding    transfer.OutputFileBinding
	checkpoint transfer.VerifiedDurableRanges
}

func (transaction *revisionTransferTransaction) Binding() transfer.OutputFileBinding {
	return transaction.binding
}
func (transaction *revisionTransferTransaction) WriteRange(
	_ context.Context,
	offset uint64,
	data []byte,
) error {
	written, err := content.NewRangeSet([]content.Range{{Offset: offset, End: offset + uint64(len(data))}})
	if err != nil {
		return err
	}
	merged, err := transfer.MergeRanges(transaction.checkpoint.Ranges(), written)
	if err != nil {
		return err
	}
	transaction.checkpoint, err = transfer.VerifyDurableRanges(
		transaction.binding, transaction.checkpoint.CheckpointGeneration()+1, merged,
	)
	return err
}
func (transaction *revisionTransferTransaction) Checkpoint(
	context.Context,
) (transfer.VerifiedDurableRanges, error) {
	return transaction.checkpoint, nil
}
func (transaction *revisionTransferTransaction) Commit(context.Context) (transfer.FileSettlement, error) {
	settlement, err := transfer.NewVerifiedFileSettlement(transfer.FilePublished, transaction.checkpoint)
	if err == nil {
		transaction.output.settlements[transaction.binding.FileID()] = settlement.Kind()
	}
	return settlement, err
}
func (transaction *revisionTransferTransaction) Pause(
	context.Context,
	transfer.FilePauseReason,
) (transfer.FileSettlement, error) {
	settlement, err := transfer.NewVerifiedFileSettlement(transfer.FilePaused, transaction.checkpoint)
	if err == nil {
		transaction.output.settlements[transaction.binding.FileID()] = settlement.Kind()
	}
	return settlement, err
}
func (transaction *revisionTransferTransaction) Retire(
	context.Context,
	transfer.FileRetireReason,
) (transfer.FileSettlement, error) {
	settlement, err := transfer.NewRetiredFileSettlement(transaction.binding)
	if err == nil {
		transaction.output.settlements[transaction.binding.FileID()] = settlement.Kind()
	}
	return settlement, err
}

func TestEveryRevisionFailureDispositionSettlesOneFileAndContinuesSibling(t *testing.T) {
	codes := []uint16{
		contentflow.RevisionCodeStale,
		contentflow.RevisionCodeNotFound,
		contentflow.RevisionCodeUnreadable,
		contentflow.RevisionCodeUnsupportedStability,
		contentflow.RevisionCodeQuota,
		contentflow.RevisionCodeLeaseExpired,
		contentflow.RevisionCodeDrift,
		contentflow.RevisionCodeInvalidLease,
	}
	for _, code := range codes {
		for _, retryable := range []bool{false, true} {
			t.Run(fmt.Sprintf("code_%04x_retryable_%t", code, retryable), func(t *testing.T) {
				failure := RemoteOperationFailureSnapshot{
					scope: protocolsession.OperationScopeRevision,
					code:  code, retryable: retryable, message: "revision operation failed",
				}
				if retryable {
					failure.retryAfter = time.Millisecond
				}
				wantSettlement := transfer.FilePaused
				if code == contentflow.RevisionCodeDrift || !retryable && permanentRevisionOperationCode(code) {
					wantSettlement = transfer.FileRetired
				}
				runContentTransferIsolationCase(
					t, failure.Scope(), code, retryable, wantSettlement, revisionOperationError(failure),
					false, code == contentflow.RevisionCodeStale || code == contentflow.RevisionCodeDrift,
				)
			})
		}
	}
}

func TestTerminalBlockOperationFailureSettlesOneFileAndContinuesSibling(t *testing.T) {
	failure := RemoteOperationFailureSnapshot{
		scope: protocolsession.OperationScopeBlock, code: contentflow.BlockCodeTimeout,
		retryable: true, retryAfter: time.Millisecond, message: "block demand exhausted its lane attempts",
	}
	runContentTransferIsolationCase(
		t, failure.Scope(), failure.Code(), failure.Retryable(), transfer.FilePaused,
		isolatedBlockOperationError(NewRemoteOperationError(failure)), false, false,
	)
}

func TestOpenResultRevisionDriftRemainsCLIVisibleAndContinuesSibling(t *testing.T) {
	fixture := newVerticalFixture(t)
	fixture.contentStore.openErr = content.ErrRevisionDrift
	sender, receiver := connectVerticalPair(t, fixture.senderFactory, fixture.receiverFactory)
	defer sender.Close()
	defer receiver.Close()
	_, failure := receiver.OpenRevision(context.Background(), fixture.fileID)
	if failure == nil {
		t.Fatal("authenticated OPEN_RESULTS drift unexpectedly opened a revision")
	}
	if _, err := receiver.RequestLane(context.Background(), 0); err != nil {
		t.Fatalf("revision-open drift damaged the protocol session: %v", err)
	}
	result := runContentTransferIsolationCase(
		t,
		protocolsession.OperationScopeRevision,
		contentflow.RevisionCodeDrift,
		false,
		0,
		failure,
		true,
		true,
	)
	if !errors.Is(result.SourceDriftFailure, content.ErrRevisionDrift) {
		t.Fatalf("CLI-visible source drift = %v", result.SourceDriftFailure)
	}
}

func runContentTransferIsolationCase(
	t *testing.T,
	scope uint8,
	code uint16,
	retryable bool,
	wantSettlement transfer.FileSettlementKind,
	failure error,
	failAtOpen bool,
	wantSourceDrift bool,
) transfer.JobResult {
	t.Helper()
	share := id16[catalog.ShareInstance](210)
	root := id16[catalog.DirectoryID](211)
	failed := id16[catalog.FileID](212)
	good := id16[catalog.FileID](213)
	size := uint64(catalog.MinChunkSize)
	entries := make([]catalog.Entry, 0, 2)
	for _, spec := range []struct {
		file catalog.FileID
		name string
	}{{failed, "a-failed.bin"}, {good, "b-good.bin"}} {
		entry, err := catalog.NewFileEntry(spec.file, spec.name, size, catalog.ModifiedTime{})
		if err != nil {
			t.Fatal(err)
		}
		entries = append(entries, entry)
	}
	page, err := catalog.NewCatalogPage(catalog.CatalogPageSpec{
		ShareInstance: share, DirectoryID: root,
		Generation: id16[catalog.DirectoryGeneration](214), Entries: entries, Terminal: true,
	}, revisionTransferCommitter{})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := catalog.NewDirectorySnapshot([]catalog.CatalogPage{page})
	if err != nil {
		t.Fatal(err)
	}
	opened := make(map[catalog.FileID]transfer.OpenedRevision)
	for index, file := range []catalog.FileID{failed, good} {
		geometry, geometryErr := content.NewFileGeometry(size, catalog.MinChunkSize)
		if geometryErr != nil {
			t.Fatal(geometryErr)
		}
		descriptor, descriptorErr := content.NewFileRevisionDescriptor(
			share, file, id16[content.FileRevision](byte(215+index)), geometry, catalog.ModifiedTime{},
		)
		if descriptorErr != nil {
			t.Fatal(descriptorErr)
		}
		opened[file], descriptorErr = transfer.NewOpenedRevision(
			id16[content.LeaseID](byte(217+index)), descriptor,
		)
		if descriptorErr != nil {
			t.Fatal(descriptorErr)
		}
	}
	rules, _ := transfer.NewSelectionRules(true, nil)
	output := newRevisionTransferOutput(t)
	targetDigest := sha256.Sum256([]byte("sessionruntime revision transfer output"))
	target, err := transfer.NewOpaqueOutputTarget(targetDigest[:])
	if err != nil {
		t.Fatal(err)
	}
	intent, err := transfer.NewTransferIntent(
		share, root, rules, target, output.BackendID(), transfer.OutputNativeTree,
	)
	if err != nil {
		t.Fatal(err)
	}
	intentDigest := intent.Digest()
	jobID, err := transfer.TransferJobIDFromBytes(intentDigest[:transfer.TransferJobIdentityBytes])
	if err != nil {
		t.Fatal(err)
	}
	revisions := revisionTransferRevisions{opened: opened, failures: make(map[catalog.FileID]error)}
	ranges := revisionTransferRanges{failed: failed, failure: failure}
	if failAtOpen {
		revisions.failures[failed] = failure
		ranges = revisionTransferRanges{}
	}
	job, err := transfer.NewTransferJob(transfer.TransferJobConfig{
		ShareInstance: share, SyntheticRoot: root, Rules: rules, Intent: intent, JobID: jobID,
		Catalog:   revisionTransferCatalog{snapshot: snapshot},
		Revisions: revisions,
		Blocks:    ranges, Output: output,
	})
	if err != nil {
		t.Fatal(err)
	}
	result := job.Run(context.Background())
	var local interface{ IsolatedFileSourceFailure() }
	if result.Outcome != transfer.JobCompletedWithErrors || result.TerminationCause != nil ||
		result.SucceededFiles != 1 || len(result.Files) != 1 ||
		output.settlements[failed] != wantSettlement || output.settlements[good] != transfer.FilePublished ||
		output.jobPauses != 0 || output.jobCompletes != 1 ||
		!errors.As(result.Files[0].Cause, &local) ||
		(result.SourceDriftFailure != nil) != wantSourceDrift {
		t.Fatalf("result=%+v settlements=%v", result, output.settlements)
	}
	if failAtOpen {
		var remote *RemoteRevisionError
		if !errors.As(result.Files[0].Cause, &remote) || remote.Failure().Code != code ||
			remote.Failure().Retryable != retryable {
			t.Fatalf("open failure lost remote revision diagnostic: %+v", result.Files[0])
		}
		return result
	}
	var remote RemoteOperationError
	if !errors.As(result.Files[0].Cause, &remote) || remote.Failure().Scope() != scope ||
		remote.Failure().Code() != code || remote.Failure().Retryable() != retryable {
		t.Fatalf("operation failure lost remote diagnostic: %+v", result.Files[0])
	}
	return result
}
