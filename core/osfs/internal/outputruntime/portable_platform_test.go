package outputruntime

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/transfer"
)

var (
	portableRuntimeFilesystems   sync.Map
	portableRuntimeUnsafePrivate sync.Map
)

func incrementalTestIdentity16[T ~[catalog.IdentityBytes]byte](value byte) T {
	var identity T
	for index := range identity {
		identity[index] = value
	}
	return identity
}

type runtimeTestRootSpec struct {
	path string
}

// The portable model keeps ordinary path-level test corruption visible while
// replacing native certification, fixed-handle, and sync costs with explicit
// identities and locks. That lets the runtime's recovery policy stay in short
// tests without pretending the double proves platform durability.
type portableRuntimeFilesystem struct {
	root string

	mu         sync.Mutex
	nextObject uint64
	objects    []portableRuntimeObject
	locks      map[uint64]bool
}

type portableRuntimeObject struct {
	id   uint64
	info os.FileInfo
}

func newRuntimeTestRootSpec(t testing.TB) runtimeTestRootSpec {
	t.Helper()
	root, err := filepath.Abs(t.TempDir())
	if err != nil {
		t.Fatalf("resolve portable output-runtime root: %v", err)
	}
	filesystem := &portableRuntimeFilesystem{
		root:  filepath.Clean(root),
		locks: make(map[uint64]bool),
	}
	portableRuntimeFilesystems.Store(filesystem.root, filesystem)
	t.Cleanup(func() {
		portableRuntimeFilesystems.Delete(filesystem.root)
		portableRuntimeUnsafePrivate.Range(func(key, _ any) bool {
			path, ok := key.(string)
			if ok && portableRuntimePathWithinRoot(filesystem.root, path) {
				portableRuntimeUnsafePrivate.Delete(key)
			}
			return true
		})
	})
	return runtimeTestRootSpec{path: filesystem.root}
}

func openOutputRuntimeTestPlatform(path string, create bool) (outputcap.Platform, error) {
	root, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	root = filepath.Clean(root)
	value, ok := portableRuntimeFilesystems.Load(root)
	if !ok {
		filesystem := &portableRuntimeFilesystem{
			root:  root,
			locks: make(map[uint64]bool),
		}
		value, _ = portableRuntimeFilesystems.LoadOrStore(root, filesystem)
	}
	filesystem := value.(*portableRuntimeFilesystem)
	_, beforeErr := os.Stat(root)
	disposition := outputcap.CallerProvidedContainer
	if create {
		if err := os.Mkdir(root, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
			return nil, err
		}
		if errors.Is(beforeErr, fs.ErrNotExist) {
			disposition = outputcap.AuthorityCreatedRoot
		}
	}
	info, err := os.Stat(root)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, outputcap.ErrUnsafeNamespace
	}
	return &portableRuntimePlatform{filesystem: filesystem, rootInfo: info, disposition: disposition}, nil
}

func (filesystem *portableRuntimeFilesystem) objectID(info os.FileInfo) uint64 {
	filesystem.mu.Lock()
	defer filesystem.mu.Unlock()
	return filesystem.objectIDLocked(info)
}

func (filesystem *portableRuntimeFilesystem) objectIDLocked(info os.FileInfo) uint64 {
	for _, object := range filesystem.objects {
		if os.SameFile(object.info, info) {
			return object.id
		}
	}
	filesystem.nextObject++
	filesystem.objects = append(filesystem.objects, portableRuntimeObject{
		id:   filesystem.nextObject,
		info: info,
	})
	return filesystem.nextObject
}

type portableRuntimePlatform struct {
	filesystem  *portableRuntimeFilesystem
	rootInfo    os.FileInfo
	disposition outputcap.RootOpenDisposition

	mu     sync.Mutex
	closed bool
}

func (*portableRuntimePlatform) DestinationCapabilities() (outputcap.DestinationCapabilities, error) {
	supported := outputcap.SupportedCapability()
	return outputcap.NewDestinationCapabilities(supported, supported, supported, supported)
}

func (*portableRuntimePlatform) LiveCleanupNativeProfile() checkpointmodel.LiveCleanupNativeProfile {
	return checkpointmodel.LiveCleanupLinuxExt4V1
}

func (platform *portableRuntimePlatform) RootOpenDisposition() outputcap.RootOpenDisposition {
	if platform == nil {
		return ""
	}
	return platform.disposition
}

func (platform *portableRuntimePlatform) usable() error {
	if platform == nil || platform.filesystem == nil || platform.rootInfo == nil {
		return fs.ErrClosed
	}
	platform.mu.Lock()
	defer platform.mu.Unlock()
	if platform.closed {
		return fs.ErrClosed
	}
	return nil
}

func (platform *portableRuntimePlatform) Root() outputcap.Directory {
	if platform.usable() != nil {
		return nil
	}
	return newPortableRuntimeDirectory(platform.filesystem, platform.filesystem.root, platform.rootInfo)
}

func (platform *portableRuntimePlatform) AcquirePublicOperationGuard() (outputcap.PublicOperationGuard, error) {
	if err := platform.usable(); err != nil {
		return nil, err
	}
	return &portableRuntimeGuard{root: platform.Root()}, nil
}

func (platform *portableRuntimePlatform) RootBinding() (outputcap.OutputRootBinding, error) {
	if err := platform.usable(); err != nil {
		return outputcap.OutputRootBinding{}, err
	}
	identity := fmt.Appendf(
		nil,
		"root:%s:object:%d",
		platform.filesystem.root,
		platform.filesystem.objectID(platform.rootInfo),
	)
	return outputcap.NewOutputRootBinding(
		platform.Certification(),
		[]byte("portable-test-volume"),
		identity,
	)
}

func (*portableRuntimePlatform) Certification() outputcap.CertificationID {
	// The runtime state codec accepts only production certification IDs. The
	// portable double reuses one valid ID solely to exercise codec/state policy;
	// native certification itself remains owned by the long tests.
	return outputcap.CertificationLinuxExt4ProcessRestart
}

func (*portableRuntimePlatform) Durability() transfer.DurabilityLevel {
	return transfer.DurabilityProcessRestart
}

func (platform *portableRuntimePlatform) ProbeRecoverableFeatures() error {
	return platform.usable()
}

func (platform *portableRuntimePlatform) ValidateModifiedTime(catalog.ModifiedTime) error {
	return platform.usable()
}

func (platform *portableRuntimePlatform) CanonicalLocatorKey(value string) (string, error) {
	if err := platform.usable(); err != nil {
		return "", err
	}
	canonical, err := catalog.CanonicalPath(value)
	if err != nil || canonical != value {
		return "", errors.Join(outputcap.ErrUnsafeNamespace, err)
	}
	return strings.ToLower(canonical), nil
}

func (platform *portableRuntimePlatform) CanonicalComponentKey(value string) (string, error) {
	if err := platform.usable(); err != nil {
		return "", err
	}
	if err := validatePortableRuntimeName(value); err != nil {
		return "", err
	}
	return strings.ToLower(value), nil
}

func (platform *portableRuntimePlatform) Close() error {
	if platform == nil {
		return nil
	}
	platform.mu.Lock()
	defer platform.mu.Unlock()
	platform.closed = true
	return nil
}

type portableRuntimeGuard struct {
	root outputcap.Directory

	mu     sync.Mutex
	closed bool
}

func (guard *portableRuntimeGuard) Root() outputcap.Directory {
	guard.mu.Lock()
	defer guard.mu.Unlock()
	if guard.closed {
		return nil
	}
	return guard.root
}

func (guard *portableRuntimeGuard) Close() error {
	if guard == nil {
		return nil
	}
	guard.mu.Lock()
	defer guard.mu.Unlock()
	guard.closed = true
	guard.root = nil
	return nil
}

type portableRuntimeDirectory struct {
	filesystem *portableRuntimeFilesystem
	info       os.FileInfo

	mu     sync.Mutex
	path   string
	closed bool
}

func newPortableRuntimeDirectory(
	filesystem *portableRuntimeFilesystem,
	path string,
	info os.FileInfo,
) *portableRuntimeDirectory {
	return &portableRuntimeDirectory{filesystem: filesystem, path: path, info: info}
}

func (directory *portableRuntimeDirectory) currentPath() (string, error) {
	if directory == nil || directory.filesystem == nil || directory.info == nil {
		return "", fs.ErrClosed
	}
	directory.mu.Lock()
	defer directory.mu.Unlock()
	if directory.closed {
		return "", fs.ErrClosed
	}
	path, err := directory.filesystem.findObjectPath(directory.info, directory.path)
	if err != nil {
		return "", err
	}
	directory.path = path
	return path, nil
}

func (directory *portableRuntimeDirectory) Close() error {
	if directory == nil {
		return nil
	}
	directory.mu.Lock()
	defer directory.mu.Unlock()
	directory.closed = true
	return nil
}

func (directory *portableRuntimeDirectory) Duplicate() (outputcap.Directory, error) {
	path, err := directory.currentPath()
	if err != nil {
		return nil, err
	}
	return newPortableRuntimeDirectory(directory.filesystem, path, directory.info), nil
}

func (directory *portableRuntimeDirectory) Sync() error {
	_, err := directory.currentPath()
	return err
}

func (directory *portableRuntimeDirectory) Names(limit int) ([]string, error) {
	path, err := directory.currentPath()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	sort.Strings(names)
	if limit >= 0 && len(names) > limit {
		names = names[:limit]
	}
	return names, nil
}

func (directory *portableRuntimeDirectory) ObserveEntry(name string) (outputcap.EntryKind, error) {
	kind, _, err := directory.ClassifyExactEntry(name)
	return kind, err
}

func (directory *portableRuntimeDirectory) ClassifyExactEntry(
	name string,
) (outputcap.EntryKind, bool, error) {
	path, err := directory.currentPath()
	if err != nil {
		return outputcap.EntryAbsent, false, err
	}
	if err := validatePortableRuntimeName(name); err != nil {
		return outputcap.EntryAbsent, false, err
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return outputcap.EntryAbsent, false, err
	}
	for _, entry := range entries {
		if !strings.EqualFold(entry.Name(), name) {
			continue
		}
		kind, kindErr := portableRuntimeEntryKind(filepath.Join(path, entry.Name()))
		return kind, entry.Name() == name, kindErr
	}
	return outputcap.EntryAbsent, true, nil
}

func (directory *portableRuntimeDirectory) ValidatePublicEntryNames(names []string) error {
	for _, name := range names {
		if err := validatePortableRuntimeName(name); err != nil {
			return err
		}
	}
	return nil
}

func (directory *portableRuntimeDirectory) ValidateCreateAuthority() error {
	_, err := directory.currentPath()
	return err
}

func (directory *portableRuntimeDirectory) ValidateMetadataAuthority() error {
	_, err := directory.currentPath()
	return err
}

func (directory *portableRuntimeDirectory) PreparePersistentDirectoryIdentityClaim() ([]byte, error) {
	return directory.PersistentDirectoryIdentityClaim()
}

func (directory *portableRuntimeDirectory) PersistentDirectoryIdentityClaim() ([]byte, error) {
	if _, err := directory.currentPath(); err != nil {
		return nil, err
	}
	// The portable model uses SameFile-backed object numbering so rename-safe
	// ownership tests exercise identity semantics instead of path coincidence.
	return fmt.Appendf(nil, "portable-directory-object:%d", directory.filesystem.objectID(directory.info)), nil
}

func (directory *portableRuntimeDirectory) OpenEntry(name string) (outputcap.CurrentEntryReference, error) {
	path, err := directory.entryPath(name)
	if err != nil {
		return nil, err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	return &portableRuntimeEntryReference{
		filesystem: directory.filesystem,
		path:       path,
		info:       info,
		kind:       portableRuntimeKind(info),
	}, nil
}

func (directory *portableRuntimeDirectory) EntryMatches(
	name string,
	expected outputcap.CurrentEntryReference,
) (bool, error) {
	path, err := directory.entryPath(name)
	if err != nil {
		return false, err
	}
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	reference, ok := expected.(*portableRuntimeEntryReference)
	if !ok || reference == nil || reference.isClosed() || reference.filesystem != directory.filesystem {
		return false, nil
	}
	return os.SameFile(info, reference.info), nil
}

func (directory *portableRuntimeDirectory) OpenPinnedDirectory(
	expected outputcap.CurrentEntryReference,
	private bool,
) (outputcap.Directory, error) {
	reference, ok := expected.(*portableRuntimeEntryReference)
	if !ok || reference == nil || reference.isClosed() ||
		reference.filesystem != directory.filesystem || reference.kind != outputcap.EntryDirectory {
		return nil, outputcap.ErrUnsafeNamespace
	}
	path, err := directory.filesystem.findObjectPath(reference.info, reference.path)
	if err != nil {
		return nil, errors.Join(outputcap.ErrUnsafeNamespace, err)
	}
	if private && !portableRuntimePrivateEnvelopeSafe(path) {
		return nil, outputcap.ErrUnsafeNamespace
	}
	return newPortableRuntimeDirectory(directory.filesystem, path, reference.info), nil
}

func (directory *portableRuntimeDirectory) RemoveEntry(
	name string,
	expected outputcap.CurrentEntryReference,
) error {
	matched, err := directory.EntryMatches(name, expected)
	if err != nil {
		return err
	}
	if !matched {
		return outputcap.ErrUnsafeNamespace
	}
	path, err := directory.entryPath(name)
	if err != nil {
		return err
	}
	return os.Remove(path)
}

func (directory *portableRuntimeDirectory) SameDirectory(other outputcap.Directory) (bool, error) {
	peer, ok := other.(*portableRuntimeDirectory)
	if !ok || peer == nil || directory.filesystem != peer.filesystem {
		return false, nil
	}
	if _, err := directory.currentPath(); err != nil {
		return false, err
	}
	if _, err := peer.currentPath(); err != nil {
		return false, err
	}
	return os.SameFile(directory.info, peer.info), nil
}

func (directory *portableRuntimeDirectory) SetModifiedTime(modified catalog.ModifiedTime) error {
	path, err := directory.currentPath()
	if err != nil {
		return err
	}
	return portableRuntimeSetModifiedTime(path, modified)
}

func (directory *portableRuntimeDirectory) OpenDirectory(
	name string,
	private bool,
) (outputcap.Directory, error) {
	path, err := directory.entryPath(name)
	if err != nil {
		return nil, err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 ||
		private && !portableRuntimePrivateEnvelopeSafe(path) {
		return nil, outputcap.ErrUnsafeNamespace
	}
	return newPortableRuntimeDirectory(directory.filesystem, path, info), nil
}

func (directory *portableRuntimeDirectory) CreateOrdinaryOutputStage(
	proof outputcap.Directory,
	name string,
	exactSize uint64,
) error {
	if proof == nil || name == "" || exactSize > uint64(^uint64(0)>>1) {
		return outputcap.ErrUnsafeNamespace
	}
	file, err := proof.CreateFile(name, false, int64(exactSize))
	if err != nil {
		return err
	}
	return file.Close()
}

func (directory *portableRuntimeDirectory) CreateLiveCleanupStage(
	proof outputcap.Directory,
	ticket checkpointmodel.LiveCleanupTicket,
) error {
	if !ticket.Valid() || proof == nil {
		return outputcap.ErrUnsafeNamespace
	}
	file, err := proof.CreateFile(ticket.StageName(), false, int64(ticket.ExactSize()))
	if err != nil {
		return err
	}
	return file.Close()
}

func (directory *portableRuntimeDirectory) RemoveLiveCleanupStage(
	ticket checkpointmodel.LiveCleanupTicket,
	expected outputcap.FileIdentity,
) error {
	if !ticket.Valid() {
		return outputcap.ErrUnsafeNamespace
	}
	return directory.RemoveFile(ticket.StageName(), expected)
}

func (directory *portableRuntimeDirectory) ReservePublicDirectoryNoReplace(
	name string,
) (outputcap.Directory, outputcap.PublishNoReplaceOutcome, error) {
	created, err := directory.CreateDirectory(name, false)
	if errors.Is(err, outputcap.ErrNamespaceCollision) {
		return nil, outputcap.PublishNoReplaceCollision, nil
	}
	if err != nil || created == nil {
		return created, 0, err
	}
	if err := errors.Join(created.Sync(), directory.Sync()); err != nil {
		return created, outputcap.PublishNoReplaceIndeterminate, err
	}
	return created, outputcap.PublishNoReplaceCommitted, nil
}

func (directory *portableRuntimeDirectory) PublishFileNoReplace(
	source outputcap.FileIdentity,
	name string,
) (outputcap.PublishNoReplaceOutcome, error) {
	published, err := directory.LinkFileNoReplace(source, name)
	if errors.Is(err, outputcap.ErrNamespaceCollision) {
		return outputcap.PublishNoReplaceCollision, nil
	}
	if err != nil || published == nil {
		return 0, err
	}
	durable, durabilityErr := directory.OpenRecoveryDurabilityFile(name, false)
	if durabilityErr != nil || durable == nil {
		return outputcap.PublishNoReplaceIndeterminate, errors.Join(
			durabilityErr, published.Close(),
		)
	}
	return outputcap.PublishNoReplaceCommitted, errors.Join(
		durable.Sync(), durable.Close(), published.Close(), directory.Sync(),
	)
}

func (directory *portableRuntimeDirectory) CreateDirectory(
	name string,
	_ bool,
) (outputcap.Directory, error) {
	path, err := directory.entryPath(name)
	if err != nil {
		return nil, err
	}
	if err := os.Mkdir(path, 0o700); err != nil {
		return nil, portableRuntimeMutationError(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	return newPortableRuntimeDirectory(directory.filesystem, path, info), nil
}

func (directory *portableRuntimeDirectory) InstallDirectoryNoReplace(
	candidate outputcap.Directory,
	name string,
) (outputcap.Directory, error) {
	target, err := directory.entryPath(name)
	if err != nil {
		return nil, err
	}
	sourcePath, sourceInfo, err := directory.filesystem.findMatchingDirectory(candidate)
	if err != nil {
		return nil, err
	}
	if _, err := os.Lstat(target); err == nil {
		return nil, outputcap.ErrNamespaceCollision
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	if err := os.Rename(sourcePath, target); err != nil {
		return nil, portableRuntimeMutationError(err)
	}
	return newPortableRuntimeDirectory(directory.filesystem, target, sourceInfo), nil
}

func (directory *portableRuntimeDirectory) RemoveDirectory(
	name string,
	expected outputcap.Directory,
) error {
	current, err := directory.OpenDirectory(name, true)
	if err != nil {
		return err
	}
	defer current.Close()
	matched, err := current.SameDirectory(expected)
	if err != nil {
		return err
	}
	if !matched {
		return outputcap.ErrUnsafeNamespace
	}
	path, err := directory.entryPath(name)
	if err != nil {
		return err
	}
	return os.Remove(path)
}

func (directory *portableRuntimeDirectory) CreateFile(
	name string,
	_ bool,
	size int64,
) (outputcap.MutableFile, error) {
	if size < 0 {
		return nil, outputcap.ErrUnsafeNamespace
	}
	path, err := directory.entryPath(name)
	if err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, portableRuntimeMutationError(err)
	}
	if err := file.Truncate(size); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return nil, err
	}
	return newPortableRuntimeFile(directory.filesystem, path, file)
}

func (directory *portableRuntimeDirectory) OpenObservedFile(
	name string,
	private bool,
) (outputcap.ObservedFile, error) {
	return directory.openFile(name, private, false)
}

func (directory *portableRuntimeDirectory) OpenRecoveryDurabilityFile(
	name string,
	private bool,
) (outputcap.RecoveryDurabilityFile, error) {
	return directory.openFile(name, private, true)
}

func (directory *portableRuntimeDirectory) OpenMutableFile(
	name string,
	private bool,
) (outputcap.MutableFile, error) {
	return directory.openFile(name, private, true)
}

func (directory *portableRuntimeDirectory) openFile(
	name string,
	private bool,
	writable bool,
) (*portableRuntimeFile, error) {
	path, err := directory.entryPath(name)
	if err != nil {
		return nil, err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || private && !portableRuntimePrivateEnvelopeSafe(path) {
		return nil, outputcap.ErrUnsafeNamespace
	}
	flags := os.O_RDONLY
	if writable {
		flags = os.O_RDWR
	}
	file, err := os.OpenFile(path, flags, 0)
	if err != nil {
		return nil, err
	}
	return newPortableRuntimeFile(directory.filesystem, path, file)
}

func (directory *portableRuntimeDirectory) LinkFileNoReplace(
	source outputcap.FileIdentity,
	name string,
) (outputcap.ObservedFile, error) {
	target, err := directory.entryPath(name)
	if err != nil {
		return nil, err
	}
	var sourcePath string
	if direct, ok := source.(*portableRuntimeFile); ok && direct.filesystem == directory.filesystem {
		sourcePath, err = direct.fixedLinkSourcePath()
	} else {
		sourcePath, _, err = directory.filesystem.findMatchingFile(source)
	}
	if err != nil {
		return nil, err
	}
	if err := os.Link(sourcePath, target); err != nil {
		return nil, portableRuntimeMutationError(err)
	}
	file, err := os.OpenFile(target, os.O_RDWR, 0)
	if err != nil {
		return nil, err
	}
	return newPortableRuntimeFile(directory.filesystem, target, file)
}

func (directory *portableRuntimeDirectory) ReplacePrivateFile(
	source outputcap.FileIdentity,
	name string,
) error {
	target, err := directory.entryPath(name)
	if err != nil {
		return err
	}
	targetInfo, err := os.Lstat(target)
	if err != nil {
		return err
	}
	if !targetInfo.Mode().IsRegular() {
		return outputcap.ErrUnsafeNamespace
	}
	sourcePath, _, err := directory.filesystem.findMatchingFile(source)
	if err != nil {
		return err
	}
	if err := os.Remove(target); err != nil {
		return err
	}
	return portableRuntimeMutationError(os.Rename(sourcePath, target))
}

func (directory *portableRuntimeDirectory) RemoveFile(name string, expected outputcap.FileIdentity) error {
	path, err := directory.entryPath(name)
	if err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return outputcap.ErrUnsafeNamespace
	}
	current, err := os.Open(path)
	if err != nil {
		return err
	}
	actual, err := newPortableRuntimeFile(directory.filesystem, path, current)
	if err != nil {
		return err
	}
	defer actual.Close()
	matched, err := expected.SameFile(actual)
	if err != nil {
		return err
	}
	if !matched {
		return outputcap.ErrUnsafeNamespace
	}
	return os.Remove(path)
}

func (directory *portableRuntimeDirectory) AcquireLock(
	name string,
	existingOnly bool,
) (outputcap.Lock, bool, error) {
	path, err := directory.entryPath(name)
	if err != nil {
		return nil, false, err
	}
	if !portableRuntimePrivateEnvelopeSafe(path) {
		return nil, false, outputcap.ErrUnsafeNamespace
	}
	file, created, err := portableRuntimeOpenLockFile(path, existingOnly)
	if err != nil {
		return nil, false, err
	}
	capability, err := newPortableRuntimeFile(directory.filesystem, path, file)
	if err != nil {
		_ = file.Close()
		return nil, false, err
	}
	identity := directory.filesystem.objectID(capability.info)
	directory.filesystem.mu.Lock()
	if directory.filesystem.locks[identity] {
		directory.filesystem.mu.Unlock()
		_ = capability.Close()
		return nil, false, outputcap.ErrNamespaceLockBusy
	}
	directory.filesystem.locks[identity] = true
	directory.filesystem.mu.Unlock()
	return &portableRuntimeLock{
		filesystem: directory.filesystem,
		identity:   identity,
		file:       capability,
	}, created, nil
}

func (directory *portableRuntimeDirectory) entryPath(name string) (string, error) {
	if err := validatePortableRuntimeName(name); err != nil {
		return "", err
	}
	path, err := directory.currentPath()
	if err != nil {
		return "", err
	}
	return filepath.Join(path, name), nil
}

func markPortableRuntimePrivateEnvelopeUnsafe(path string) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return
	}
	portableRuntimeUnsafePrivate.Store(filepath.Clean(absolute), struct{}{})
}

func TestPortableRuntimeTracksUnsafePrivateEnvelopes(t *testing.T) {
	t.Parallel()
	root := newRuntimeTestRootSpec(t)
	path := filepath.Join(root.path, "private")
	if !portableRuntimePrivateEnvelopeSafe(path) {
		t.Fatal("new private envelope is unsafe")
	}
	markPortableRuntimePrivateEnvelopeUnsafe(path)
	if portableRuntimePrivateEnvelopeSafe(path) {
		t.Fatal("marked private envelope is safe")
	}
}

func portableRuntimePrivateEnvelopeSafe(path string) bool {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	_, unsafe := portableRuntimeUnsafePrivate.Load(filepath.Clean(absolute))
	return !unsafe
}

func portableRuntimePathWithinRoot(root, path string) bool {
	relative, err := filepath.Rel(root, path)
	return err == nil && relative != ".." &&
		!strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func validatePortableRuntimeName(name string) error {
	if name == "" || name == "." || name == ".." ||
		strings.ContainsAny(name, `/\`) || strings.IndexByte(name, 0) >= 0 {
		return outputcap.ErrUnsafeNamespace
	}
	return nil
}

func portableRuntimeEntryKind(path string) (outputcap.EntryKind, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return outputcap.EntryAbsent, nil
	}
	if err != nil {
		return outputcap.EntryAbsent, err
	}
	return portableRuntimeKind(info), nil
}

func portableRuntimeKind(info os.FileInfo) outputcap.EntryKind {
	switch {
	case info == nil:
		return outputcap.EntryAbsent
	case info.Mode().IsRegular():
		return outputcap.EntryRegularFile
	case info.IsDir():
		return outputcap.EntryDirectory
	default:
		return outputcap.EntryOther
	}
}

func portableRuntimeMutationError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, fs.ErrExist):
		return errors.Join(outputcap.ErrNamespaceCollision, err)
	default:
		return err
	}
}

func portableRuntimeSetModifiedTime(path string, modified catalog.ModifiedTime) error {
	if !modified.Present() {
		return nil
	}
	value := time.Unix(modified.Seconds(), int64(modified.Nanoseconds()))
	return os.Chtimes(path, value, value)
}

func portableRuntimeModifiedTimeMatches(actual time.Time, expected catalog.ModifiedTime) bool {
	if actual.Unix() != expected.Seconds() {
		return false
	}
	switch expected.Precision() {
	case catalog.TimePrecisionSeconds:
		return true
	case catalog.TimePrecisionMilliseconds:
		return actual.Nanosecond()/1_000_000 == int(expected.Nanoseconds())/1_000_000
	case catalog.TimePrecisionNanoseconds:
		return actual.Nanosecond() == int(expected.Nanoseconds())
	default:
		return false
	}
}

var (
	_ outputcap.Platform                   = (*portableRuntimePlatform)(nil)
	_ outputcap.Directory                  = (*portableRuntimeDirectory)(nil)
	_ outputcap.PublicEntryNamesValidator  = (*portableRuntimeDirectory)(nil)
	_ outputcap.CreateAuthorityValidator   = (*portableRuntimeDirectory)(nil)
	_ outputcap.MetadataAuthorityValidator = (*portableRuntimeDirectory)(nil)
	_ outputcap.MutableFile                = (*portableRuntimeFile)(nil)
	_ io.ReaderAt                          = (*portableRuntimeFile)(nil)
)
