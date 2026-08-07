package outputnamespace

import (
	"bytes"
	"errors"
	"io/fs"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/osfs/internal/resumestate"
	"github.com/windshare/windshare/core/transfer"
)

// memoryCapabilityFS models handle-relative identity separately from names. That
// distinction is why namespace tests can expose replacement races without using
// a platform backend or weakening outputcap's production boundary.
type memoryCapabilityFS struct {
	mu      sync.Mutex
	nextID  uint64
	root    *memoryCapabilityNode
	binding resumestate.OutputRootBinding
}

type memoryCapabilityNode struct {
	id       uint64
	kind     outputcap.EntryKind
	children map[string]*memoryCapabilityNode
	data     []byte
	modified catalog.ModifiedTime
	locked   bool
}

var filesystemOutputBackendID = func() transfer.OutputBackendID {
	return transfer.NativeFilesystemOutputBackendID
}()

func v3RecoveryRoot(t *testing.T) *memoryCapabilityFS {
	t.Helper()
	return newMemoryCapabilityFS(t)
}

func openOutputV3Platform(
	filesystem *memoryCapabilityFS,
	_ bool,
) (outputcap.Platform, error) {
	return filesystem.platform(), nil
}

func v3RecoveryIdentity16[T ~[catalog.IdentityBytes]byte](value byte) T {
	var identity T
	for index := range identity {
		identity[index] = value
	}
	return identity
}

func v3RecoveryIntentDigest(selection transfer.OutputSelection) transfer.TransferIntentDigest {
	if selection.ShareInstance().IsZero() || selection.SyntheticRoot().IsZero() {
		fallback, err := transfer.NewOutputSelection(
			v3RecoveryIdentity16[catalog.ShareInstance](1),
			v3RecoveryIdentity16[catalog.DirectoryID](2),
			v3RecoveryIdentity16[catalog.DirectoryGeneration](3), nil, nil,
		)
		if err != nil {
			panic(err)
		}
		selection = fallback
	}
	rules, err := transfer.NewSelectionRules(true, nil)
	if err != nil {
		panic(err)
	}
	target, err := transfer.NewOpaqueOutputTarget(bytes.Repeat([]byte{0x4d}, transfer.OutputRootIdentityBytes))
	if err != nil {
		panic(err)
	}
	intent, err := transfer.NewTransferIntent(
		selection.ShareInstance(), selection.SyntheticRoot(), rules, target,
		transfer.NativeFilesystemOutputBackendID, transfer.OutputNativeTree,
	)
	if err != nil {
		panic(err)
	}
	return intent.Digest()
}

func v3RecoveryModifiedTime(t *testing.T) catalog.ModifiedTime {
	t.Helper()
	modified, err := catalog.NewModifiedTime(1_700_000_000, 0, catalog.TimePrecisionSeconds)
	if err != nil {
		t.Fatal(err)
	}
	return modified
}

func v3RecoverySelection(t *testing.T, withFile bool, exactSize uint64) transfer.OutputSelection {
	t.Helper()
	var paths []string
	if withFile {
		paths = []string{"file.bin"}
	}
	share := v3RecoveryIdentity16[catalog.ShareInstance](1)
	root := v3RecoveryIdentity16[catalog.DirectoryID](2)
	generation := v3RecoveryIdentity16[catalog.DirectoryGeneration](3)
	files := make([]transfer.OutputSelectionFile, 0, len(paths))
	for index, path := range paths {
		files = append(files, transfer.OutputSelectionFile{
			Path: path, FileID: v3RecoveryIdentity16[catalog.FileID](byte(4 + index)),
			ParentDirectoryID: root, ParentGeneration: generation,
			ExpectedSize: exactSize, ModifiedTime: v3RecoveryModifiedTime(t),
		})
	}
	plan, err := transfer.NewOutputSelection(share, root, generation, nil, files)
	if err != nil {
		t.Fatal(err)
	}
	rules, err := transfer.NewSelectionRules(true, nil)
	if err != nil {
		t.Fatal(err)
	}
	request, err := transfer.NewCanonicalSelectionRequest(share, root, rules)
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := transfer.NewTerminalSelectionObservationV1(request, plan)
	if err != nil {
		t.Fatal(err)
	}
	selection, err := canonical.BindPlan(plan)
	if err != nil {
		t.Fatal(err)
	}
	return selection
}

func v3RecoveryAncestryBinding(
	t *testing.T,
	root resumestate.OutputRootBinding,
	selection transfer.OutputSelection,
) resumestate.OutputAncestryBinding {
	t.Helper()
	binding, err := resumestate.NewOutputAncestryBinding(
		root,
		selection.Identity(),
		[]resumestate.OutputAncestryIdentityClaim{{
			CanonicalPath: "", IdentityClaim: []byte("test-root-ancestry"),
		}},
	)
	if err != nil {
		t.Fatal(err)
	}
	return binding
}

type v3RecoverySessionIDs struct {
	mu   sync.Mutex
	next byte
}

func (generator *v3RecoverySessionIDs) NewOutputSessionID() (transfer.OutputSessionID, error) {
	generator.mu.Lock()
	defer generator.mu.Unlock()
	generator.next++
	return v3RecoveryIdentity16[transfer.OutputSessionID](generator.next), nil
}

func v3RecoveryAuthority(
	t *testing.T,
	_ *memoryCapabilityFS,
	sessions *v3RecoverySessionIDs,
) Controller {
	t.Helper()
	if sessions == nil {
		sessions = &v3RecoverySessionIDs{}
	}
	return NewController(ControllerConfig{
		Backend:      filesystemOutputBackendID,
		IntentDigest: v3RecoveryIntentDigest(transfer.OutputSelection{}),
		Random:       bytes.NewReader(bytes.Repeat([]byte{0xa5}, 64*1024)),
		SessionIDs:   sessions,
	})
}

type testStateSession struct {
	platform   outputcap.Platform
	sessionDir outputcap.Directory
	state      resumestate.SessionAuthority
	store      Store
}

func newTestStateSession(t *testing.T, selection transfer.OutputSelection) *testStateSession {
	t.Helper()
	filesystem := newMemoryCapabilityFS(t)
	platform := filesystem.platform()
	root, err := platform.RootBinding()
	if err != nil {
		t.Fatal(err)
	}
	control, err := resumestate.NewControl(resumestate.ControlSpec{
		Backend: filesystemOutputBackendID, OutputRoot: root,
		Certification: platform.Certification(), Durability: platform.Durability(), Generation: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	sessionID := v3RecoveryIdentity16[transfer.OutputSessionID](0x61)
	intent := v3RecoveryIntentDigest(selection)
	header, err := resumestate.NewHeader(resumestate.HeaderSpec{
		Backend: filesystemOutputBackendID, SessionID: sessionID, IntentDigest: intent,
		Selection: selection, OutputRoot: root, OutputAncestry: v3RecoveryAncestryBinding(t, root, selection),
	})
	if err != nil {
		t.Fatal(err)
	}
	state, err := resumestate.BindSessionAuthority(
		control, header, selection,
		resumestate.IntentNamespaceName(intent),
		resumestate.SessionDirectoryName(sessionID),
	)
	if err != nil {
		t.Fatal(err)
	}
	sessionDirectory, err := platform.Root().CreateDirectory("state-session", true)
	if err != nil {
		t.Fatal(err)
	}
	store := NewStore(StoreConfig{Random: strings.NewReader(strings.Repeat("q", 4096))})
	encoded, err := resumestate.EncodeHeader(header)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.EnsureInitialRecord(
		sessionDirectory, resumestate.HeaderRecordName, encoded, resumestate.MaxSessionHeaderBytes,
	); err != nil {
		t.Fatal(err)
	}
	return &testStateSession{
		platform: platform, sessionDir: sessionDirectory, state: state, store: store,
	}
}

func (session *testStateSession) close(t *testing.T) {
	t.Helper()
	if err := errors.Join(session.sessionDir.Close(), session.platform.Close()); err != nil {
		t.Error(err)
	}
}

func v3RecoveryOutputFile(
	t *testing.T,
	_ *testStateSession,
	selection transfer.OutputSelection,
	exactSize uint64,
) transfer.OutputFile {
	t.Helper()
	if len(selection.Files()) != 1 || selection.Files()[0].ExpectedSize != exactSize {
		t.Fatal("recovery output file differs from its canonical selection")
	}
	selected := selection.Files()[0]
	geometry, err := content.NewFileGeometry(selected.ExpectedSize, catalog.DefaultChunkSize)
	if err != nil {
		t.Fatal(err)
	}
	descriptor, err := content.NewFileRevisionDescriptor(
		selection.ShareInstance(), selected.FileID,
		v3RecoveryIdentity16[content.FileRevision](5), geometry, selected.ModifiedTime,
	)
	if err != nil {
		t.Fatal(err)
	}
	return transfer.OutputFile{Path: selected.Path, ExpectedSize: selected.ExpectedSize, Descriptor: descriptor}
}

func newMemoryCapabilityFS(t *testing.T) *memoryCapabilityFS {
	t.Helper()
	binding, err := resumestate.NewOutputRootBinding(
		resumestate.CertificationWindowsNTFSProcessRestart,
		[]byte("outputnamespace-test-volume"),
		[]byte("outputnamespace-test-root"),
	)
	if err != nil {
		t.Fatal(err)
	}
	filesystem := &memoryCapabilityFS{nextID: 1, binding: binding}
	filesystem.root = &memoryCapabilityNode{
		id: filesystem.nextID, kind: outputcap.EntryDirectory,
		children: make(map[string]*memoryCapabilityNode),
	}
	return filesystem
}

func (filesystem *memoryCapabilityFS) newNode(kind outputcap.EntryKind) *memoryCapabilityNode {
	filesystem.nextID++
	node := &memoryCapabilityNode{id: filesystem.nextID, kind: kind}
	if kind == outputcap.EntryDirectory {
		node.children = make(map[string]*memoryCapabilityNode)
	}
	return node
}

func (filesystem *memoryCapabilityFS) platform() *memoryCapabilityPlatform {
	return &memoryCapabilityPlatform{filesystem: filesystem}
}

type memoryCapabilityPlatform struct {
	filesystem *memoryCapabilityFS
	closed     bool
}

func (platform *memoryCapabilityPlatform) Root() outputcap.Directory {
	return &memoryCapabilityDirectory{filesystem: platform.filesystem, node: platform.filesystem.root}
}

func (platform *memoryCapabilityPlatform) AcquirePublicOperationGuard() (outputcap.PublicOperationGuard, error) {
	return &memoryCapabilityGuard{root: platform.Root()}, nil
}

func (platform *memoryCapabilityPlatform) RootBinding() (resumestate.OutputRootBinding, error) {
	return platform.filesystem.binding, nil
}

func (*memoryCapabilityPlatform) Certification() resumestate.CertificationID {
	return resumestate.CertificationWindowsNTFSProcessRestart
}

func (*memoryCapabilityPlatform) Durability() transfer.DurabilityLevel {
	return transfer.DurabilityProcessRestart
}

func (*memoryCapabilityPlatform) ProbeRecoverableFeatures() error { return nil }
func (*memoryCapabilityPlatform) ValidateSelectionMetadata(transfer.OutputSelection) error {
	return nil
}
func (*memoryCapabilityPlatform) ValidateModifiedTime(catalog.ModifiedTime) error { return nil }
func (*memoryCapabilityPlatform) CanonicalLocatorKey(value string) (string, error) {
	return strings.ToUpper(value), nil
}
func (*memoryCapabilityPlatform) CanonicalComponentKey(value string) (string, error) {
	return strings.ToUpper(value), nil
}
func (platform *memoryCapabilityPlatform) Close() error {
	if platform == nil {
		return nil
	}
	platform.closed = true
	return nil
}

type memoryCapabilityGuard struct{ root outputcap.Directory }

func (guard *memoryCapabilityGuard) Root() outputcap.Directory { return guard.root }
func (*memoryCapabilityGuard) Close() error                    { return nil }

type memoryCapabilityDirectory struct {
	filesystem *memoryCapabilityFS
	node       *memoryCapabilityNode
	closed     bool
}

func (directory *memoryCapabilityDirectory) Close() error {
	if directory == nil {
		return nil
	}
	directory.closed = true
	return nil
}

func (directory *memoryCapabilityDirectory) Duplicate() (outputcap.Directory, error) {
	if err := directory.usable(); err != nil {
		return nil, err
	}
	return &memoryCapabilityDirectory{filesystem: directory.filesystem, node: directory.node}, nil
}

func (directory *memoryCapabilityDirectory) Sync() error { return directory.usable() }

func (directory *memoryCapabilityDirectory) Names(limit int) ([]string, error) {
	if err := directory.usable(); err != nil {
		return nil, err
	}
	directory.filesystem.mu.Lock()
	defer directory.filesystem.mu.Unlock()
	names := make([]string, 0, len(directory.node.children))
	for name := range directory.node.children {
		names = append(names, name)
	}
	slices.Sort(names)
	if limit >= 0 && len(names) > limit {
		names = names[:limit]
	}
	return names, nil
}

func (directory *memoryCapabilityDirectory) NamesWithPrefix(prefix string, limit int) ([]string, error) {
	names, err := directory.Names(int(^uint(0) >> 1))
	if err != nil {
		return nil, err
	}
	matched := names[:0]
	for _, name := range names {
		if strings.HasPrefix(name, prefix) {
			matched = append(matched, name)
		}
	}
	if limit >= 0 && len(matched) > limit {
		matched = matched[:limit]
	}
	return matched, nil
}

func (directory *memoryCapabilityDirectory) ObserveEntry(name string) (outputcap.EntryKind, error) {
	kind, _, err := directory.ClassifyExactEntry(name)
	return kind, err
}

func (directory *memoryCapabilityDirectory) ClassifyExactEntry(name string) (outputcap.EntryKind, bool, error) {
	if err := directory.usable(); err != nil {
		return outputcap.EntryAbsent, false, err
	}
	directory.filesystem.mu.Lock()
	defer directory.filesystem.mu.Unlock()
	node, ok := directory.node.children[name]
	if !ok {
		return outputcap.EntryAbsent, true, nil
	}
	return node.kind, true, nil
}

func (*memoryCapabilityDirectory) ValidatePublicEntryName(name string) error {
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, `/\\`) {
		return outputcap.ErrUnsafeNamespace
	}
	return nil
}

func (directory *memoryCapabilityDirectory) PrepareIdentityClaim() (outputcap.PersistentDirectoryIdentity, error) {
	return directory.IdentityClaim()
}

func (directory *memoryCapabilityDirectory) IdentityClaim() (outputcap.PersistentDirectoryIdentity, error) {
	if err := directory.usable(); err != nil {
		return outputcap.PersistentDirectoryIdentity{}, err
	}
	return outputcap.NewPersistentDirectoryIdentity([]byte(directory.identity())), nil
}

func (directory *memoryCapabilityDirectory) OpenEntry(name string) (outputcap.CurrentEntryReference, error) {
	if err := directory.usable(); err != nil {
		return nil, err
	}
	directory.filesystem.mu.Lock()
	defer directory.filesystem.mu.Unlock()
	node, ok := directory.node.children[name]
	if !ok {
		return nil, fs.ErrNotExist
	}
	return &memoryCapabilityEntryReference{filesystem: directory.filesystem, node: node}, nil
}

func (directory *memoryCapabilityDirectory) EntryMatches(name string, expected outputcap.CurrentEntryReference) (bool, error) {
	if err := directory.usable(); err != nil {
		return false, err
	}
	directory.filesystem.mu.Lock()
	node, ok := directory.node.children[name]
	directory.filesystem.mu.Unlock()
	if !ok || expected == nil {
		return false, nil
	}
	reference, ok := expected.(*memoryCapabilityEntryReference)
	return ok && reference.node == node && !reference.closed, nil
}

func (directory *memoryCapabilityDirectory) OpenPinnedDirectory(
	expected outputcap.CurrentEntryReference,
	_ bool,
) (outputcap.Directory, error) {
	reference, ok := expected.(*memoryCapabilityEntryReference)
	if !ok || reference.closed || reference.node.kind != outputcap.EntryDirectory {
		return nil, outputcap.ErrUnsafeNamespace
	}
	return &memoryCapabilityDirectory{filesystem: directory.filesystem, node: reference.node}, nil
}

func (directory *memoryCapabilityDirectory) RemoveEntry(name string, expected outputcap.CurrentEntryReference) error {
	if err := directory.usable(); err != nil {
		return err
	}
	directory.filesystem.mu.Lock()
	defer directory.filesystem.mu.Unlock()
	node, ok := directory.node.children[name]
	if !ok {
		return fs.ErrNotExist
	}
	reference, ok := expected.(*memoryCapabilityEntryReference)
	if !ok || reference.node != node || reference.closed {
		return outputcap.ErrUnsafeNamespace
	}
	delete(directory.node.children, name)
	return nil
}

func (directory *memoryCapabilityDirectory) SameDirectory(other outputcap.Directory) (bool, error) {
	if err := directory.usable(); err != nil {
		return false, err
	}
	if other == nil {
		return false, nil
	}
	identity, err := other.IdentityClaim()
	if err != nil {
		return false, err
	}
	return identity.Equal(outputcap.NewPersistentDirectoryIdentity([]byte(directory.identity()))), nil
}

func (*memoryCapabilityDirectory) SetModifiedTime(catalog.ModifiedTime) error { return nil }

func (directory *memoryCapabilityDirectory) OpenDirectory(name string, _ bool) (outputcap.Directory, error) {
	if err := directory.usable(); err != nil {
		return nil, err
	}
	directory.filesystem.mu.Lock()
	defer directory.filesystem.mu.Unlock()
	node, ok := directory.node.children[name]
	if !ok {
		return nil, fs.ErrNotExist
	}
	if node.kind != outputcap.EntryDirectory {
		return nil, outputcap.ErrUnsafeNamespace
	}
	return &memoryCapabilityDirectory{filesystem: directory.filesystem, node: node}, nil
}

func (directory *memoryCapabilityDirectory) CreateDirectory(name string, _ bool) (outputcap.Directory, error) {
	if err := directory.usable(); err != nil {
		return nil, err
	}
	directory.filesystem.mu.Lock()
	defer directory.filesystem.mu.Unlock()
	if _, exists := directory.node.children[name]; exists {
		return nil, outputcap.ErrNamespaceCollision
	}
	node := directory.filesystem.newNode(outputcap.EntryDirectory)
	directory.node.children[name] = node
	return &memoryCapabilityDirectory{filesystem: directory.filesystem, node: node}, nil
}

func (directory *memoryCapabilityDirectory) InstallDirectoryNoReplace(
	candidate outputcap.Directory,
	name string,
) (outputcap.Directory, error) {
	if err := directory.usable(); err != nil {
		return nil, err
	}
	directory.filesystem.mu.Lock()
	defer directory.filesystem.mu.Unlock()
	if _, exists := directory.node.children[name]; exists {
		return nil, outputcap.ErrNamespaceCollision
	}
	node, err := directoryNode(candidate)
	if err != nil {
		return nil, err
	}
	for candidateName, existing := range directory.node.children {
		if candidateName != name && existing == node {
			delete(directory.node.children, candidateName)
		}
	}
	directory.node.children[name] = node
	return &memoryCapabilityDirectory{filesystem: directory.filesystem, node: node}, nil
}

func (directory *memoryCapabilityDirectory) RemoveDirectory(name string, expected outputcap.Directory) error {
	if err := directory.usable(); err != nil {
		return err
	}
	directory.filesystem.mu.Lock()
	node, ok := directory.node.children[name]
	if !ok {
		directory.filesystem.mu.Unlock()
		return fs.ErrNotExist
	}
	if node.kind != outputcap.EntryDirectory || len(node.children) != 0 {
		directory.filesystem.mu.Unlock()
		return outputcap.ErrUnsafeNamespace
	}
	directory.filesystem.mu.Unlock()
	matched, err := expected.SameDirectory(&memoryCapabilityDirectory{filesystem: directory.filesystem, node: node})
	if err != nil || !matched {
		return errors.Join(outputcap.ErrUnsafeNamespace, err)
	}
	directory.filesystem.mu.Lock()
	defer directory.filesystem.mu.Unlock()
	if directory.node.children[name] != node {
		return outputcap.ErrUnsafeNamespace
	}
	delete(directory.node.children, name)
	return nil
}

type outputV3CandidateSessionIDs struct {
	err  error
	zero bool
	next byte
}

func (ids *outputV3CandidateSessionIDs) NewOutputSessionID() (transfer.OutputSessionID, error) {
	if ids.err != nil {
		return transfer.OutputSessionID{}, ids.err
	}
	if ids.zero {
		return transfer.OutputSessionID{}, nil
	}
	ids.next++
	return v3RecoveryIdentity16[transfer.OutputSessionID](ids.next), nil
}
