package checkpointstore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io/fs"
	"path/filepath"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/core/osfs/internal/fileexecution"
	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/osfs/internal/outputsession"
	"github.com/windshare/windshare/core/transfer"
)

type runtimeClosureClaimPorts struct {
	claim outputsession.FileClaim
}

func (*runtimeClosureClaimPorts) CanonicalLocatorKey(path string) (string, error) {
	return "runtime-closure:" + path, nil
}

func (*runtimeClosureClaimPorts) MaterializeDirectory(
	context.Context,
	outputsession.DirectoryClaim,
) (outputsession.DirectoryMaterialization, error) {
	return outputsession.DirectoryMaterialization{
		Cut: outputsession.MutationStable, Disposition: outputsession.DirectoryCallerProvidedRoot,
	}, nil
}

func (*runtimeClosureClaimPorts) FinalizeDirectory(
	context.Context,
	outputsession.DirectoryClaim,
) (outputsession.DirectoryFinalization, error) {
	return outputsession.FinalizedDirectory(), nil
}

func (ports *runtimeClosureClaimPorts) BeginFile(
	_ context.Context,
	claim outputsession.FileClaim,
) (outputsession.FileBeginObservation, error) {
	ports.claim = claim
	settlement, err := transfer.NewCollisionFileSettlement(claim.File().Target)
	return outputsession.FileBeginObservation{Cut: outputsession.MutationStable, Settlement: settlement}, err
}

func (*runtimeClosureClaimPorts) ReleaseOutputSession(context.Context) error { return nil }

type runtimeClosureClaimFixture struct {
	intent    transfer.TransferIntent
	ownership checkpointmodel.Ownership
	sessionID transfer.OutputSessionID
	claim     outputsession.FileClaim
}

type runtimeClosureIdentity interface {
	~[catalog.IdentityBytes]byte
}

func runtimeClosureID[T runtimeClosureIdentity](seed byte) T {
	var value T
	for index := range value {
		value[index] = seed + byte(index)
	}
	return value
}

func runtimeClosureClaim(t *testing.T, exactSize uint64) runtimeClosureClaimFixture {
	t.Helper()
	share := runtimeClosureID[catalog.ShareInstance](1)
	rootID := runtimeClosureID[catalog.DirectoryID](21)
	rules, err := transfer.NewSelectionRules(true, nil)
	if err != nil {
		t.Fatal(err)
	}
	intent, err := transfer.NewFilesystemTransferIntent(
		share, rootID, rules, filepath.Join(t.TempDir(), "output"),
		transfer.NativeFilesystemOutputBackendID, transfer.OutputNativeTree,
	)
	if err != nil {
		t.Fatal(err)
	}
	ownership, err := checkpointmodel.NewOwnership(checkpointmodel.OwnershipSpec{
		Backend: intent.BackendID(), Certification: checkpointmodel.CertificationWindowsNTFSProcessRestart,
		RootIdentity: bytes.Repeat([]byte{0x51}, sha256.Size), RootOpenDisposition: checkpointmodel.CallerProvidedContainer,
	})
	if err != nil {
		t.Fatal(err)
	}
	sessionID := runtimeClosureID[transfer.OutputSessionID](61)
	geometry, err := content.NewFileGeometry(exactSize, catalog.MinChunkSize)
	if err != nil {
		t.Fatal(err)
	}
	descriptor, err := content.NewFileRevisionDescriptor(
		share, runtimeClosureID[catalog.FileID](31), runtimeClosureID[content.FileRevision](41),
		geometry, catalog.ModifiedTime{},
	)
	if err != nil {
		t.Fatal(err)
	}
	const path = "file.bin"
	locator, err := transfer.NewPathOutputLocator(path)
	if err != nil {
		t.Fatal(err)
	}
	target, err := transfer.NewOutputFileTarget(intent.BackendID(), sessionID, descriptor, locator)
	if err != nil {
		t.Fatal(err)
	}
	ports := &runtimeClosureClaimPorts{}
	session, err := outputsession.New(outputsession.Config{
		Intent: intent, SessionID: sessionID,
		Capabilities: transfer.OutputCapabilities{
			Durability: transfer.DurabilityPowerLoss, Mode: transfer.OutputNativeTree,
			RandomWrite: true, FileFailureIsolation: true, ModifiedTime: true,
		},
		ReceiptSecret: bytes.Repeat([]byte{0x91}, 32),
		Locator:       ports, Directories: ports, Files: ports, Resources: ports,
	})
	if err != nil {
		t.Fatal(err)
	}
	admission, err := session.AdmitDirectory(context.Background(), transfer.OutputDirectory{
		DirectoryID: rootID, Generation: runtimeClosureID[catalog.DirectoryGeneration](71), Path: "",
	})
	if err != nil {
		t.Fatal(err)
	}
	file := transfer.OutputFile{
		Path: path, ExpectedSize: exactSize, Descriptor: descriptor,
		Target: target, ParentAdmission: admission,
	}
	if _, err := session.BeginFile(context.Background(), file); err != nil {
		t.Fatal(err)
	}
	if ports.claim.ID() == 0 {
		t.Fatal("output session did not produce an atomic file claim")
	}
	return runtimeClosureClaimFixture{intent: intent, ownership: ownership, sessionID: sessionID, claim: ports.claim}
}

func runtimeClosureRecord(t *testing.T, fixture runtimeClosureClaimFixture, objectFill byte) checkpointmodel.Record {
	t.Helper()
	file := fixture.claim.File()
	record, err := checkpointmodel.NewRecord(checkpointmodel.RecordSpec{
		TransferIntentDigest: fixture.intent.Digest(), FileID: file.Descriptor.FileID(),
		FileRevision: file.Descriptor.FileRevision(), CanonicalPath: file.Path, ExactSize: file.ExpectedSize,
		BackendID: string(fixture.ownership.Backend()), RootIdentity: fixture.ownership.RootIdentity().Bytes(),
		OwnedOutputObject: bytes.Repeat([]byte{objectFill}, sha256.Size),
		StateGeneration:   1, CheckpointGeneration: 0,
		Phase: checkpointmodel.PhaseActive, CommitState: checkpointmodel.CommitCandidate,
	})
	if err != nil {
		t.Fatal(err)
	}
	return record
}

type runtimeClosureCheckpointCapture struct {
	key fileexecution.CheckpointKey
	err error
}

func (capture *runtimeClosureCheckpointCapture) Lookup(
	_ context.Context,
	key fileexecution.CheckpointKey,
) (checkpointmodel.Record, bool, error) {
	capture.key = key
	return checkpointmodel.Record{}, false, capture.err
}

func (*runtimeClosureCheckpointCapture) Store(
	context.Context,
	*checkpointmodel.Record,
	checkpointmodel.Record,
) (fileexecution.CheckpointObservation, error) {
	return fileexecution.CheckpointObservation{}, errors.New("unexpected checkpoint store")
}

type runtimeClosureDirectoryAuthority struct{}

func (runtimeClosureDirectoryAuthority) BindFile(
	_ context.Context,
	claim outputsession.FileClaim,
) (fileexecution.FileDestination, error) {
	return &runtimeClosureDestination{claimID: claim.ID(), target: claim.File().Target}, nil
}

type runtimeClosureDestination struct {
	claimID outputsession.ClaimID
	target  transfer.OutputFileTarget
}

func (destination *runtimeClosureDestination) ClaimID() outputsession.ClaimID {
	return destination.claimID
}
func (destination *runtimeClosureDestination) Target() transfer.OutputFileTarget {
	return destination.target
}
func (*runtimeClosureDestination) ObserveFinal(
	context.Context,
	fileexecution.FinalExpectation,
) (fileexecution.FinalObservation, error) {
	return fileexecution.ObserveFinal(fileexecution.FinalAbsent)
}
func (*runtimeClosureDestination) ObserveFinalPresence(context.Context) (fileexecution.FinalObservation, error) {
	return fileexecution.ObserveFinal(fileexecution.FinalAbsent)
}
func (*runtimeClosureDestination) PublishNoReplace(
	context.Context,
	fileexecution.OwnedFile,
	fileexecution.FinalExpectation,
) (fileexecution.FinalObservation, error) {
	return fileexecution.ObserveFinal(fileexecution.FinalAbsent)
}
func (*runtimeClosureDestination) SyncFinalParent(context.Context) error { return nil }
func (*runtimeClosureDestination) Close() error                          { return nil }

func runtimeClosureObject(t *testing.T, fill byte) checkpointmodel.ObjectID {
	t.Helper()
	object, err := checkpointmodel.ObjectIDFromBytes(bytes.Repeat([]byte{fill}, sha256.Size))
	if err != nil {
		t.Fatal(err)
	}
	return object
}

func runtimeClosureWriteCandidate(
	t *testing.T,
	records outputcap.Directory,
	record checkpointmodel.Record,
	attempt int,
) string {
	t.Helper()
	encoded, err := checkpointmodel.EncodeRecord(record)
	if err != nil {
		t.Fatal(err)
	}
	shardName, recordName := recordLocation(record.RecordID())
	shard, err := OpenShard(records, shardName, true)
	if err != nil {
		t.Fatal(err)
	}
	candidateName := TemporaryName(recordName, encoded, attempt)
	writeMemoryFile(t, shard, candidateName, encoded)
	if err := shard.Close(); err != nil {
		t.Fatal(err)
	}
	return candidateName
}

func runtimeClosureUnexpectedWitness(t *testing.T) InitialCandidateWitness {
	t.Helper()
	return func(checkpointmodel.Record) (bool, error) {
		t.Fatal("committed candidate unexpectedly requested an initial witness")
		return false, nil
	}
}

func runtimeClosureRequireMissingCandidate(
	t *testing.T,
	records outputcap.Directory,
	recordID checkpointmodel.RecordID,
	name string,
) {
	t.Helper()
	shardName, _ := recordLocation(recordID)
	shard, err := OpenShard(records, shardName, false)
	if err != nil {
		t.Fatal(err)
	}
	defer shard.Close()
	if _, err := ReadFile(shard, name); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("candidate %q remains: %v", name, err)
	}
}

func runtimeClosureRequirePresentCandidate(
	t *testing.T,
	records outputcap.Directory,
	recordID checkpointmodel.RecordID,
	name string,
) {
	t.Helper()
	shardName, _ := recordLocation(recordID)
	shard, err := OpenShard(records, shardName, false)
	if err != nil {
		t.Fatal(err)
	}
	defer shard.Close()
	if _, err := ReadFile(shard, name); err != nil {
		t.Fatalf("candidate %q was mutated: %v", name, err)
	}
}

func runtimeClosureRequireImage(t *testing.T, directory outputcap.Directory, name string, want []byte) {
	t.Helper()
	got, err := ReadFile(directory, name)
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("image %q = %q, %v", name, got, err)
	}
}

func runtimeClosureOpenWithCloseFailure(base outputcap.Directory, failure error) outputcap.Directory {
	return &runtimeClosureOpenDirectory{
		Directory: base,
		open: func(name string, private bool) (outputcap.Directory, error) {
			child, err := base.OpenDirectory(name, private)
			if err != nil {
				return child, err
			}
			return &runtimeClosureCloseDirectory{Directory: child, close: func() error { return failure }}, nil
		},
	}
}

func runtimeClosureOpenWithLinkFailure(
	base outputcap.Directory,
	shardName string,
	failure error,
) outputcap.Directory {
	return &runtimeClosureOpenDirectory{
		Directory: base,
		open: func(name string, private bool) (outputcap.Directory, error) {
			child, err := base.OpenDirectory(name, private)
			if err != nil || name != shardName {
				return child, err
			}
			return &runtimeClosureLinkDirectory{
				Directory: child,
				link:      func(outputcap.File, string) (outputcap.File, error) { return nil, failure },
			}, nil
		},
	}
}

func runtimeClosureOpenWithReplaceFailure(
	base outputcap.Directory,
	shardName string,
	failure error,
) outputcap.Directory {
	return &runtimeClosureOpenDirectory{
		Directory: base,
		open: func(name string, private bool) (outputcap.Directory, error) {
			child, err := base.OpenDirectory(name, private)
			if err != nil || name != shardName {
				return child, err
			}
			return &runtimeClosureReplaceDirectory{
				Directory: child,
				replace:   func(outputcap.File, string) error { return failure },
			}, nil
		},
	}
}

func runtimeClosureCloseRepository(t *testing.T, namespace *Namespace, lease *IntentLease, repository *Repository) {
	t.Helper()
	if err := errors.Join(repository.Close(), lease.Close(), namespace.Close()); err != nil {
		t.Error(err)
	}
}

func runtimeClosureInitializedRoot(t *testing.T, fill byte) (*memoryDirectory, CertifiedConfig) {
	t.Helper()
	root := newMemoryDirectory()
	config, _ := certifiedFixture(t, root, checkpointmodel.CallerProvidedContainer, fill)
	namespace, err := Initialize(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := namespace.Close(); err != nil {
		t.Fatal(err)
	}
	return root, config
}

func runtimeClosureConfigWithCheckpoint(
	t *testing.T,
	root *memoryDirectory,
	config CertifiedConfig,
	wrap func(outputcap.Directory) outputcap.Directory,
) CertifiedConfig {
	t.Helper()
	control, err := root.OpenDirectory(ControlDirectory, true)
	if err != nil {
		t.Fatal(err)
	}
	checkpointRoot, err := control.OpenDirectory(CheckpointDirectory, true)
	if err != nil {
		t.Fatal(err)
	}
	wrappedCheckpoint := wrap(checkpointRoot)
	wrappedControl := &runtimeClosureOpenDirectory{
		Directory: control,
		open: func(name string, private bool) (outputcap.Directory, error) {
			if name == CheckpointDirectory {
				return wrappedCheckpoint, nil
			}
			return control.OpenDirectory(name, private)
		},
	}
	config.Root = &runtimeClosureOpenDirectory{
		Directory: root,
		open: func(name string, private bool) (outputcap.Directory, error) {
			if name == ControlDirectory {
				return wrappedControl, nil
			}
			return root.OpenDirectory(name, private)
		},
	}
	return config
}

func runtimeClosureTrackedDirectory(
	directory outputcap.Directory,
	name string,
	order *[]string,
	failure error,
) outputcap.Directory {
	return &runtimeClosureCloseDirectory{
		Directory: directory,
		close: func() error {
			*order = append(*order, name)
			return failure
		},
	}
}

func runtimeClosureCreateOwnedDirectory(
	t *testing.T,
	root outputcap.Directory,
	object checkpointmodel.ObjectID,
	suffix string,
) {
	t.Helper()
	shardName, name := ownedObjectLocation(object, suffix)
	shard, err := OpenShard(root, shardName, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := shard.CreateDirectory(name, true); err != nil {
		t.Fatal(err)
	}
	if err := shard.Close(); err != nil {
		t.Fatal(err)
	}
}

func runtimeClosureCreateOwnedFile(
	t *testing.T,
	root outputcap.Directory,
	object checkpointmodel.ObjectID,
	suffix string,
	size uint64,
) outputcap.File {
	t.Helper()
	shardName, name := ownedObjectLocation(object, suffix)
	shard, err := OpenShard(root, shardName, true)
	if err != nil {
		t.Fatal(err)
	}
	file, err := shard.CreateFile(name, true, int64(size))
	if err != nil {
		t.Fatal(err)
	}
	if err := shard.Close(); err != nil {
		t.Fatal(err)
	}
	return file
}

func runtimeClosureCreateOwnedPair(
	t *testing.T,
	stages outputcap.Directory,
	anchors outputcap.Directory,
	object checkpointmodel.ObjectID,
	size uint64,
) {
	t.Helper()
	stage := runtimeClosureCreateOwnedFile(t, stages, object, ownedStageSuffix, size)
	anchorShard, anchorName := ownedObjectLocation(object, ownedAnchorSuffix)
	anchorDirectory, err := OpenShard(anchors, anchorShard, true)
	if err != nil {
		t.Fatal(err)
	}
	anchor, err := anchorDirectory.LinkFileNoReplace(stage, anchorName)
	if err != nil {
		t.Fatal(err)
	}
	if err := errors.Join(stage.Close(), anchor.Close(), anchorDirectory.Close()); err != nil {
		t.Fatal(err)
	}
}

type runtimeClosureOpenDirectory struct {
	outputcap.Directory
	open func(string, bool) (outputcap.Directory, error)
}

func (directory *runtimeClosureOpenDirectory) OpenDirectory(name string, private bool) (outputcap.Directory, error) {
	return directory.open(name, private)
}

type runtimeClosureNamesDirectory struct {
	outputcap.Directory
	names func(int) ([]string, error)
}

func (directory *runtimeClosureNamesDirectory) Names(limit int) ([]string, error) {
	return directory.names(limit)
}

type runtimeClosureClassifyDirectory struct {
	outputcap.Directory
	classify func(string) (outputcap.EntryKind, bool, error)
}

type runtimeClosureObserveDirectory struct {
	outputcap.Directory
	observe func(string) (outputcap.EntryKind, error)
}

func (directory *runtimeClosureObserveDirectory) ObserveEntry(name string) (outputcap.EntryKind, error) {
	return directory.observe(name)
}

func (directory *runtimeClosureClassifyDirectory) ClassifyExactEntry(
	name string,
) (outputcap.EntryKind, bool, error) {
	return directory.classify(name)
}

type runtimeClosureCreateDirectory struct {
	outputcap.Directory
	create func(string, bool) (outputcap.Directory, error)
}

func (directory *runtimeClosureCreateDirectory) CreateDirectory(
	name string,
	private bool,
) (outputcap.Directory, error) {
	return directory.create(name, private)
}

type runtimeClosureCreateFileDirectory struct {
	outputcap.Directory
	create func(string, bool, int64) (outputcap.File, error)
}

func (directory *runtimeClosureCreateFileDirectory) CreateFile(
	name string,
	private bool,
	size int64,
) (outputcap.File, error) {
	return directory.create(name, private, size)
}

type runtimeClosureLinkDirectory struct {
	outputcap.Directory
	link func(outputcap.File, string) (outputcap.File, error)
}

func (directory *runtimeClosureLinkDirectory) LinkFileNoReplace(source outputcap.File, name string) (outputcap.File, error) {
	return directory.link(source, name)
}

type runtimeClosureRemoveDirectory struct {
	outputcap.Directory
	remove func(string, outputcap.File) error
}

func (directory *runtimeClosureRemoveDirectory) RemoveFile(name string, expected outputcap.File) error {
	return directory.remove(name, expected)
}

type runtimeClosureReplaceDirectory struct {
	outputcap.Directory
	replace func(outputcap.File, string) error
}

func (directory *runtimeClosureReplaceDirectory) ReplacePrivateFile(source outputcap.File, name string) error {
	return directory.replace(source, name)
}

type runtimeClosureOpenFileDirectory struct {
	outputcap.Directory
	open func(string, bool, bool) (outputcap.File, error)
}

func (directory *runtimeClosureOpenFileDirectory) OpenFile(
	name string,
	private bool,
	writable bool,
) (outputcap.File, error) {
	return directory.open(name, private, writable)
}

type runtimeClosureSyncDirectory struct {
	outputcap.Directory
	sync func() error
}

func (directory *runtimeClosureSyncDirectory) Sync() error { return directory.sync() }

type runtimeClosureDuplicateDirectory struct {
	outputcap.Directory
	duplicate func() (outputcap.Directory, error)
}

func (directory *runtimeClosureDuplicateDirectory) Duplicate() (outputcap.Directory, error) {
	return directory.duplicate()
}

type runtimeClosureAcquireDirectory struct {
	outputcap.Directory
	acquire func(string, bool) (outputcap.Lock, bool, error)
}

func (directory *runtimeClosureAcquireDirectory) AcquireLock(
	name string,
	private bool,
) (outputcap.Lock, bool, error) {
	return directory.acquire(name, private)
}

type runtimeClosureCloseDirectory struct {
	outputcap.Directory
	close func() error
}

func (directory *runtimeClosureCloseDirectory) Close() error { return directory.close() }

type runtimeClosureCloseFile struct {
	outputcap.File
	close func() error
}

func (file *runtimeClosureCloseFile) Close() error { return file.close() }

type runtimeClosureSizeFile struct {
	outputcap.File
	size func() (uint64, error)
}

func (file *runtimeClosureSizeFile) Size() (uint64, error) { return file.size() }

type runtimeClosureReadFile struct {
	outputcap.File
	read func([]byte, int64) (int, error)
}

func (file *runtimeClosureReadFile) ReadAt(target []byte, offset int64) (int, error) {
	return file.read(target, offset)
}

type runtimeClosureCloseLock struct {
	outputcap.Lock
	close func() error
}

func (lock *runtimeClosureCloseLock) Close() error { return lock.close() }
