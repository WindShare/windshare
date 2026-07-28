package outputnamespace

import (
	"errors"
	"io"
	"io/fs"
	"strings"
	"sync"
	"testing"

	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/osfs/internal/resumestate"
	"github.com/windshare/windshare/core/transfer"
)

func writeControlFixtureFile(
	t *testing.T,
	filesystem *memoryCapabilityFS,
	directoryName string,
	fileName string,
	data []byte,
) {
	t.Helper()
	platform := filesystem.platform()
	directory, err := platform.Root().OpenDirectory(directoryName, true)
	if err != nil {
		t.Fatal(err)
	}
	file, err := directory.OpenFile(fileName, true, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(int64(len(data))); err != nil {
		t.Fatal(err)
	}
	if len(data) != 0 {
		written, writeErr := file.WriteAt(data, 0)
		if writeErr == nil && written != len(data) {
			writeErr = io.ErrShortWrite
		}
		if writeErr != nil {
			t.Fatal(writeErr)
		}
	}
	if err := errors.Join(file.Sync(), directory.Sync(), file.Close(), directory.Close(), platform.Close()); err != nil {
		t.Fatal(err)
	}
}

func outputV3ControlSessionRequireFault(
	t *testing.T,
	err error,
	scope transfer.OutputFaultScope,
	code transfer.OutputFaultCode,
) {
	t.Helper()
	var fault *transfer.OutputFault
	if !errors.As(err, &fault) || fault.Scope() != scope || fault.Code() != code {
		t.Fatalf("output fault = %#v in %v, want scope=%v code=%v", fault, err, scope, code)
	}
}

const (
	outputV3CSRootBinding      = "root-binding"
	outputV3CSNames            = "names"
	outputV3CSNamesWithPrefix  = "names-with-prefix"
	outputV3CSObserveEntry     = "observe-entry"
	outputV3CSClassifyEntry    = "classify-entry"
	outputV3CSOpenDirectory    = "open-directory"
	outputV3CSCreateDirectory  = "create-directory"
	outputV3CSInstallDirectory = "install-directory"
	outputV3CSRemoveDirectory  = "remove-directory"
	outputV3CSOpenFile         = "open-file"
	outputV3CSCreateFile       = "create-file"
	outputV3CSRemoveFile       = "remove-file"
	outputV3CSAcquireLock      = "acquire-lock"
	outputV3CSSameDirectory    = "same-directory"
	outputV3CSSync             = "sync"
)

type outputV3ControlSessionFaultPlan struct {
	mu             sync.Mutex
	operation      string
	path           string
	name           string
	namePrefix     bool
	atCall         int
	seen           int
	fired          int
	failure        error
	forceDifferent bool
	forceCreated   bool
}

func outputV3ControlSessionFailure(operation, path, name string) *outputV3ControlSessionFaultPlan {
	return &outputV3ControlSessionFaultPlan{
		operation: operation, path: path, name: name, failure: errOutputV3ControlSessionInjected,
	}
}

func (plan *outputV3ControlSessionFaultPlan) trigger(operation, path, name string) (bool, error) {
	if plan == nil {
		return false, nil
	}
	plan.mu.Lock()
	defer plan.mu.Unlock()
	nameMatches := name == plan.name
	if plan.namePrefix {
		nameMatches = strings.HasPrefix(name, plan.name)
	}
	if operation != plan.operation || path != plan.path || !nameMatches {
		return false, nil
	}
	plan.seen++
	atCall := plan.atCall
	if atCall == 0 {
		atCall = 1
	}
	if plan.seen != atCall {
		return false, nil
	}
	plan.fired++
	return true, plan.failure
}

func (plan *outputV3ControlSessionFaultPlan) requireFired(t *testing.T) {
	t.Helper()
	plan.mu.Lock()
	defer plan.mu.Unlock()
	if plan.fired != 1 {
		t.Fatalf("fault %s path=%q name=%q fired %d times, want once", plan.operation, plan.path, plan.name, plan.fired)
	}
}

type outputV3ControlSessionFaultPlatform struct {
	outputcap.Platform
	root outputcap.Directory
	plan *outputV3ControlSessionFaultPlan
}

func openOutputV3ControlSessionFaultPlatform(
	t *testing.T,
	root *memoryCapabilityFS,
	plan *outputV3ControlSessionFaultPlan,
) outputcap.Platform {
	t.Helper()
	platform, err := openOutputV3Platform(root, false)
	if err != nil {
		t.Fatal(err)
	}
	return wrapOutputV3ControlSessionFaultPlatform(platform, plan)
}

func wrapOutputV3ControlSessionFaultPlatform(
	platform outputcap.Platform,
	plan *outputV3ControlSessionFaultPlan,
) outputcap.Platform {
	return &outputV3ControlSessionFaultPlatform{
		Platform: platform,
		root:     wrapOutputV3ControlSessionFaultDirectory(platform.Root(), plan, ""),
		plan:     plan,
	}
}

func (platform *outputV3ControlSessionFaultPlatform) Root() outputcap.Directory { return platform.root }

func (platform *outputV3ControlSessionFaultPlatform) AcquirePublicOperationGuard() (
	outputcap.PublicOperationGuard,
	error,
) {
	return platform.Platform.AcquirePublicOperationGuard()
}

func (platform *outputV3ControlSessionFaultPlatform) RootBinding() (resumestate.OutputRootBinding, error) {
	if matched, err := platform.plan.trigger(outputV3CSRootBinding, "", ""); matched {
		return resumestate.OutputRootBinding{}, err
	}
	return platform.Platform.RootBinding()
}

type outputV3ControlSessionFaultDirectory struct {
	outputcap.Directory
	plan *outputV3ControlSessionFaultPlan
	path string
}

func wrapOutputV3ControlSessionFaultDirectory(
	directory outputcap.Directory,
	plan *outputV3ControlSessionFaultPlan,
	path string,
) outputcap.Directory {
	if directory == nil {
		return nil
	}
	return &outputV3ControlSessionFaultDirectory{Directory: directory, plan: plan, path: path}
}

func unwrapOutputV3ControlSessionFaultDirectory(directory outputcap.Directory) outputcap.Directory {
	if wrapped, ok := directory.(*outputV3ControlSessionFaultDirectory); ok {
		return wrapped.Directory
	}
	return directory
}

func outputV3ControlSessionChildPath(parent, name string) string {
	if parent == "" {
		return name
	}
	return parent + "/" + name
}

func (directory *outputV3ControlSessionFaultDirectory) Duplicate() (outputcap.Directory, error) {
	duplicate, err := directory.Directory.Duplicate()
	return wrapOutputV3ControlSessionFaultDirectory(duplicate, directory.plan, directory.path), err
}

func (directory *outputV3ControlSessionFaultDirectory) SameDirectory(other outputcap.Directory) (bool, error) {
	if matched, err := directory.plan.trigger(outputV3CSSameDirectory, directory.path, ""); matched {
		if err != nil {
			return false, err
		}
		return !directory.plan.forceDifferent, nil
	}
	return directory.Directory.SameDirectory(unwrapOutputV3ControlSessionFaultDirectory(other))
}

func (directory *outputV3ControlSessionFaultDirectory) Names(limit int) ([]string, error) {
	if matched, err := directory.plan.trigger(outputV3CSNames, directory.path, ""); matched {
		return nil, err
	}
	return directory.Directory.Names(limit)
}

func (directory *outputV3ControlSessionFaultDirectory) NamesWithPrefix(
	prefix string,
	limit int,
) ([]string, error) {
	if matched, err := directory.plan.trigger(outputV3CSNamesWithPrefix, directory.path, prefix); matched {
		return nil, err
	}
	return directory.Directory.NamesWithPrefix(prefix, limit)
}

func (directory *outputV3ControlSessionFaultDirectory) ObserveEntry(name string) (outputcap.EntryKind, error) {
	if matched, err := directory.plan.trigger(outputV3CSObserveEntry, directory.path, name); matched {
		return outputcap.EntryAbsent, err
	}
	return directory.Directory.ObserveEntry(name)
}

func (directory *outputV3ControlSessionFaultDirectory) ClassifyExactEntry(
	name string,
) (outputcap.EntryKind, bool, error) {
	if matched, err := directory.plan.trigger(outputV3CSClassifyEntry, directory.path, name); matched {
		return outputcap.EntryAbsent, false, err
	}
	return directory.Directory.ClassifyExactEntry(name)
}

func (directory *outputV3ControlSessionFaultDirectory) OpenDirectory(
	name string,
	private bool,
) (outputcap.Directory, error) {
	if matched, err := directory.plan.trigger(outputV3CSOpenDirectory, directory.path, name); matched {
		return nil, err
	}
	opened, err := directory.Directory.OpenDirectory(name, private)
	return wrapOutputV3ControlSessionFaultDirectory(
		opened, directory.plan, outputV3ControlSessionChildPath(directory.path, name),
	), err
}

func (directory *outputV3ControlSessionFaultDirectory) OpenPinnedDirectory(
	expected outputcap.CurrentEntryReference,
	private bool,
) (outputcap.Directory, error) {
	opened, err := directory.Directory.OpenPinnedDirectory(expected, private)
	return wrapOutputV3ControlSessionFaultDirectory(opened, directory.plan, directory.path), err
}

func (directory *outputV3ControlSessionFaultDirectory) CreateDirectory(
	name string,
	private bool,
) (outputcap.Directory, error) {
	if matched, err := directory.plan.trigger(outputV3CSCreateDirectory, directory.path, name); matched {
		return nil, err
	}
	created, err := directory.Directory.CreateDirectory(name, private)
	return wrapOutputV3ControlSessionFaultDirectory(
		created, directory.plan, outputV3ControlSessionChildPath(directory.path, name),
	), err
}

func (directory *outputV3ControlSessionFaultDirectory) InstallDirectoryNoReplace(
	candidate outputcap.Directory,
	name string,
) (outputcap.Directory, error) {
	if matched, err := directory.plan.trigger(outputV3CSInstallDirectory, directory.path, name); matched {
		return nil, err
	}
	installed, err := directory.Directory.InstallDirectoryNoReplace(
		unwrapOutputV3ControlSessionFaultDirectory(candidate), name,
	)
	return wrapOutputV3ControlSessionFaultDirectory(
		installed, directory.plan, outputV3ControlSessionChildPath(directory.path, name),
	), err
}

func (directory *outputV3ControlSessionFaultDirectory) RemoveDirectory(
	name string,
	expected outputcap.Directory,
) error {
	if matched, err := directory.plan.trigger(outputV3CSRemoveDirectory, directory.path, name); matched {
		return err
	}
	return directory.Directory.RemoveDirectory(
		name, unwrapOutputV3ControlSessionFaultDirectory(expected),
	)
}

func (directory *outputV3ControlSessionFaultDirectory) OpenFile(
	name string,
	private bool,
	writable bool,
) (outputcap.File, error) {
	if matched, err := directory.plan.trigger(outputV3CSOpenFile, directory.path, name); matched {
		return nil, err
	}
	return directory.Directory.OpenFile(name, private, writable)
}

func (directory *outputV3ControlSessionFaultDirectory) CreateFile(
	name string,
	private bool,
	size int64,
) (outputcap.File, error) {
	if matched, err := directory.plan.trigger(outputV3CSCreateFile, directory.path, name); matched {
		return nil, err
	}
	return directory.Directory.CreateFile(name, private, size)
}

func (directory *outputV3ControlSessionFaultDirectory) RemoveFile(name string, expected outputcap.File) error {
	if matched, err := directory.plan.trigger(outputV3CSRemoveFile, directory.path, name); matched {
		return err
	}
	return directory.Directory.RemoveFile(name, expected)
}

func (directory *outputV3ControlSessionFaultDirectory) AcquireLock(
	name string,
	existingOnly bool,
) (outputcap.Lock, bool, error) {
	if matched, err := directory.plan.trigger(outputV3CSAcquireLock, directory.path, name); matched {
		if err != nil || !directory.plan.forceCreated {
			return nil, false, err
		}
		lock, _, lockErr := directory.Directory.AcquireLock(name, existingOnly)
		return lock, true, lockErr
	}
	return directory.Directory.AcquireLock(name, existingOnly)
}

func (directory *outputV3ControlSessionFaultDirectory) Sync() error {
	if matched, err := directory.plan.trigger(outputV3CSSync, directory.path, ""); matched {
		return err
	}
	return directory.Directory.Sync()
}

func (directory *outputV3ControlSessionFaultDirectory) ValidateCreateAuthority() error {
	if validator, ok := directory.Directory.(outputcap.CreateAuthorityValidator); ok {
		return validator.ValidateCreateAuthority()
	}
	return nil
}

func (directory *outputV3ControlSessionFaultDirectory) ValidateMetadataAuthority() error {
	if validator, ok := directory.Directory.(outputcap.MetadataAuthorityValidator); ok {
		return validator.ValidateMetadataAuthority()
	}
	return nil
}

func (directory *outputV3ControlSessionFaultDirectory) ValidatePublicEntryNames(names []string) error {
	if validator, ok := directory.Directory.(outputcap.PublicEntryNamesValidator); ok {
		return validator.ValidatePublicEntryNames(names)
	}
	for _, name := range names {
		if err := directory.Directory.ValidatePublicEntryName(name); err != nil {
			return err
		}
	}
	return nil
}

func (directory *memoryCapabilityDirectory) CreateFile(name string, _ bool, size int64) (outputcap.File, error) {
	if err := directory.usable(); err != nil {
		return nil, err
	}
	if size < 0 || size > int64(int(^uint(0)>>1)) {
		return nil, outputcap.ErrUnsafeNamespace
	}
	directory.filesystem.mu.Lock()
	defer directory.filesystem.mu.Unlock()
	if _, exists := directory.node.children[name]; exists {
		return nil, outputcap.ErrNamespaceCollision
	}
	node := directory.filesystem.newNode(outputcap.EntryRegularFile)
	node.data = make([]byte, int(size))
	directory.node.children[name] = node
	return &memoryCapabilityFile{filesystem: directory.filesystem, node: node}, nil
}

func (directory *memoryCapabilityDirectory) OpenFile(name string, _ bool, _ bool) (outputcap.File, error) {
	if err := directory.usable(); err != nil {
		return nil, err
	}
	directory.filesystem.mu.Lock()
	defer directory.filesystem.mu.Unlock()
	node, ok := directory.node.children[name]
	if !ok {
		return nil, fs.ErrNotExist
	}
	if node.kind != outputcap.EntryRegularFile {
		return nil, outputcap.ErrUnsafeNamespace
	}
	return &memoryCapabilityFile{filesystem: directory.filesystem, node: node}, nil
}

func (directory *memoryCapabilityDirectory) LinkFileNoReplace(source outputcap.File, name string) (outputcap.File, error) {
	if err := directory.usable(); err != nil {
		return nil, err
	}
	node, err := fileNode(source)
	if err != nil {
		return nil, err
	}
	directory.filesystem.mu.Lock()
	defer directory.filesystem.mu.Unlock()
	if _, exists := directory.node.children[name]; exists {
		return nil, outputcap.ErrNamespaceCollision
	}
	directory.node.children[name] = node
	return &memoryCapabilityFile{filesystem: directory.filesystem, node: node}, nil
}

func (directory *memoryCapabilityDirectory) ReplacePrivateFile(source outputcap.File, name string) error {
	if err := directory.usable(); err != nil {
		return err
	}
	node, err := fileNode(source)
	if err != nil {
		return err
	}
	directory.filesystem.mu.Lock()
	defer directory.filesystem.mu.Unlock()
	if current, exists := directory.node.children[name]; !exists {
		return fs.ErrNotExist
	} else if current.kind != outputcap.EntryRegularFile {
		return outputcap.ErrUnsafeNamespace
	}
	for existingName, existing := range directory.node.children {
		if existingName != name && existing == node {
			delete(directory.node.children, existingName)
		}
	}
	directory.node.children[name] = node
	return nil
}

func (directory *memoryCapabilityDirectory) RemoveFile(name string, expected outputcap.File) error {
	if err := directory.usable(); err != nil {
		return err
	}
	directory.filesystem.mu.Lock()
	node, ok := directory.node.children[name]
	directory.filesystem.mu.Unlock()
	if !ok {
		return fs.ErrNotExist
	}
	if node.kind != outputcap.EntryRegularFile {
		return outputcap.ErrUnsafeNamespace
	}
	matched, err := expected.SameFile(&memoryCapabilityFile{filesystem: directory.filesystem, node: node})
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

func (directory *memoryCapabilityDirectory) AcquireLock(
	name string,
	existingOnly bool,
) (outputcap.Lock, bool, error) {
	if err := directory.usable(); err != nil {
		return nil, false, err
	}
	directory.filesystem.mu.Lock()
	defer directory.filesystem.mu.Unlock()
	node, exists := directory.node.children[name]
	created := false
	if !exists {
		if existingOnly {
			return nil, false, fs.ErrNotExist
		}
		node = directory.filesystem.newNode(outputcap.EntryRegularFile)
		directory.node.children[name] = node
		created = true
	}
	if node.kind != outputcap.EntryRegularFile {
		return nil, false, outputcap.ErrUnsafeNamespace
	}
	if node.locked {
		return nil, false, outputcap.ErrNamespaceLockBusy
	}
	node.locked = true
	file := &memoryCapabilityFile{filesystem: directory.filesystem, node: node}
	return &memoryCapabilityLock{filesystem: directory.filesystem, node: node, file: file}, created, nil
}

func (directory *memoryCapabilityDirectory) usable() error {
	if directory == nil || directory.filesystem == nil || directory.node == nil || directory.closed {
		return fs.ErrClosed
	}
	if directory.node.kind != outputcap.EntryDirectory {
		return outputcap.ErrUnsafeNamespace
	}
	return nil
}

func (directory *memoryCapabilityDirectory) identity() string {
	return "memory-directory:" + strings.Repeat("0", int(directory.node.id%7)) + string(rune(directory.node.id+32))
}

func directoryNode(directory outputcap.Directory) (*memoryCapabilityNode, error) {
	capability, ok := directory.(*memoryCapabilityDirectory)
	if !ok || capability.node == nil {
		return nil, outputcap.ErrUnsafeNamespace
	}
	return capability.node, nil
}
