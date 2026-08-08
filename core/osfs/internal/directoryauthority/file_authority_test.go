package directoryauthority

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/core/osfs/internal/fileexecution"
	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/osfs/internal/outputsession"
	"github.com/windshare/windshare/core/transfer"
)

const fileAuthorityExactSize = uint64(1)

var errFileAuthorityCapture = errors.New("file authority test captured the claim")

type fileAuthorityFilePolicy struct {
	mu              sync.Mutex
	metadataMatches bool
	metadataErr     error
	closeErr        error
	closeCalls      int
}

func (policy *fileAuthorityFilePolicy) close() error {
	policy.mu.Lock()
	defer policy.mu.Unlock()
	policy.closeCalls++
	return policy.closeErr
}

func (policy *fileAuthorityFilePolicy) calls() int {
	policy.mu.Lock()
	defer policy.mu.Unlock()
	return policy.closeCalls
}

type fileAuthorityObservedFile struct {
	outputcap.File
	policy *fileAuthorityFilePolicy
}

func (file *fileAuthorityObservedFile) Close() error {
	if file == nil || file.policy == nil {
		return nil
	}
	return file.policy.close()
}

func (file *fileAuthorityObservedFile) MetadataMatches(
	uint64,
	catalog.ModifiedTime,
) (bool, error) {
	file.policy.mu.Lock()
	defer file.policy.mu.Unlock()
	return file.policy.metadataMatches, file.policy.metadataErr
}

func (file *fileAuthorityObservedFile) SameFile(other outputcap.File) (bool, error) {
	peer := other
	if wrapped, ok := other.(*fileAuthorityObservedFile); ok {
		peer = wrapped.File
	}
	return file.File.SameFile(peer)
}

type fileAuthorityPlatform struct {
	*fakePlatform
	finalPolicy *fileAuthorityFilePolicy
	rootNil     bool
}

func newFileAuthorityPlatform() *fileAuthorityPlatform {
	return &fileAuthorityPlatform{
		fakePlatform: newFakePlatform(outputcap.CallerProvidedContainer),
		finalPolicy: &fileAuthorityFilePolicy{
			metadataMatches: true,
		},
	}
}

func (platform *fileAuthorityPlatform) AcquirePublicOperationGuard() (outputcap.PublicOperationGuard, error) {
	guard, err := platform.fakePlatform.AcquirePublicOperationGuard()
	if err != nil {
		return nil, err
	}
	if platform.rootNil {
		return &fileAuthorityGuard{PublicOperationGuard: guard}, nil
	}
	root, ok := guard.Root().(*fakeDirectory)
	if !ok || root == nil {
		return &fileAuthorityGuard{PublicOperationGuard: guard}, nil
	}
	return &fileAuthorityGuard{
		PublicOperationGuard: guard,
		root:                 platform.wrapDirectory(root),
	}, nil
}

func (platform *fileAuthorityPlatform) wrapDirectory(directory *fakeDirectory) outputcap.Directory {
	if directory == nil {
		return nil
	}
	return &fileAuthorityDirectory{
		Directory: directory,
		base:      directory,
		platform:  platform,
	}
}

func (platform *fileAuthorityPlatform) wrapFile(file outputcap.File) outputcap.File {
	if file == nil {
		return nil
	}
	return &fileAuthorityObservedFile{File: file, policy: platform.finalPolicy}
}

type fileAuthorityGuard struct {
	outputcap.PublicOperationGuard
	root outputcap.Directory
}

func (guard *fileAuthorityGuard) Root() outputcap.Directory {
	if guard == nil {
		return nil
	}
	return guard.root
}

type fileAuthorityDirectory struct {
	outputcap.Directory
	base     *fakeDirectory
	platform *fileAuthorityPlatform
}

func (directory *fileAuthorityDirectory) Duplicate() (outputcap.Directory, error) {
	duplicated, err := directory.base.Duplicate()
	if err != nil || duplicated == nil {
		return nil, err
	}
	return directory.platform.wrapDirectory(duplicated.(*fakeDirectory)), nil
}

func (directory *fileAuthorityDirectory) SameDirectory(other outputcap.Directory) (bool, error) {
	peer := other
	if wrapped, ok := other.(*fileAuthorityDirectory); ok {
		peer = wrapped.base
	}
	return directory.base.SameDirectory(peer)
}

func (directory *fileAuthorityDirectory) OpenPinnedDirectory(
	expected outputcap.CurrentEntryReference,
	private bool,
) (outputcap.Directory, error) {
	opened, err := directory.base.OpenPinnedDirectory(expected, private)
	if err != nil || opened == nil {
		return nil, err
	}
	return directory.platform.wrapDirectory(opened.(*fakeDirectory)), nil
}

func (directory *fileAuthorityDirectory) OpenDirectory(name string, private bool) (outputcap.Directory, error) {
	opened, err := directory.base.OpenDirectory(name, private)
	if err != nil || opened == nil {
		return nil, err
	}
	return directory.platform.wrapDirectory(opened.(*fakeDirectory)), nil
}

func (directory *fileAuthorityDirectory) CreateDirectory(name string, private bool) (outputcap.Directory, error) {
	created, err := directory.base.CreateDirectory(name, private)
	if created == nil {
		return nil, err
	}
	return directory.platform.wrapDirectory(created.(*fakeDirectory)), err
}

func (directory *fileAuthorityDirectory) InstallDirectoryNoReplace(
	candidate outputcap.Directory,
	name string,
) (outputcap.Directory, error) {
	if wrapped, ok := candidate.(*fileAuthorityDirectory); ok {
		candidate = wrapped.base
	}
	installed, err := directory.base.InstallDirectoryNoReplace(candidate, name)
	if err != nil || installed == nil {
		return nil, err
	}
	return directory.platform.wrapDirectory(installed.(*fakeDirectory)), nil
}

func (directory *fileAuthorityDirectory) OpenFile(
	name string,
	private bool,
	writable bool,
) (outputcap.File, error) {
	opened, err := directory.base.OpenFile(name, private, writable)
	if err != nil || opened == nil {
		return nil, err
	}
	return directory.platform.wrapFile(opened), nil
}

type fileAuthorityObjects struct {
	mu sync.Mutex

	platform *fileAuthorityPlatform
	match    bool
	matchErr error

	publishMutates bool
	publishNil     bool
	publishErr     error
	linkedPolicy   *fileAuthorityFilePolicy

	matchCalls   int
	publishCalls int
	lastObject   checkpointmodel.ObjectID
	lastSize     uint64
	lastLeaf     string
}

func newFileAuthorityObjects(platform *fileAuthorityPlatform) *fileAuthorityObjects {
	return &fileAuthorityObjects{
		platform: platform,
		match:    true,
		linkedPolicy: &fileAuthorityFilePolicy{
			metadataMatches: true,
		},
	}
}

func (objects *fileAuthorityObjects) FinalMatchesOwned(
	_ context.Context,
	object checkpointmodel.ObjectID,
	exactSize uint64,
	_ outputcap.File,
) (bool, error) {
	objects.mu.Lock()
	defer objects.mu.Unlock()
	objects.matchCalls++
	objects.lastObject = object
	objects.lastSize = exactSize
	return objects.match, objects.matchErr
}

func (objects *fileAuthorityObjects) PublishOwnedNoReplace(
	_ context.Context,
	object checkpointmodel.ObjectID,
	exactSize uint64,
	parent outputcap.Directory,
	leaf string,
) (outputcap.File, error) {
	objects.mu.Lock()
	objects.publishCalls++
	objects.lastObject = object
	objects.lastSize = exactSize
	objects.lastLeaf = leaf
	mutate := objects.publishMutates
	returnNil := objects.publishNil
	publishErr := objects.publishErr
	policy := objects.linkedPolicy
	objects.mu.Unlock()

	var node *fakeNode
	if mutate {
		wrapped, ok := parent.(*fileAuthorityDirectory)
		if !ok || wrapped == nil {
			return nil, errors.Join(errFakeUnsupported, publishErr)
		}
		node = objects.platform.addFile(wrapped.base.node, leaf, exactSize)
	}
	if returnNil {
		return nil, publishErr
	}
	if node == nil {
		node = &fakeNode{id: 1_000_000, size: exactSize}
	}
	return &fileAuthorityObservedFile{File: &fakeFile{node: node}, policy: policy}, publishErr
}

type fileAuthorityOwnedFile struct {
	object checkpointmodel.ObjectID
}

func (file *fileAuthorityOwnedFile) ObjectID() checkpointmodel.ObjectID { return file.object }
func (*fileAuthorityOwnedFile) WriteAt(buffer []byte, _ int64) (int, error) {
	return len(buffer), nil
}
func (*fileAuthorityOwnedFile) Sync() error { return nil }
func (*fileAuthorityOwnedFile) SetModifiedTime(catalog.ModifiedTime) error {
	return nil
}
func (*fileAuthorityOwnedFile) MetadataMatches(uint64, catalog.ModifiedTime) (bool, error) {
	return true, nil
}
func (*fileAuthorityOwnedFile) Close() error { return nil }

type fileAuthorityClaimCapture struct {
	authority   *FileAuthority
	claim       outputsession.FileClaim
	destination fileexecution.FileDestination
	bindErr     error
}

func (capture *fileAuthorityClaimCapture) BeginFile(
	ctx context.Context,
	claim outputsession.FileClaim,
) (outputsession.FileBeginObservation, error) {
	capture.claim = claim
	capture.destination, capture.bindErr = capture.authority.BindFile(ctx, claim)
	return outputsession.FileBeginObservation{Cut: outputsession.MutationNoChange},
		errors.Join(errFileAuthorityCapture, capture.bindErr)
}

type fileAuthorityLocator struct {
	directories *Authority
	mismatch    string
}

func (locator fileAuthorityLocator) CanonicalLocatorKey(path string) (string, error) {
	key, err := locator.directories.CanonicalLocatorKey(path)
	if err == nil && path == locator.mismatch {
		return key + ":different", nil
	}
	return key, err
}

type fileAuthorityFixtureOptions struct {
	nested          bool
	preexisting     bool
	locatorMismatch bool
	objectLocator   bool
}

type fileAuthorityFixture struct {
	platform    *fileAuthorityPlatform
	directories *Authority
	objects     *fileAuthorityObjects
	authority   *FileAuthority
	capture     *fileAuthorityClaimCapture
	destination fileexecution.FileDestination
	file        transfer.OutputFile
	parent      *fakeNode
	leaf        string
	object      checkpointmodel.ObjectID
	expectation fileexecution.FinalExpectation
}

func newFileAuthorityFixture(t *testing.T, options fileAuthorityFixtureOptions) fileAuthorityFixture {
	t.Helper()
	platform := newFileAuthorityPlatform()
	if options.nested && options.preexisting {
		platform.addDirectory(platform.rootNode(), "folder")
	}
	directories, err := New(platform, Config{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = directories.Close() })
	objects := newFileAuthorityObjects(platform)
	authority, err := NewFileAuthority(directories, objects)
	if err != nil {
		t.Fatal(err)
	}
	capture := &fileAuthorityClaimCapture{authority: authority}

	share := testIdentity[catalog.ShareInstance](131)
	rootID := testIdentity[catalog.DirectoryID](141)
	rules, err := transfer.NewSelectionRules(true, nil)
	if err != nil {
		t.Fatal(err)
	}
	rootPath, err := filepath.Abs(filepath.Join("testdata", "file-authority-output"))
	if err != nil {
		t.Fatal(err)
	}
	intent, err := transfer.NewFilesystemTransferIntent(
		share,
		rootID,
		rules,
		rootPath,
		transfer.NativeFilesystemOutputBackendID,
		transfer.OutputNativeTree,
	)
	if err != nil {
		t.Fatal(err)
	}
	sessionID := testIdentity[transfer.OutputSessionID](151)
	var locator outputsession.LocatorCanonicalizer = directories
	path := "final.bin"
	if options.nested {
		path = "folder/final.bin"
	}
	if options.locatorMismatch {
		locator = fileAuthorityLocator{directories: directories, mismatch: path}
	}
	session, err := outputsession.New(outputsession.Config{
		Intent:    intent,
		SessionID: sessionID,
		Capabilities: transfer.OutputCapabilities{
			Durability:           transfer.DurabilityPowerLoss,
			Mode:                 transfer.OutputNativeTree,
			RandomWrite:          true,
			FileFailureIsolation: true,
			ModifiedTime:         true,
		},
		ReceiptSecret: bytes.Repeat([]byte{0x71}, 32),
		Locator:       locator,
		Directories:   directories,
		Files:         capture,
		Resources: outputsession.ResourceReleaserFunc(func(context.Context) error {
			return nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	rootAdmission, err := session.AdmitDirectory(context.Background(), transfer.OutputDirectory{
		DirectoryID: rootID,
		Generation:  testIdentity[catalog.DirectoryGeneration](161),
	})
	if err != nil {
		t.Fatal(err)
	}
	parentAdmission := rootAdmission
	parent := platform.rootNode()
	if options.nested {
		parentAdmission, err = session.AdmitDirectory(context.Background(), transfer.OutputDirectory{
			DirectoryID:     testIdentity[catalog.DirectoryID](171),
			Generation:      testIdentity[catalog.DirectoryGeneration](181),
			ParentAdmission: rootAdmission,
			Path:            "folder",
		})
		if err != nil {
			t.Fatal(err)
		}
		platform.mu.Lock()
		parent = platform.root.entries["folder"].node
		platform.mu.Unlock()
	}

	geometry, err := content.NewFileGeometry(fileAuthorityExactSize, catalog.MinChunkSize)
	if err != nil {
		t.Fatal(err)
	}
	descriptor, err := content.NewFileRevisionDescriptor(
		share,
		testIdentity[catalog.FileID](191),
		testIdentity[content.FileRevision](201),
		geometry,
		mustModifiedTime(t, 211),
	)
	if err != nil {
		t.Fatal(err)
	}
	var outputLocator transfer.OutputLocator
	if options.objectLocator {
		outputLocator, err = transfer.NewOutputObjectLocator(bytes.Repeat([]byte{0x81}, 32))
	} else {
		outputLocator, err = transfer.NewPathOutputLocator(path)
	}
	if err != nil {
		t.Fatal(err)
	}
	target, err := transfer.NewOutputFileTarget(intent.BackendID(), sessionID, descriptor, outputLocator)
	if err != nil {
		t.Fatal(err)
	}
	file := transfer.OutputFile{
		Path:            path,
		ExpectedSize:    fileAuthorityExactSize,
		Descriptor:      descriptor,
		Target:          target,
		ParentAdmission: parentAdmission,
	}
	_, beginErr := session.BeginFile(context.Background(), file)
	if !errors.Is(beginErr, errFileAuthorityCapture) {
		t.Fatalf("capture begin error = %v", beginErr)
	}
	object, identity := fileAuthorityObject(t, 0x91)
	expectation, err := fileexecution.NewFinalExpectation(
		identity,
		fileAuthorityExactSize,
		descriptor.ModifiedTime(),
	)
	if err != nil {
		t.Fatal(err)
	}
	return fileAuthorityFixture{
		platform: platform, directories: directories, objects: objects, authority: authority,
		capture: capture, destination: capture.destination, file: file, parent: parent,
		leaf: "final.bin", object: object, expectation: expectation,
	}
}

func fileAuthorityObject(
	t *testing.T,
	seed byte,
) (checkpointmodel.ObjectID, transfer.OutputObjectIdentity) {
	t.Helper()
	raw := bytes.Repeat([]byte{seed}, transfer.OutputObjectIdentityBytes)
	object, err := checkpointmodel.ObjectIDFromBytes(raw)
	if err != nil {
		t.Fatal(err)
	}
	identity, err := transfer.OutputObjectIdentityFromBytes(raw)
	if err != nil {
		t.Fatal(err)
	}
	return object, identity
}

func TestFileAuthorityBindsOnlyExactLiveSessionClaims(t *testing.T) {
	platform := newFileAuthorityPlatform()
	objects := newFileAuthorityObjects(platform)
	if _, err := NewFileAuthority(nil, objects); !errors.Is(err, ErrInvalidConfiguration) {
		t.Fatalf("nil directory authority error = %v", err)
	}
	directories, err := New(platform, Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer directories.Close()
	if _, err := NewFileAuthority(directories, nil); !errors.Is(err, ErrInvalidConfiguration) {
		t.Fatalf("nil object authority error = %v", err)
	}

	fixture := newFileAuthorityFixture(t, fileAuthorityFixtureOptions{})
	if fixture.capture.bindErr != nil || fixture.destination == nil {
		t.Fatalf("exact binding destination=%T error=%v", fixture.destination, fixture.capture.bindErr)
	}
	if fixture.destination.ClaimID() != fixture.capture.claim.ID() ||
		fixture.destination.Target() != fixture.file.Target {
		t.Fatalf("destination binding claim=%d target=%+v", fixture.destination.ClaimID(), fixture.destination.Target())
	}
	if (*fileDestination)(nil).ClaimID() != 0 || (*fileDestination)(nil).Target() != (transfer.OutputFileTarget{}) ||
		(*fileDestination)(nil).Close() != nil {
		t.Fatal("nil destination accessors did not remain inert")
	}

	if _, err := (*FileAuthority)(nil).BindFile(context.Background(), outputsession.FileClaim{}); !errors.Is(err, ErrInvalidClaim) {
		t.Fatalf("nil authority error = %v", err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := fixture.authority.BindFile(canceled, fixture.capture.claim); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled bind error = %v", err)
	}

	mismatched := newFileAuthorityFixture(t, fileAuthorityFixtureOptions{locatorMismatch: true})
	if !errors.Is(mismatched.capture.bindErr, ErrInvalidClaim) || mismatched.capture.destination != nil {
		t.Fatalf("locator mismatch destination=%T error=%v", mismatched.capture.destination, mismatched.capture.bindErr)
	}
	objectLocator := newFileAuthorityFixture(t, fileAuthorityFixtureOptions{objectLocator: true})
	if !errors.Is(objectLocator.capture.bindErr, ErrInvalidClaim) || objectLocator.capture.destination != nil {
		t.Fatalf("non-path locator destination=%T error=%v", objectLocator.capture.destination, objectLocator.capture.bindErr)
	}

	foreignPlatform := newFileAuthorityPlatform()
	foreignDirectories, err := New(foreignPlatform, Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer foreignDirectories.Close()
	foreign, err := NewFileAuthority(foreignDirectories, newFileAuthorityObjects(foreignPlatform))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := foreign.BindFile(context.Background(), fixture.capture.claim); !errors.Is(err, ErrParentUnavailable) {
		t.Fatalf("foreign parent binding error = %v", err)
	}
}

func TestFileAuthorityClassifiesPresenceWithoutAdoptingAliases(t *testing.T) {
	tests := []struct {
		name      string
		populate  func(fileAuthorityFixture)
		condition fileexecution.FinalCondition
	}{
		{name: "absent", condition: fileexecution.FinalAbsent},
		{
			name: "exact collision",
			populate: func(fixture fileAuthorityFixture) {
				fixture.platform.addFile(fixture.parent, fixture.leaf, fileAuthorityExactSize)
			},
			condition: fileexecution.FinalCollision,
		},
		{
			name: "platform alias",
			populate: func(fixture fileAuthorityFixture) {
				fixture.platform.addFile(fixture.parent, "FINAL.BIN", fileAuthorityExactSize)
			},
			condition: fileexecution.FinalUnsafe,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newFileAuthorityFixture(t, fileAuthorityFixtureOptions{})
			if test.populate != nil {
				test.populate(fixture)
			}
			observed, err := fixture.destination.ObserveFinalPresence(context.Background())
			if err != nil || observed.Condition() != test.condition {
				t.Fatalf("presence condition=%v error=%v", observed.Condition(), err)
			}
		})
	}

	cleanupFailure := newFileAuthorityFixture(t, fileAuthorityFixtureOptions{})
	errCleanup := errors.New("public guard release failed")
	cleanupFailure.platform.guardCloseErr = errCleanup
	observed, err := cleanupFailure.destination.ObserveFinalPresence(context.Background())
	if observed.Condition() != fileexecution.FinalAbsent || !errors.Is(err, errCleanup) {
		t.Fatalf("cleanup observation=%v error=%v", observed.Condition(), err)
	}

	guardFailure := newFileAuthorityFixture(t, fileAuthorityFixtureOptions{})
	errGuard := errors.New("public guard unavailable")
	guardFailure.platform.guardErr = errGuard
	if _, err := guardFailure.destination.ObserveFinalPresence(context.Background()); !errors.Is(err, errGuard) {
		t.Fatalf("guard acquisition error = %v", err)
	}

	rootLoss := newFileAuthorityFixture(t, fileAuthorityFixtureOptions{})
	rootLoss.platform.rootNil = true
	if _, err := rootLoss.destination.ObserveFinalPresence(context.Background()); !errors.Is(err, ErrRetainedAuthorityChanged) {
		t.Fatalf("missing guarded root error = %v", err)
	}

	restart := newFileAuthorityFixture(t, fileAuthorityFixtureOptions{nested: true, preexisting: true})
	restart.platform.replaceDirectory(restart.platform.rootNode(), "folder")
	if _, err := restart.destination.ObserveFinalPresence(context.Background()); !errors.Is(err, ErrRetainedAuthorityChanged) {
		t.Fatalf("replaced restart lineage error = %v", err)
	}
}

func TestFileAuthorityComparesOwnedIdentityBeforeMetadata(t *testing.T) {
	fixture := newFileAuthorityFixture(t, fileAuthorityFixtureOptions{})
	fixture.platform.addFile(fixture.parent, fixture.leaf, fileAuthorityExactSize)

	fixture.objects.match = false
	observed, err := fixture.destination.ObserveFinal(context.Background(), fixture.expectation)
	if err != nil || observed.Condition() != fileexecution.FinalCollision {
		t.Fatalf("foreign final condition=%v error=%v", observed.Condition(), err)
	}

	fixture.objects.match = true
	fixture.platform.finalPolicy.metadataMatches = false
	observed, err = fixture.destination.ObserveFinal(context.Background(), fixture.expectation)
	if err != nil || observed.Condition() != fileexecution.FinalOwnedMetadataMismatch {
		t.Fatalf("metadata mismatch condition=%v error=%v", observed.Condition(), err)
	}

	fixture.platform.finalPolicy.metadataMatches = true
	observed, err = fixture.destination.ObserveFinal(context.Background(), fixture.expectation)
	if err != nil || observed.Condition() != fileexecution.FinalOwnedExact {
		t.Fatalf("exact final condition=%v error=%v", observed.Condition(), err)
	}
	fixture.objects.mu.Lock()
	lastObject, lastSize := fixture.objects.lastObject, fixture.objects.lastSize
	fixture.objects.mu.Unlock()
	if lastObject != fixture.object || lastSize != fileAuthorityExactSize {
		t.Fatalf("owned comparison object=%v size=%d", lastObject, lastSize)
	}

	errCompare := errors.New("owned identity comparison failed")
	errClose := errors.New("final handle close failed")
	fixture.objects.matchErr = errCompare
	fixture.platform.finalPolicy.closeErr = errClose
	if _, err := fixture.destination.ObserveFinal(context.Background(), fixture.expectation); !errors.Is(err, errCompare) || !errors.Is(err, errClose) {
		t.Fatalf("comparison cleanup error = %v", err)
	}
	fixture.objects.matchErr = nil
	fixture.platform.finalPolicy.closeErr = nil

	errMetadata := errors.New("metadata observation failed")
	fixture.platform.finalPolicy.metadataErr = errMetadata
	if _, err := fixture.destination.ObserveFinal(context.Background(), fixture.expectation); !errors.Is(err, errMetadata) {
		t.Fatalf("metadata observation error = %v", err)
	}
	fixture.platform.finalPolicy.metadataErr = nil

	if _, err := fixture.destination.ObserveFinal(context.Background(), fileexecution.FinalExpectation{}); !errors.Is(err, ErrInvalidClaim) {
		t.Fatalf("zero final expectation error = %v", err)
	}

	wrongKind := newFileAuthorityFixture(t, fileAuthorityFixtureOptions{})
	wrongKind.platform.addDirectory(wrongKind.parent, wrongKind.leaf)
	observed, err = wrongKind.destination.ObserveFinal(context.Background(), wrongKind.expectation)
	if err != nil || observed.Condition() != fileexecution.FinalUnsafe {
		t.Fatalf("wrong-kind final condition=%v error=%v", observed.Condition(), err)
	}
}

func TestFileAuthorityPublishesNoReplaceAndReconcilesTheLiveFinal(t *testing.T) {
	t.Run("published exact", func(t *testing.T) {
		fixture := newFileAuthorityFixture(t, fileAuthorityFixtureOptions{})
		fixture.objects.publishMutates = true
		owned := &fileAuthorityOwnedFile{object: fixture.object}
		observed, err := fixture.destination.PublishNoReplace(context.Background(), owned, fixture.expectation)
		if err != nil || observed.Condition() != fileexecution.FinalOwnedExact {
			t.Fatalf("published condition=%v error=%v", observed.Condition(), err)
		}
		fixture.objects.mu.Lock()
		calls, leaf := fixture.objects.publishCalls, fixture.objects.lastLeaf
		fixture.objects.mu.Unlock()
		if calls != 1 || leaf != fixture.leaf || fixture.objects.linkedPolicy.calls() != 1 {
			t.Fatalf("publication calls=%d leaf=%q linked closes=%d", calls, leaf, fixture.objects.linkedPolicy.calls())
		}
	})

	t.Run("existing final is never replaced", func(t *testing.T) {
		fixture := newFileAuthorityFixture(t, fileAuthorityFixtureOptions{})
		fixture.platform.addFile(fixture.parent, fixture.leaf, fileAuthorityExactSize)
		fixture.objects.match = false
		observed, err := fixture.destination.PublishNoReplace(
			context.Background(),
			&fileAuthorityOwnedFile{object: fixture.object},
			fixture.expectation,
		)
		if err != nil || observed.Condition() != fileexecution.FinalCollision {
			t.Fatalf("collision condition=%v error=%v", observed.Condition(), err)
		}
		fixture.objects.mu.Lock()
		calls := fixture.objects.publishCalls
		fixture.objects.mu.Unlock()
		if calls != 0 {
			t.Fatalf("publication ran %d times for an existing final", calls)
		}
	})

	t.Run("nil publication outcome is unsafe", func(t *testing.T) {
		fixture := newFileAuthorityFixture(t, fileAuthorityFixtureOptions{})
		fixture.objects.publishNil = true
		observed, err := fixture.destination.PublishNoReplace(
			context.Background(),
			&fileAuthorityOwnedFile{object: fixture.object},
			fixture.expectation,
		)
		if observed.Condition() != fileexecution.FinalAbsent || !errors.Is(err, outputcap.ErrUnsafeNamespace) {
			t.Fatalf("nil outcome condition=%v error=%v", observed.Condition(), err)
		}
	})

	t.Run("reported failure is reconciled from the final", func(t *testing.T) {
		fixture := newFileAuthorityFixture(t, fileAuthorityFixtureOptions{})
		fixture.objects.publishMutates = true
		fixture.objects.publishErr = errors.New("link completion was not reported")
		observed, err := fixture.destination.PublishNoReplace(
			context.Background(),
			&fileAuthorityOwnedFile{object: fixture.object},
			fixture.expectation,
		)
		if observed.Condition() != fileexecution.FinalOwnedExact ||
			!errors.Is(err, fixture.objects.publishErr) {
			t.Fatalf("reconciled condition=%v error=%v", observed.Condition(), err)
		}
	})

	t.Run("linked handle close failure remains visible", func(t *testing.T) {
		fixture := newFileAuthorityFixture(t, fileAuthorityFixtureOptions{})
		fixture.objects.publishMutates = true
		errClose := errors.New("linked handle close failed")
		fixture.objects.linkedPolicy.closeErr = errClose
		observed, err := fixture.destination.PublishNoReplace(
			context.Background(),
			&fileAuthorityOwnedFile{object: fixture.object},
			fixture.expectation,
		)
		if observed.Condition() != fileexecution.FinalOwnedExact || !errors.Is(err, errClose) {
			t.Fatalf("linked close condition=%v error=%v", observed.Condition(), err)
		}
	})

	fixture := newFileAuthorityFixture(t, fileAuthorityFixtureOptions{})
	if _, err := fixture.destination.PublishNoReplace(context.Background(), nil, fixture.expectation); !errors.Is(err, ErrInvalidClaim) {
		t.Fatalf("nil owned file error = %v", err)
	}
	otherObject, _ := fileAuthorityObject(t, 0x92)
	if _, err := fixture.destination.PublishNoReplace(
		context.Background(),
		&fileAuthorityOwnedFile{object: otherObject},
		fixture.expectation,
	); !errors.Is(err, ErrInvalidClaim) {
		t.Fatalf("foreign owned file error = %v", err)
	}
}

func TestFileAuthoritySyncAndCloseRetainParentGuardSemantics(t *testing.T) {
	fixture := newFileAuthorityFixture(t, fileAuthorityFixtureOptions{nested: true, preexisting: true})
	if err := fixture.destination.SyncFinalParent(context.Background()); err != nil {
		t.Fatal(err)
	}
	fixture.platform.mu.Lock()
	syncCalls := fixture.parent.syncCalls
	fixture.platform.mu.Unlock()
	if syncCalls != 1 {
		t.Fatalf("final parent sync calls = %d", syncCalls)
	}

	errSync := errors.New("final parent sync failed")
	fixture.platform.mu.Lock()
	fixture.parent.syncErr = errSync
	fixture.platform.mu.Unlock()
	if err := fixture.destination.SyncFinalParent(context.Background()); !errors.Is(err, errSync) {
		t.Fatalf("final parent sync error = %v", err)
	}

	if err := fixture.destination.Close(); err != nil {
		t.Fatal(err)
	}
	if err := fixture.destination.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.destination.ObserveFinalPresence(context.Background()); !errors.Is(err, ErrAuthorityClosed) {
		t.Fatalf("closed destination error = %v", err)
	}
}
