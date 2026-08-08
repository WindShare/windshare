package directoryauthority

import (
	"errors"
	"io/fs"
	"sort"
	"strings"
	"sync"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/osfs/internal/outputcap"
)

var errFakeUnsupported = errors.New("fake directory operation is unsupported")

type fakeCreatePlan struct {
	err          error
	mutate       bool
	returnHandle bool
}

type fakeEntry struct {
	kind    outputcap.EntryKind
	node    *fakeNode
	aliases []string
}

type fakeNode struct {
	id      uint64
	entries map[string]fakeEntry
	size    uint64

	namesCalls     int
	lastNamesLimit int
	createCalls    int
	syncCalls      int
	setCalls       int

	nextCreate           *fakeCreatePlan
	syncErr              error
	setErr               error
	setMutates           bool
	metadataMatches      *bool
	metadataObserveErr   error
	createAuthorityErr   error
	metadataAuthorityErr error
	modified             catalog.ModifiedTime
}

type fakePlatform struct {
	mu sync.Mutex

	root          *fakeNode
	nextID        uint64
	disposition   outputcap.RootOpenDisposition
	guardErr      error
	guardCloseErr error
	modifiedErr   error
	guardCalls    int
	openEntryHook func()
}

func newFakePlatform(disposition outputcap.RootOpenDisposition) *fakePlatform {
	platform := &fakePlatform{nextID: 2, disposition: disposition}
	platform.root = &fakeNode{id: 1, entries: make(map[string]fakeEntry)}
	return platform
}

func (platform *fakePlatform) RootOpenDisposition() outputcap.RootOpenDisposition {
	return platform.disposition
}

func (platform *fakePlatform) ValidateModifiedTime(catalog.ModifiedTime) error {
	platform.mu.Lock()
	defer platform.mu.Unlock()
	return platform.modifiedErr
}

func (platform *fakePlatform) CanonicalLocatorKey(path string) (string, error) {
	if path == "" {
		return rootLocatorKey, nil
	}
	parts := strings.Split(path, "/")
	for index, part := range parts {
		key, err := platform.CanonicalComponentKey(part)
		if err != nil {
			return "", err
		}
		parts[index] = key
	}
	return strings.Join(parts, "/"), nil
}

func (platform *fakePlatform) CanonicalComponentKey(name string) (string, error) {
	if name == "" || strings.ContainsAny(name, "/\\\x00") {
		return "", ErrInvalidLocator
	}
	key := strings.ToUpper(strings.TrimRight(name, " ."))
	if key == "" {
		return "", ErrInvalidLocator
	}
	return key, nil
}

func (platform *fakePlatform) AcquirePublicOperationGuard() (outputcap.PublicOperationGuard, error) {
	platform.mu.Lock()
	defer platform.mu.Unlock()
	platform.guardCalls++
	if platform.guardErr != nil {
		return nil, platform.guardErr
	}
	return &fakeGuard{
		root: &fakeDirectory{platform: platform, node: platform.root}, closeErr: platform.guardCloseErr,
	}, nil
}

func (platform *fakePlatform) addDirectory(parent *fakeNode, name string, aliases ...string) *fakeNode {
	platform.mu.Lock()
	defer platform.mu.Unlock()
	return platform.addDirectoryLocked(parent, name, aliases)
}

func (platform *fakePlatform) addDirectoryLocked(parent *fakeNode, name string, aliases []string) *fakeNode {
	node := &fakeNode{id: platform.nextID, entries: make(map[string]fakeEntry)}
	platform.nextID++
	parent.entries[name] = fakeEntry{kind: outputcap.EntryDirectory, node: node, aliases: append([]string(nil), aliases...)}
	return node
}

func (platform *fakePlatform) replaceDirectory(parent *fakeNode, name string) *fakeNode {
	platform.mu.Lock()
	defer platform.mu.Unlock()
	return platform.addDirectoryLocked(parent, name, nil)
}

func (platform *fakePlatform) addFile(parent *fakeNode, name string, size uint64) *fakeNode {
	platform.mu.Lock()
	defer platform.mu.Unlock()
	node := &fakeNode{id: platform.nextID, size: size}
	platform.nextID++
	parent.entries[name] = fakeEntry{kind: outputcap.EntryRegularFile, node: node}
	return node
}

func (platform *fakePlatform) rootNode() *fakeNode {
	platform.mu.Lock()
	defer platform.mu.Unlock()
	return platform.root
}

func (platform *fakePlatform) setOpenEntryHook(hook func()) {
	platform.mu.Lock()
	defer platform.mu.Unlock()
	platform.openEntryHook = hook
}

func (platform *fakePlatform) runOpenEntryHook() {
	platform.mu.Lock()
	hook := platform.openEntryHook
	platform.openEntryHook = nil
	platform.mu.Unlock()
	if hook != nil {
		hook()
	}
}

type fakeGuard struct {
	root     *fakeDirectory
	closeErr error
	mu       sync.Mutex
	closed   bool
}

func (guard *fakeGuard) Root() outputcap.Directory { return guard.root }

func (guard *fakeGuard) Close() error {
	guard.mu.Lock()
	defer guard.mu.Unlock()
	if guard.closed {
		return nil
	}
	guard.closed = true
	return guard.closeErr
}

type fakeDirectory struct {
	platform *fakePlatform
	node     *fakeNode
	mu       sync.Mutex
	closed   bool
}

func (directory *fakeDirectory) Close() error {
	directory.mu.Lock()
	directory.closed = true
	directory.mu.Unlock()
	return nil
}

func (directory *fakeDirectory) Duplicate() (outputcap.Directory, error) {
	if directory == nil || directory.node == nil {
		return nil, errFakeUnsupported
	}
	return &fakeDirectory{platform: directory.platform, node: directory.node}, nil
}

func (directory *fakeDirectory) Sync() error {
	directory.platform.mu.Lock()
	defer directory.platform.mu.Unlock()
	directory.node.syncCalls++
	return directory.node.syncErr
}

func (directory *fakeDirectory) Names(limit int) ([]string, error) {
	directory.platform.mu.Lock()
	defer directory.platform.mu.Unlock()
	directory.node.namesCalls++
	directory.node.lastNamesLimit = limit
	if limit < 0 || len(directory.node.entries) > limit {
		return nil, ErrParentSnapshotUnavailable
	}
	names := make([]string, 0, len(directory.node.entries))
	for name := range directory.node.entries {
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}

func (directory *fakeDirectory) ObserveEntry(name string) (outputcap.EntryKind, error) {
	kind, _, err := directory.ClassifyExactEntry(name)
	return kind, err
}

func (directory *fakeDirectory) ClassifyExactEntry(name string) (outputcap.EntryKind, bool, error) {
	directory.platform.mu.Lock()
	defer directory.platform.mu.Unlock()
	entry, actual, exists := directory.resolveLocked(name)
	if !exists {
		return outputcap.EntryAbsent, true, nil
	}
	return entry.kind, actual == name, nil
}

func (directory *fakeDirectory) resolveLocked(name string) (fakeEntry, string, bool) {
	wanted, err := directory.platform.CanonicalComponentKey(name)
	if err != nil {
		return fakeEntry{}, "", false
	}
	for actual, entry := range directory.node.entries {
		key, _ := directory.platform.CanonicalComponentKey(actual)
		if key == wanted {
			return entry, actual, true
		}
		for _, alias := range entry.aliases {
			key, _ = directory.platform.CanonicalComponentKey(alias)
			if key == wanted {
				return entry, actual, true
			}
		}
	}
	return fakeEntry{}, "", false
}

func (directory *fakeDirectory) OpenEntry(name string) (outputcap.CurrentEntryReference, error) {
	directory.platform.runOpenEntryHook()
	directory.platform.mu.Lock()
	defer directory.platform.mu.Unlock()
	entry, actual, exists := directory.resolveLocked(name)
	if !exists || actual != name {
		return nil, ErrRetainedAuthorityChanged
	}
	return &fakeEntryReference{kind: entry.kind, node: entry.node}, nil
}

func (directory *fakeDirectory) EntryMatches(name string, expected outputcap.CurrentEntryReference) (bool, error) {
	reference, ok := expected.(*fakeEntryReference)
	if !ok {
		return false, errFakeUnsupported
	}
	directory.platform.mu.Lock()
	defer directory.platform.mu.Unlock()
	entry, actual, exists := directory.resolveLocked(name)
	return exists && actual == name && entry.node == reference.node && entry.kind == reference.kind, nil
}

func (directory *fakeDirectory) OpenPinnedDirectory(
	expected outputcap.CurrentEntryReference,
	_ bool,
) (outputcap.Directory, error) {
	reference, ok := expected.(*fakeEntryReference)
	if !ok || reference.kind != outputcap.EntryDirectory || reference.node == nil {
		return nil, errFakeUnsupported
	}
	return &fakeDirectory{platform: directory.platform, node: reference.node}, nil
}

func (directory *fakeDirectory) RemoveEntry(string, outputcap.CurrentEntryReference) error {
	return errFakeUnsupported
}

func (directory *fakeDirectory) SameDirectory(other outputcap.Directory) (bool, error) {
	candidate, ok := other.(*fakeDirectory)
	return ok && candidate != nil && candidate.node == directory.node, nil
}

func (directory *fakeDirectory) SetModifiedTime(modified catalog.ModifiedTime) error {
	directory.platform.mu.Lock()
	defer directory.platform.mu.Unlock()
	directory.node.setCalls++
	if directory.node.setErr == nil || directory.node.setMutates {
		directory.node.modified = modified
	}
	return directory.node.setErr
}

func (directory *fakeDirectory) MetadataMatches(modified catalog.ModifiedTime) (bool, error) {
	directory.platform.mu.Lock()
	defer directory.platform.mu.Unlock()
	if directory.node.metadataObserveErr != nil {
		return false, directory.node.metadataObserveErr
	}
	if directory.node.metadataMatches != nil {
		return *directory.node.metadataMatches, nil
	}
	return directory.node.modified == modified, nil
}

func (directory *fakeDirectory) OpenDirectory(name string, _ bool) (outputcap.Directory, error) {
	reference, err := directory.OpenEntry(name)
	if err != nil {
		return nil, err
	}
	defer reference.Close()
	return directory.OpenPinnedDirectory(reference, false)
}

func (directory *fakeDirectory) CreateDirectory(name string, _ bool) (outputcap.Directory, error) {
	directory.platform.mu.Lock()
	defer directory.platform.mu.Unlock()
	directory.node.createCalls++
	if plan := directory.node.nextCreate; plan != nil {
		directory.node.nextCreate = nil
		var node *fakeNode
		if plan.mutate {
			node = directory.platform.addDirectoryLocked(directory.node, name, nil)
		}
		if plan.returnHandle && node != nil {
			return &fakeDirectory{platform: directory.platform, node: node}, plan.err
		}
		return nil, plan.err
	}
	if _, _, exists := directory.resolveLocked(name); exists {
		return nil, outputcap.ErrNamespaceCollision
	}
	node := directory.platform.addDirectoryLocked(directory.node, name, nil)
	return &fakeDirectory{platform: directory.platform, node: node}, nil
}

func (directory *fakeDirectory) InstallDirectoryNoReplace(outputcap.Directory, string) (outputcap.Directory, error) {
	return nil, errFakeUnsupported
}

func (directory *fakeDirectory) RemoveDirectory(string, outputcap.Directory) error {
	return errFakeUnsupported
}

func (directory *fakeDirectory) CreateFile(string, bool, int64) (outputcap.File, error) {
	return nil, errFakeUnsupported
}

func (directory *fakeDirectory) OpenFile(name string, _ bool, _ bool) (outputcap.File, error) {
	directory.platform.mu.Lock()
	defer directory.platform.mu.Unlock()
	entry, actual, exists := directory.resolveLocked(name)
	if !exists {
		return nil, fs.ErrNotExist
	}
	if actual != name || entry.kind != outputcap.EntryRegularFile || entry.node == nil {
		return nil, errFakeUnsupported
	}
	return &fakeFile{node: entry.node}, nil
}

func (directory *fakeDirectory) LinkFileNoReplace(outputcap.File, string) (outputcap.File, error) {
	return nil, errFakeUnsupported
}

func (directory *fakeDirectory) ReplacePrivateFile(outputcap.File, string) error {
	return errFakeUnsupported
}

func (directory *fakeDirectory) RemoveFile(string, outputcap.File) error {
	return errFakeUnsupported
}

func (directory *fakeDirectory) AcquireLock(string, bool) (outputcap.Lock, bool, error) {
	return nil, false, errFakeUnsupported
}

func (directory *fakeDirectory) ValidateCreateAuthority() error {
	directory.platform.mu.Lock()
	defer directory.platform.mu.Unlock()
	return directory.node.createAuthorityErr
}

func (directory *fakeDirectory) ValidateMetadataAuthority() error {
	directory.platform.mu.Lock()
	defer directory.platform.mu.Unlock()
	return directory.node.metadataAuthorityErr
}

type fakeEntryReference struct {
	kind outputcap.EntryKind
	node *fakeNode
}

func (reference *fakeEntryReference) Kind() outputcap.EntryKind { return reference.kind }
func (reference *fakeEntryReference) Close() error              { return nil }

type fakeFile struct {
	outputcap.File
	node *fakeNode
}

func (*fakeFile) Close() error { return nil }

func (file *fakeFile) Size() (uint64, error) {
	if file == nil || file.node == nil {
		return 0, errFakeUnsupported
	}
	return file.node.size, nil
}

func (file *fakeFile) SameFile(other outputcap.File) (bool, error) {
	peer, ok := other.(*fakeFile)
	return ok && file != nil && peer != nil && file.node == peer.node, nil
}

type fakeAliasSnapshotter struct {
	mu      sync.Mutex
	entries []PublicEntryName
	calls   int
	limit   int
	after   func()
}

func (snapshotter *fakeAliasSnapshotter) SnapshotPublicEntryNames(
	_ outputcap.Directory,
	limit int,
) ([]PublicEntryName, error) {
	snapshotter.mu.Lock()
	snapshotter.calls++
	snapshotter.limit = limit
	entries := append([]PublicEntryName(nil), snapshotter.entries...)
	after := snapshotter.after
	snapshotter.mu.Unlock()
	if after != nil {
		after()
	}
	return entries, nil
}

func (snapshotter *fakeAliasSnapshotter) count() int {
	snapshotter.mu.Lock()
	defer snapshotter.mu.Unlock()
	return snapshotter.calls
}
