package directoryauthority

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/core/osfs/internal/fileexecution"
	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/osfs/internal/outputsession"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/ordinaryoutput"
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

func (directory *fileAuthorityDirectory) PersistentDirectoryIdentityClaim() ([]byte, error) {
	return []byte("file-authority-dir-claim"), nil
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

func (directory *fileAuthorityDirectory) LinkFileNoReplace(
	source outputcap.File,
	name string,
) (outputcap.File, error) {
	if observed, ok := source.(*fileAuthorityObservedFile); ok {
		source = observed.File
	}
	native, ok := source.(*fakeFile)
	if !ok || native == nil || native.node == nil {
		return nil, errFakeUnsupported
	}
	directory.platform.mu.Lock()
	defer directory.platform.mu.Unlock()
	if _, exists := directory.base.node.entries[name]; exists {
		return nil, outputcap.ErrNamespaceCollision
	}
	directory.base.node.entries[name] = fakeEntry{
		kind: outputcap.EntryRegularFile,
		node: native.node,
	}
	return directory.platform.wrapFile(&fakeFile{node: native.node}), nil
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
	capture.destination, capture.bindErr = capture.authority.BindFile(
		ctx, claim.File(), claim.DestinationPath(),
	)
	return outputsession.FileBeginObservation{Cut: outputsession.MutationNoChange},
		errors.Join(errFileAuthorityCapture, capture.bindErr)
}

type fileAuthorityFixtureOptions struct {
	nested      bool
	preexisting bool
	live        bool
}

type fileAuthorityFixture struct {
	platform    *fileAuthorityPlatform
	directories *Authority
	objects     *fileAuthorityObjects
	authority   *FileAuthority
	capture     *fileAuthorityClaimCapture
	destination fileexecution.FileDestination
	file        transfer.MaterializationFile
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
	sessionID := testIdentity[transfer.OutputSessionID](151)
	var authority *FileAuthority
	if options.live {
		authority, err = NewLiveFileAuthority(directories, sessionID)
	} else {
		authority, err = NewFileAuthority(directories, objects, sessionID)
	}
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
	intent := testDirectTreeIntent(t, share, rootID, rules)
	projector, err := transfer.OrdinaryOutputArtifactPathProjector(intent)
	if err != nil {
		t.Fatal(err)
	}
	path := "final.bin"
	if options.nested {
		path = "folder/final.bin"
	}
	session, err := outputsession.New(outputsession.Config{
		Intent:    intent,
		SessionID: sessionID,
		Capabilities: transfer.DirectTreeCapabilities{
			Durability:           transfer.DurabilityPowerLoss,
			RandomWrite:          true,
			FileFailureIsolation: true,
			ModifiedTime:         true,
		},
		ReceiptSecret: bytes.Repeat([]byte{0x71}, 32),
		Locator:       directories,
		Destinations: outputsession.ArtifactDestinationBinderFunc(func(path ordinaryoutput.ArtifactPath) (outputsession.DestinationPath, error) {
			return outputsession.NewDestinationPath(path.String())
		}),
		Directories: directories,
		Files:       capture,
		Resources: outputsession.ResourceReleaserFunc(func(context.Context) error {
			return nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	rootSource := testSourceDirectory(
		t, rootID, testIdentity[catalog.DirectoryGeneration](161),
		transfer.DirectoryAdmission{}, "", catalog.ModifiedTime{},
	)
	rootAdmission, err := session.AdmitDirectory(
		context.Background(), projectedDirectoryRequest(
			t, intent, rootSource, transfer.MaterializedDirectoryClaim{},
		),
	)
	if err != nil {
		t.Fatal(err)
	}
	parentAdmission := rootAdmission
	parent := platform.rootNode()
	var parentMaterialization transfer.MaterializedDirectoryClaim
	if options.nested {
		directorySource := testSourceDirectory(
			t, testIdentity[catalog.DirectoryID](171), testIdentity[catalog.DirectoryGeneration](181),
			rootAdmission, "folder", catalog.ModifiedTime{},
		)
		directoryRequest := projectedDirectoryRequest(
			t, intent, directorySource, transfer.MaterializedDirectoryClaim{},
		)
		parentAdmission, err = session.AdmitDirectory(context.Background(), directoryRequest)
		if err != nil {
			t.Fatal(err)
		}
		_, materialized := directoryRequest.Projection().ArtifactPath()
		if !materialized {
			t.Fatal("nested directory projection did not materialize")
		}
		parentMaterialization, err = transfer.NewMaterializedDirectoryClaim(parentAdmission, directoryRequest)
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
	sourcePath, err := ordinaryoutput.NewSourceCatalogPath(path)
	if err != nil {
		t.Fatal(err)
	}
	file, err := transfer.NewMaterializationFile(
		projector, sourcePath, descriptor, sessionID, parentAdmission, parentMaterialization,
	)
	if err != nil {
		t.Fatal(err)
	}
	_, beginErr := session.BeginFile(context.Background(), file)
	if !errors.Is(beginErr, errFileAuthorityCapture) {
		t.Fatalf("capture begin error = %v", beginErr)
	}
	object, identity := fileAuthorityObject(t, 0x91)
	expectation, err := fileexecution.NewFinalExpectation(
		identity,
		fileAuthorityExactSize,
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
) (checkpointmodel.ObjectID, transfer.OwnedObjectID) {
	t.Helper()
	raw := bytes.Repeat([]byte{seed}, transfer.OwnedObjectIdentityBytes)
	object, err := checkpointmodel.ObjectIDFromBytes(raw)
	if err != nil {
		t.Fatal(err)
	}
	identity, err := transfer.OwnedObjectIDFromBytes(raw)
	if err != nil {
		t.Fatal(err)
	}
	return object, identity
}

func TestFileAuthorityBindsOnlyExactLiveSessionClaims(t *testing.T) {
	platform := newFileAuthorityPlatform()
	objects := newFileAuthorityObjects(platform)
	if _, err := NewFileAuthority(nil, objects, testIdentity[transfer.OutputSessionID](1)); !errors.Is(err, ErrInvalidConfiguration) {
		t.Fatalf("nil directory authority error = %v", err)
	}
	directories, err := New(platform, Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer directories.Close()
	if _, err := NewFileAuthority(directories, nil, testIdentity[transfer.OutputSessionID](1)); !errors.Is(err, ErrInvalidConfiguration) {
		t.Fatalf("nil object authority error = %v", err)
	}

	fixture := newFileAuthorityFixture(t, fileAuthorityFixtureOptions{})
	if fixture.capture.bindErr != nil || fixture.destination == nil {
		t.Fatalf("exact binding destination=%T error=%v", fixture.destination, fixture.capture.bindErr)
	}
	if fixture.destination.Target() != fixture.file.Target() {
		t.Fatalf("destination target=%+v", fixture.destination.Target())
	}
	if (*fileDestination)(nil).Target() != (transfer.FileMaterializationTarget{}) ||
		(*fileDestination)(nil).Close() != nil {
		t.Fatal("nil destination accessors did not remain inert")
	}

	if _, err := (*FileAuthority)(nil).BindFile(
		context.Background(), transfer.MaterializationFile{}, transfer.OutputDestinationPath{},
	); !errors.Is(err, ErrInvalidClaim) {
		t.Fatalf("nil authority error = %v", err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := fixture.authority.BindFile(
		canceled, fixture.capture.claim.File(), fixture.capture.claim.DestinationPath(),
	); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled bind error = %v", err)
	}

	foreignPlatform := newFileAuthorityPlatform()
	foreignDirectories, err := New(foreignPlatform, Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer foreignDirectories.Close()
	foreign, err := NewFileAuthority(foreignDirectories, newFileAuthorityObjects(foreignPlatform), testIdentity[transfer.OutputSessionID](152))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := foreign.BindFile(
		context.Background(), fixture.capture.claim.File(), fixture.capture.claim.DestinationPath(),
	); !errors.Is(err, ErrInvalidClaim) {
		t.Fatalf("foreign session binding error = %v", err)
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

func TestFileAuthorityProvesOwnedIdentityAndSizeWithoutDisplayMetadata(t *testing.T) {
	fixture := newFileAuthorityFixture(t, fileAuthorityFixtureOptions{})
	fixture.platform.addFile(fixture.parent, fixture.leaf, fileAuthorityExactSize)

	fixture.objects.match = false
	observed, err := fixture.destination.ObserveFinal(context.Background(), fixture.expectation)
	if err != nil || observed.Condition() != fileexecution.FinalCollision {
		t.Fatalf("foreign final condition=%v error=%v", observed.Condition(), err)
	}

	fixture.objects.match = true
	fixture.platform.finalPolicy.metadataMatches = false
	fixture.platform.finalPolicy.metadataErr = errors.New("display metadata unavailable")
	observed, err = fixture.destination.ObserveFinal(context.Background(), fixture.expectation)
	if err != nil || observed.Condition() != fileexecution.FinalOwnedExact {
		t.Fatalf("owned final condition=%v error=%v", observed.Condition(), err)
	}

	fixture.platform.finalPolicy.metadataMatches = true
	fixture.platform.finalPolicy.metadataErr = nil
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

type liveFileAuthorityOwned struct {
	*fileAuthorityOwnedFile
	native outputcap.File
}

func (file *liveFileAuthorityOwned) NativeFile() outputcap.File {
	if file == nil {
		return nil
	}
	return file.native
}

func TestLiveFileAuthorityPublishesOnlyTheRetainedStageIdentity(t *testing.T) {
	sessionID := testIdentity[transfer.OutputSessionID](151)
	if _, err := NewLiveFileAuthority(nil, sessionID); !errors.Is(err, ErrInvalidConfiguration) {
		t.Fatalf("nil live directory authority error = %v", err)
	}
	platform := newFileAuthorityPlatform()
	directories, err := New(platform, Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer directories.Close()
	if _, err := NewLiveFileAuthority(directories, transfer.OutputSessionID{}); !errors.Is(err, ErrInvalidConfiguration) {
		t.Fatalf("zero live session error = %v", err)
	}

	fixture := newFileAuthorityFixture(t, fileAuthorityFixtureOptions{live: true})
	if fixture.capture.bindErr != nil || fixture.destination == nil {
		t.Fatalf("live binding destination=%T error=%v", fixture.destination, fixture.capture.bindErr)
	}
	destination, ok := fixture.destination.(*fileDestination)
	if !ok {
		t.Fatalf("live destination type = %T", fixture.destination)
	}
	if err := destination.WithExactParent(context.Background(), nil); !errors.Is(err, ErrInvalidClaim) {
		t.Fatalf("nil parent callback error = %v", err)
	}
	parentVisited := false
	if err := destination.WithExactParent(context.Background(), func(parent outputcap.Directory) error {
		parentVisited = parent != nil
		return nil
	}); err != nil || !parentVisited {
		t.Fatalf("parent visit = (%t, %v)", parentVisited, err)
	}
	if observed, err := fixture.destination.ObserveFinalPresence(context.Background()); err != nil ||
		observed.Condition() != fileexecution.FinalAbsent {
		t.Fatalf("initial final = (%v, %v)", observed.Condition(), err)
	}

	stageNode := &fakeNode{id: 900, size: fileAuthorityExactSize}
	owned := &liveFileAuthorityOwned{
		fileAuthorityOwnedFile: &fileAuthorityOwnedFile{object: fixture.object},
		native:                 &fakeFile{node: stageNode},
	}
	if observed, err := destination.ObserveOwnedFinal(
		context.Background(), owned, fixture.expectation,
	); err != nil || observed.Condition() != fileexecution.FinalAbsent {
		t.Fatalf("pre-publish owned final = (%v, %v)", observed.Condition(), err)
	}
	observed, err := fixture.destination.PublishNoReplace(
		context.Background(), owned, fixture.expectation,
	)
	if err != nil || observed.Condition() != fileexecution.FinalOwnedExact {
		t.Fatalf("live publish = (%v, %v)", observed.Condition(), err)
	}
	if observed, err = destination.ObserveOwnedFinal(
		context.Background(), owned, fixture.expectation,
	); err != nil || observed.Condition() != fileexecution.FinalOwnedExact {
		t.Fatalf("published owned final = (%v, %v)", observed.Condition(), err)
	}
	if observed, err = fixture.destination.PublishNoReplace(
		context.Background(), owned, fixture.expectation,
	); err != nil || observed.Condition() != fileexecution.FinalOwnedExact {
		t.Fatalf("idempotent live publish = (%v, %v)", observed.Condition(), err)
	}
	if observed, err = fixture.destination.ObserveFinalPresence(context.Background()); err != nil ||
		observed.Condition() != fileexecution.FinalCollision {
		t.Fatalf("presence-only final = (%v, %v)", observed.Condition(), err)
	}

	if _, err := destination.ObserveOwnedFinal(
		context.Background(),
		&fileAuthorityOwnedFile{object: fixture.object},
		fixture.expectation,
	); !errors.Is(err, ErrInvalidClaim) {
		t.Fatalf("non-native live object error = %v", err)
	}
	otherObject, _ := fileAuthorityObject(t, 0x92)
	if _, err := fixture.destination.PublishNoReplace(
		context.Background(),
		&liveFileAuthorityOwned{
			fileAuthorityOwnedFile: &fileAuthorityOwnedFile{object: otherObject},
			native:                 owned.native,
		},
		fixture.expectation,
	); !errors.Is(err, ErrInvalidClaim) {
		t.Fatalf("foreign live object error = %v", err)
	}

	fixture.platform.mu.Lock()
	fixture.parent.entries[fixture.leaf] = fakeEntry{
		kind: outputcap.EntryRegularFile,
		node: &fakeNode{id: 901, size: fileAuthorityExactSize},
	}
	fixture.platform.mu.Unlock()
	if observed, err = destination.ObserveOwnedFinal(
		context.Background(), owned, fixture.expectation,
	); err != nil || observed.Condition() != fileexecution.FinalCollision {
		t.Fatalf("replaced live final = (%v, %v)", observed.Condition(), err)
	}
}

type captureDirectoryExecutor struct {
	outputsession.DirectoryExecutor
	captured outputsession.DirectoryClaim
}

func (c *captureDirectoryExecutor) MaterializeDirectory(
	ctx context.Context,
	claim outputsession.DirectoryClaim,
) (outputsession.DirectoryMaterialization, error) {
	c.captured = claim
	return c.DirectoryExecutor.MaterializeDirectory(ctx, claim)
}

func TestAuthorityOwnedDirectoryID(t *testing.T) {
	platform := newFileAuthorityPlatform()
	platform.addDirectory(platform.rootNode(), "folder")
	directories, err := New(platform, Config{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = directories.Close() })
	capture := &captureDirectoryExecutor{DirectoryExecutor: directories}
	sessionID := testIdentity[transfer.OutputSessionID](151)
	share := testIdentity[catalog.ShareInstance](131)
	rootID := testIdentity[catalog.DirectoryID](141)
	rules, err := transfer.NewSelectionRules(true, nil)
	if err != nil {
		t.Fatal(err)
	}
	intent := testDirectTreeIntent(t, share, rootID, rules)
	session, err := outputsession.New(outputsession.Config{
		Intent:    intent,
		SessionID: sessionID,
		Capabilities: transfer.DirectTreeCapabilities{
			Durability:   transfer.DurabilityPowerLoss,
			ModifiedTime: true,
		},
		ReceiptSecret: bytes.Repeat([]byte{0x71}, 32),
		Locator:       directories,
		Destinations: outputsession.ArtifactDestinationBinderFunc(func(path ordinaryoutput.ArtifactPath) (outputsession.DestinationPath, error) {
			return outputsession.NewDestinationPath(path.String())
		}),
		Directories: capture,
		Files:       &fileAuthorityClaimCapture{authority: &FileAuthority{}},
		Resources: outputsession.ResourceReleaserFunc(func(context.Context) error {
			return nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	rootSource := testSourceDirectory(
		t, rootID, testIdentity[catalog.DirectoryGeneration](161),
		transfer.DirectoryAdmission{}, "", catalog.ModifiedTime{},
	)
	rootAdmission, err := session.AdmitDirectory(
		context.Background(), projectedDirectoryRequest(
			t, intent, rootSource, transfer.MaterializedDirectoryClaim{},
		),
	)
	if err != nil {
		t.Fatal(err)
	}
	directorySource := testSourceDirectory(
		t, testIdentity[catalog.DirectoryID](171), testIdentity[catalog.DirectoryGeneration](181),
		rootAdmission, "folder", catalog.ModifiedTime{},
	)
	_, err = session.AdmitDirectory(
		context.Background(), projectedDirectoryRequest(
			t, intent, directorySource, transfer.MaterializedDirectoryClaim{},
		),
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := directories.OwnedDirectoryID(outputsession.DirectoryClaim{}); !errors.Is(err, ErrInvalidClaim) {
		t.Fatalf("empty claim error = %v", err)
	}
	ownedID, err := directories.OwnedDirectoryID(capture.captured)
	if err != nil || ownedID.IsZero() {
		t.Fatalf("OwnedDirectoryID on captured claim = (%x, %v)", ownedID.Bytes(), err)
	}
}
