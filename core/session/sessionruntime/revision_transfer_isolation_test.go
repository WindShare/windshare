package sessionruntime

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/session/contentflow"
	"github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/transfer"
	transferfault "github.com/windshare/windshare/core/transfer/fault"
	"github.com/windshare/windshare/core/transfer/receivecontract"
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
func (revisionTransferRevisions) ReleaseRevision(context.Context, transfer.RevisionHandle) error {
	return nil
}

type revisionTransferRanges struct {
	failed    catalog.FileID
	failure   error
	delivered content.Range
}

func (source *revisionTransferRanges) ReadRange(
	ctx context.Context,
	_ transfer.RevisionHandle,
	descriptor content.FileRevisionDescriptor,
	requested content.Range,
	sink transfer.RangeSink,
) error {
	if descriptor.FileID() == source.failed && requested.Offset != 0 {
		return source.failure
	}
	if err := sink.WriteRange(ctx, requested.Offset, make([]byte, requested.Length())); err != nil {
		return err
	}
	if descriptor.FileID() == source.failed {
		source.delivered = requested
	}
	return nil
}

type revisionTransferOutput struct {
	session         transfer.OutputSessionID
	secret          [32]byte
	scope           transfer.DirectoryAdmissionScope
	binding         transfer.DirectTreeSessionBinding
	settlements     map[catalog.FileID]transfer.FileSettlementKind
	jobPauses       int
	jobCompletes    int
	filePauses      int
	fileRetirements int
}

func newRevisionTransferOutput(t *testing.T) *revisionTransferOutput {
	t.Helper()
	return &revisionTransferOutput{
		session: id16[transfer.OutputSessionID](201),
		secret:  [32]byte{1}, settlements: make(map[catalog.FileID]transfer.FileSettlementKind),
	}
}

func newSessionRuntimeDirectTreeIntent(
	t *testing.T,
	share catalog.ShareInstance,
	root catalog.DirectoryID,
	rules transfer.SelectionRules,
	label string,
) transfer.ReceiveIntent {
	t.Helper()
	selection, err := transfer.NewSelectionSpec(share, root, rules)
	if err != nil {
		t.Fatal(err)
	}
	artifact := receivecontract.NewCatalogRootDirectoryTree()
	identityMaterial := append(append(share.Bytes(), root.Bytes()...), label...)
	operationDigest := sha256.Sum256(append([]byte("sessionruntime/test-operation/v1\x00"), identityMaterial...))
	operation, err := receivecontract.OperationIDFromBytes(operationDigest[:receivecontract.StableIdentityBytes])
	if err != nil {
		t.Fatal(err)
	}
	reservationDigest := sha256.Sum256(append([]byte("sessionruntime/test-reservation/v1\x00"), identityMaterial...))
	reservationID, err := receivecontract.DestinationReservationIDFromBytes(
		reservationDigest[:receivecontract.StableIdentityBytes],
	)
	if err != nil {
		t.Fatal(err)
	}
	authorityDigest := sha256.Sum256(append([]byte("sessionruntime/test-authority/v1\x00"), identityMaterial...))
	authority, err := receivecontract.AuthorityRefFromBytes(authorityDigest[:])
	if err != nil {
		t.Fatal(err)
	}
	reservation, err := receivecontract.NewNativeContainerRootReservation(
		operation, reservationID, artifact, authority,
	)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := receivecontract.NewDirectTreePlan(artifact, reservation)
	if err != nil {
		t.Fatal(err)
	}
	intent, err := transfer.NewReceiveIntent(selection, artifact, plan)
	if err != nil {
		t.Fatal(err)
	}
	return intent
}

func (output *revisionTransferOutput) OpenDirectTree(
	_ context.Context,
	intent transfer.ReceiveIntent,
) (transfer.DirectTreeSession, error) {
	scope, err := transfer.NewDirectoryAdmissionScope(intent)
	if err != nil {
		return nil, err
	}
	binding, err := transfer.BindDirectTreeSession(intent)
	if err != nil {
		return nil, err
	}
	output.scope = scope
	output.binding = binding
	return output, nil
}
func (output *revisionTransferOutput) SessionID() transfer.OutputSessionID { return output.session }
func (output *revisionTransferOutput) Binding() transfer.DirectTreeSessionBinding {
	return output.binding
}
func (*revisionTransferOutput) Capabilities() transfer.DirectTreeCapabilities {
	capabilities, _ := transfer.NewDirectTreeCapabilities(transfer.DirectTreeCapabilities{
		Durability: transfer.DurabilityPowerLoss, RandomWrite: true, FileFailureIsolation: true,
	})
	return capabilities
}
func (output *revisionTransferOutput) AdmitDirectory(
	_ context.Context,
	request transfer.DirectoryMaterializationRequest,
) (transfer.DirectoryAdmission, error) {
	directory, ok := request.Directory()
	if !ok {
		return transfer.DirectoryAdmission{}, transfer.ErrInvalidDirectoryAdmission
	}
	return transfer.NewDirectoryAdmissionWithSecret(output.secret[:], output.scope, directory)
}
func (*revisionTransferOutput) FinalizeDirectory(
	_ context.Context,
	admission transfer.DirectoryAdmission,
) (transfer.DirectorySettlement, error) {
	return transfer.NewFinalizedDirectorySettlement(admission)
}
func (output *revisionTransferOutput) BeginFile(
	_ context.Context,
	file transfer.MaterializationFile,
) (transfer.FileStart, error) {
	digest := sha256.Sum256([]byte(file.ArtifactPath().String()))
	identity, err := transfer.OwnedObjectIDFromBytes(digest[:])
	if err != nil {
		return transfer.FileStart{}, err
	}
	binding, err := transfer.BindFileMaterializationTarget(file.Target(), identity)
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
func (output *revisionTransferOutput) PauseTree(
	context.Context,
	transfer.JobPauseReason,
) (transfer.DirectTreeSettlement, error) {
	output.jobPauses++
	return transfer.NewDirectTreeSettlement(transfer.DirectTreeSettlementPaused)
}
func (output *revisionTransferOutput) FinalizeTree(
	_ context.Context,
	outcome transfer.DirectTreeOutcome,
) (transfer.DirectTreeSettlement, error) {
	output.jobCompletes++
	kind := transfer.DirectTreeSettlementSuccess
	if outcome == transfer.DirectTreeOutcomePartial {
		kind = transfer.DirectTreeSettlementPartial
	}
	return transfer.NewDirectTreeSettlement(kind)
}

type revisionTransferTransaction struct {
	output     *revisionTransferOutput
	binding    transfer.MaterializedFileBinding
	checkpoint transfer.VerifiedDurableRanges
}

func (transaction *revisionTransferTransaction) Binding() transfer.MaterializedFileBinding {
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
	transaction.output.filePauses++
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
	transaction.output.fileRetirements++
	settlement, err := transfer.NewFailedFileSettlement(transaction.binding)
	if err == nil {
		transaction.output.settlements[transaction.binding.FileID()] = settlement.Kind()
	}
	return settlement, err
}

func TestEveryRevisionFailureDispositionSettlesOneFileAndContinuesSibling(t *testing.T) {
	tests := []struct {
		code           uint16
		retryable      bool
		wantSettlement transfer.FileSettlementKind
		wantSourceCode transferfault.SourceCode
	}{
		{contentflow.RevisionCodeStale, false, transfer.FilePaused, transferfault.SourceRevisionChanged},
		{contentflow.RevisionCodeStale, true, transfer.FilePaused, transferfault.SourceRevisionChanged},
		{contentflow.RevisionCodeNotFound, false, transfer.FileFailed, transferfault.SourcePermanent},
		{contentflow.RevisionCodeNotFound, true, transfer.FilePaused, transferfault.SourceUnavailable},
		{contentflow.RevisionCodeUnreadable, false, transfer.FileFailed, transferfault.SourcePermanent},
		{contentflow.RevisionCodeUnreadable, true, transfer.FilePaused, transferfault.SourceUnavailable},
		{contentflow.RevisionCodeUnsupportedStability, false, transfer.FileFailed, transferfault.SourcePermanent},
		{contentflow.RevisionCodeUnsupportedStability, true, transfer.FilePaused, transferfault.SourceUnavailable},
		{contentflow.RevisionCodeQuota, false, transfer.FilePaused, transferfault.SourceUnavailable},
		{contentflow.RevisionCodeQuota, true, transfer.FilePaused, transferfault.SourceUnavailable},
		{contentflow.RevisionCodeLeaseExpired, false, transfer.FilePaused, transferfault.SourceUnavailable},
		{contentflow.RevisionCodeLeaseExpired, true, transfer.FilePaused, transferfault.SourceUnavailable},
		{contentflow.RevisionCodeDrift, false, transfer.FileFailed, transferfault.SourceRevisionInvalidated},
		{contentflow.RevisionCodeDrift, true, transfer.FileFailed, transferfault.SourceRevisionInvalidated},
		{contentflow.RevisionCodeInvalidLease, false, transfer.FilePaused, transferfault.SourceUnavailable},
		{contentflow.RevisionCodeInvalidLease, true, transfer.FilePaused, transferfault.SourceUnavailable},
	}
	for _, test := range tests {
		t.Run(fmt.Sprintf("code_%04x_retryable_%t", test.code, test.retryable), func(t *testing.T) {
			failure := RemoteOperationFailureSnapshot{
				scope: protocolsession.OperationScopeRevision,
				code:  test.code, retryable: test.retryable, message: "revision operation failed",
			}
			if test.retryable {
				failure.retryAfter = time.Millisecond
			}
			runContentTransferIsolationCase(
				t, failure.Scope(), test.code, test.retryable, test.wantSettlement, revisionOperationError(failure),
				false, test.wantSourceCode,
			)
		})
	}
}

func TestTerminalBlockOperationFailureSettlesOneFileAndContinuesSibling(t *testing.T) {
	failure := RemoteOperationFailureSnapshot{
		scope: protocolsession.OperationScopeBlock, code: contentflow.BlockCodeTimeout,
		retryable: true, retryAfter: time.Millisecond, message: "block demand exhausted its lane attempts",
	}
	runContentTransferIsolationCase(
		t, failure.Scope(), failure.Code(), failure.Retryable(), transfer.FilePaused,
		isolatedBlockOperationError(NewRemoteOperationError(failure)), false, transferfault.SourceUnavailable,
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
	runContentTransferIsolationCase(
		t,
		protocolsession.OperationScopeRevision,
		contentflow.RevisionCodeDrift,
		false,
		0,
		failure,
		true,
		transferfault.SourceRevisionInvalidated,
	)
}

func TestOpenResultRevisionStaleRemainsCLIVisibleAndContinuesSibling(t *testing.T) {
	fixture := newVerticalFixture(t)
	fixture.contentStore.openErr = content.ErrRevisionStale
	sender, receiver := connectVerticalPair(t, fixture.senderFactory, fixture.receiverFactory)
	defer sender.Close()
	defer receiver.Close()
	_, failure := receiver.OpenRevision(context.Background(), fixture.fileID)
	if failure == nil {
		t.Fatal("authenticated OPEN_RESULTS stale unexpectedly opened a revision")
	}
	if _, err := receiver.RequestLane(context.Background(), 0); err != nil {
		t.Fatalf("revision-open stale damaged the protocol session: %v", err)
	}
	runContentTransferIsolationCase(
		t,
		protocolsession.OperationScopeRevision,
		contentflow.RevisionCodeStale,
		false,
		0,
		failure,
		true,
		transferfault.SourceRevisionChanged,
	)
}

func runContentTransferIsolationCase(
	t *testing.T,
	scope uint8,
	code uint16,
	retryable bool,
	wantSettlement transfer.FileSettlementKind,
	failure error,
	failAtOpen bool,
	wantSourceCode transferfault.SourceCode,
) {
	t.Helper()
	share := id16[catalog.ShareInstance](210)
	root := id16[catalog.DirectoryID](211)
	failed := id16[catalog.FileID](212)
	good := id16[catalog.FileID](213)
	// Commit a complete read window before the next request fails, so retaining
	// a checkpoint must preserve real progress rather than an empty range set.
	size := uint64(transfer.DefaultConcurrentBlocks+1) * uint64(catalog.MinChunkSize)
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
			id16[transfer.RevisionHandle](byte(217+index)), descriptor,
		)
		if descriptorErr != nil {
			t.Fatal(descriptorErr)
		}
	}
	rules, _ := transfer.NewSelectionRules(true, nil)
	output := newRevisionTransferOutput(t)
	selection, err := transfer.NewSelectionSpec(share, root, rules)
	if err != nil {
		t.Fatal(err)
	}
	artifact := receivecontract.NewCatalogRootDirectoryTree()
	operationDigest := sha256.Sum256([]byte("sessionruntime revision transfer operation"))
	operation, err := receivecontract.OperationIDFromBytes(operationDigest[:receivecontract.StableIdentityBytes])
	if err != nil {
		t.Fatal(err)
	}
	reservationDigest := sha256.Sum256([]byte("sessionruntime revision transfer reservation"))
	reservationID, err := receivecontract.DestinationReservationIDFromBytes(
		reservationDigest[:receivecontract.StableIdentityBytes],
	)
	if err != nil {
		t.Fatal(err)
	}
	authorityDigest := sha256.Sum256([]byte("sessionruntime revision transfer authority"))
	authority, err := receivecontract.AuthorityRefFromBytes(authorityDigest[:])
	if err != nil {
		t.Fatal(err)
	}
	reservation, err := receivecontract.NewNativeContainerRootReservation(
		operation, reservationID, artifact, authority,
	)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := receivecontract.NewDirectTreePlan(artifact, reservation)
	if err != nil {
		t.Fatal(err)
	}
	intent, err := transfer.NewReceiveIntent(selection, artifact, plan)
	if err != nil {
		t.Fatal(err)
	}
	intentDigest := intent.Digest()
	jobID, err := transfer.TransferJobIDFromBytes(intentDigest[:transfer.TransferJobIdentityBytes])
	if err != nil {
		t.Fatal(err)
	}
	revisions := revisionTransferRevisions{opened: opened, failures: make(map[catalog.FileID]error)}
	ranges := &revisionTransferRanges{failed: failed, failure: failure}
	if failAtOpen {
		revisions.failures[failed] = failure
		ranges = &revisionTransferRanges{}
	}
	job, err := transfer.NewTransferJob(transfer.TransferJobConfig{
		ReceiveIntent: intent, JobID: jobID,
		Catalog:   revisionTransferCatalog{snapshot: snapshot},
		Revisions: revisions,
		Blocks:    ranges, Materializer: output,
	})
	if err != nil {
		t.Fatal(err)
	}
	result := job.Run(context.Background())
	wantFault, err := transferfault.NewSource(transferfault.ScopeFileLocal, wantSourceCode)
	if err != nil {
		t.Fatal(err)
	}
	wantSourceDrift := wantSourceCode == transferfault.SourceRevisionChanged ||
		wantSourceCode == transferfault.SourceRevisionInvalidated
	if result.Outcome != transfer.DirectTreeOutcomePartial || result.TerminationCause != nil ||
		result.SucceededFiles != 1 || len(result.Files) != 1 ||
		output.settlements[failed] != wantSettlement || output.settlements[good] != transfer.FilePublished ||
		output.jobPauses != 0 || output.jobCompletes != 1 {
		t.Fatalf("result=%+v settlements=%v", result, output.settlements)
	}
	if result.Files[0].Fault != wantFault || result.Files[0].Settlement.Kind() != wantSettlement ||
		(result.SourceDriftFailure != nil) != wantSourceDrift ||
		wantSourceDrift && result.SourceDriftFault != wantFault ||
		!wantSourceDrift && result.SourceDriftFault.Valid() {
		t.Fatalf("file fault=%v drift=%v; want source fault=%v drift=%t",
			result.Files[0].Fault, result.SourceDriftFault, wantFault, wantSourceDrift)
	}
	checkpoint, hasCheckpoint := result.Files[0].Settlement.VerifiedCheckpoint()
	switch wantSettlement {
	case transfer.FilePaused:
		if output.filePauses != 1 || output.fileRetirements != 0 || !hasCheckpoint ||
			ranges.delivered.Offset != 0 || ranges.delivered.End == 0 || ranges.delivered.End >= size ||
			!slices.Equal(checkpoint.Ranges().Ranges(), []content.Range{ranges.delivered}) ||
			result.Progress.VerifiedBytes != size+ranges.delivered.Length() ||
			result.Progress.NewlyVerifiedBytes != size+ranges.delivered.Length() ||
			result.Progress.FileOutcomes.PausedFiles != 1 {
			t.Fatalf("pause lost committed progress: output=%+v checkpoint=%+v progress=%+v",
				output, checkpoint, result.Progress)
		}
	case transfer.FileFailed:
		if output.filePauses != 0 || output.fileRetirements != 1 || hasCheckpoint {
			t.Fatalf("permanent failure did not retire its file: output=%+v settlement=%+v",
				output, result.Files[0].Settlement)
		}
	default:
		if output.filePauses != 0 || output.fileRetirements != 0 || hasCheckpoint {
			t.Fatalf("revision-open failure unexpectedly settled a file transaction: %+v", output)
		}
	}
	if failAtOpen {
		var remote *RemoteRevisionError
		if !errors.As(result.Files[0].Cause, &remote) || remote.Failure().Code != code ||
			remote.Failure().Retryable != retryable {
			t.Fatalf("open failure lost remote revision diagnostic: %+v", result.Files[0])
		}
		return
	}
	var remote RemoteOperationError
	if !errors.As(result.Files[0].Cause, &remote) || remote.Failure().Scope() != scope ||
		remote.Failure().Code() != code || remote.Failure().Retryable() != retryable {
		t.Fatalf("operation failure lost remote diagnostic: %+v", result.Files[0])
	}
}
