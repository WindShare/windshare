//go:build windows

package perfevidence

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"github.com/windshare/windshare/internal/processowner/protocol"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

const (
	cleanupMutationLimit   = 64
	evidenceStoreReadBatch = 128
	windowsFileDeleteChild = 0x00000040
	windowsMutationAccess  = windows.ACCESS_MASK(
		windows.FILE_WRITE_DATA |
			windows.FILE_APPEND_DATA |
			windows.FILE_WRITE_EA |
			windows.FILE_WRITE_ATTRIBUTES |
			windowsFileDeleteChild |
			windows.DELETE |
			windows.WRITE_DAC |
			windows.WRITE_OWNER |
			windows.GENERIC_WRITE |
			windows.GENERIC_ALL |
			windows.MAXIMUM_ALLOWED,
	)
)

var reopenWindowsFileProcedure = windows.NewLazySystemDLL("kernel32.dll").NewProc("ReOpenFile")

type outputRootAuthority struct {
	path     string
	identity directoryIdentity
	handle   windows.Handle
}

type stageDirectoryAuthority struct {
	path        string
	name        string
	identity    directoryIdentity
	handle      windows.Handle
	transition  func(relative, phase string) error
	leaseHandle windows.Handle
	liveLease   bool
}

func evidenceFileClass(relative string) evidenceStoreFileClass {
	switch filepath.ToSlash(relative) {
	case manifestName, stageOwnerName:
		return evidenceMetadataFile
	case payloadName:
		return evidencePayloadFile
	default:
		return evidenceArtifactFile
	}
}

func defaultEvidenceStoreMeter() *evidenceStoreMeter {
	meter, err := newEvidenceStoreMeter(DefaultEvidenceStoreBudget())
	if err != nil {
		panic(err)
	}
	return meter
}

func (meter *evidenceStoreMeter) observeDirectory(relative string, depth int) error {
	return meter.observeObject(relative, depth, 0, evidenceArtifactFile)
}

func evidenceRelativeDepth(relative string) int {
	clean := filepath.ToSlash(filepath.Clean(relative))
	if clean == "." || clean == "" {
		return 1
	}
	return strings.Count(clean, "/") + 1
}

func requireDirectChildName(name string) error {
	if name == "" || name == "." || name == ".." || filepath.Base(name) != name {
		return fmt.Errorf("filesystem authority child %q is not a direct name", name)
	}
	return nil
}

func (walk *evidenceStoreWalk) observeDirectory(relative string) error {
	if walk == nil || walk.meter == nil {
		return errors.New("evidence store walk has no shared meter")
	}
	return walk.meter.observeDirectory(relative, evidenceRelativeDepth(relative))
}

func (walk *evidenceStoreWalk) observeFile(relative string, file *os.File, info os.FileInfo) error {
	if walk == nil || walk.meter == nil {
		return errors.New("evidence store walk has no shared meter")
	}
	if _, alreadyCounted := walk.skipFiles[relative]; !alreadyCounted {
		if err := walk.meter.observeFile(
			relative, evidenceRelativeDepth(relative), info.Size(), evidenceFileClass(relative),
		); err != nil {
			return err
		}
	}
	if walk.visitor != nil {
		return walk.visitor(relative, file, info)
	}
	return nil
}

type protectedWindowsFile struct {
	file     *os.File
	path     string
	identity directoryIdentity
}

type protectedWindowsDirectory struct {
	handle   windows.Handle
	path     string
	identity directoryIdentity
}

type windowsConsumptionAuthority struct {
	mu                         sync.Mutex
	files                      []protectedWindowsFile
	directories                []protectedWindowsDirectory
	publicationSource          string
	publicationWatchHandle     windows.Handle
	publicationWatchEvent      windows.Handle
	publicationWatchOverlapped windows.Overlapped
	publicationWatchBuffer     []byte
	publicationWatchPending    bool
	closed                     bool
}

type windowsMutationOutput struct {
	path        string
	file        *os.File
	directories []protectedWindowsDirectory
	hash        hash.Hash
	written     int64
	sealed      bool
	adopted     bool
	finalized   bool
	retained    byteConsumptionAuthority
}

func prepareMutationOutput(path string) (mutationOutputSink, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	parent := filepath.Dir(absolute)
	directoryHandle, err := openWindowsDirectory(parent, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE)
	if err != nil {
		return nil, fmt.Errorf("retain protected-output directory: %w", err)
	}
	directoryIdentity, err := windowsDirectoryIdentity(directoryHandle)
	if err != nil {
		return nil, errors.Join(err, windows.CloseHandle(directoryHandle))
	}
	encoded, err := windows.UTF16PtrFromString(absolute)
	if err != nil {
		return nil, errors.Join(err, windows.CloseHandle(directoryHandle))
	}
	handle, err := windows.CreateFile(
		encoded,
		windows.GENERIC_READ|windows.GENERIC_WRITE|windows.DELETE|windows.SYNCHRONIZE,
		windows.FILE_SHARE_READ,
		nil,
		windows.CREATE_NEW,
		windows.FILE_FLAG_OPEN_REPARSE_POINT,
		0,
	)
	if err != nil {
		return nil, errors.Join(err, windows.CloseHandle(directoryHandle))
	}
	return &windowsMutationOutput{
		path: absolute, file: os.NewFile(uintptr(handle), absolute), hash: sha256.New(),
		directories: []protectedWindowsDirectory{{
			handle: directoryHandle, path: parent, identity: directoryIdentity,
		}},
	}, nil
}

func (output *windowsMutationOutput) WriteContext(ctx context.Context, content []byte) (int, error) {
	if output == nil || output.file == nil || output.sealed || output.finalized {
		return 0, errors.New("protected output writer is closed")
	}
	if err := ctx.Err(); err != nil {
		return 0, context.Cause(ctx)
	}
	written, err := output.file.Write(content)
	if written > 0 {
		_, _ = output.hash.Write(content[:written])
		output.written += int64(written)
	}
	return written, errors.Join(err, context.Cause(ctx))
}

func (output *windowsMutationOutput) Seal(
	ctx context.Context,
	expectedBytes int64,
	expectedSHA256 string,
) error {
	if output == nil || output.file == nil || output.sealed || output.finalized {
		return errors.New("protected output writer cannot be sealed")
	}
	if err := ctx.Err(); err != nil {
		return context.Cause(ctx)
	}
	observedSHA256 := hex.EncodeToString(output.hash.Sum(nil))
	if output.written != expectedBytes || observedSHA256 != expectedSHA256 {
		return fmt.Errorf(
			"protected output frame identity mismatch: got %d/%s, want %d/%s",
			output.written, observedSHA256, expectedBytes, expectedSHA256,
		)
	}
	if err := output.file.Sync(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return context.Cause(ctx)
	}
	if _, err := output.file.Seek(0, 0); err != nil {
		return err
	}
	info, err := output.file.Stat()
	if err != nil || info.Size() != expectedBytes {
		return errors.Join(errors.New("protected output size changed before retention"), err)
	}
	var opened windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(windows.Handle(output.file.Fd()), &opened); err != nil {
		return err
	}
	expectedIdentity := windowsFileIdentity(opened)
	bridgeHandle, err := reopenWindowsFile(
		windows.Handle(output.file.Fd()),
		windows.GENERIC_READ,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		windows.FILE_FLAG_OPEN_REPARSE_POINT,
	)
	if err != nil {
		return fmt.Errorf("open protected-output read bridge: %w", err)
	}
	bridge := os.NewFile(uintptr(bridgeHandle), output.path)
	if err := verifyWindowsMutationRead(
		bridge, output.path, expectedIdentity, expectedBytes, expectedSHA256,
	); err != nil {
		return errors.Join(fmt.Errorf("verify protected-output read bridge: %w", err), bridge.Close())
	}
	if err := output.file.Close(); err != nil {
		output.file = nil
		return errors.Join(
			fmt.Errorf("retire protected-output writer: %w", err),
			deleteWindowsMutationBridge(bridge),
		)
	}
	output.file = nil
	finalHandle, err := reopenWindowsFile(
		windows.Handle(bridge.Fd()),
		windows.GENERIC_READ,
		windows.FILE_SHARE_READ,
		windows.FILE_FLAG_OPEN_REPARSE_POINT,
	)
	if err != nil {
		return errors.Join(
			fmt.Errorf("open protected-output final read authority: %w", err),
			deleteWindowsMutationBridge(bridge),
		)
	}
	final := os.NewFile(uintptr(finalHandle), output.path)
	if err := verifyWindowsMutationRead(
		final, output.path, expectedIdentity, expectedBytes, expectedSHA256,
	); err != nil {
		return errors.Join(
			fmt.Errorf("verify protected-output final read authority: %w", err),
			final.Close(),
			deleteWindowsMutationBridge(bridge),
		)
	}
	if err := bridge.Close(); err != nil {
		return errors.Join(
			fmt.Errorf("retire protected-output read bridge: %w", err),
			deleteWindowsMutationFinal(final),
		)
	}
	authority := &windowsConsumptionAuthority{
		files: []protectedWindowsFile{{
			file: final, path: output.path, identity: expectedIdentity,
		}},
		directories: output.directories,
	}
	if err := authority.Verify(); err != nil {
		return errors.Join(err, deleteWindowsMutationFinal(final))
	}
	output.directories = nil
	output.retained = authority
	output.sealed = true
	return nil
}

func reopenWindowsFile(
	handle windows.Handle,
	desiredAccess uint32,
	shareMode uint32,
	flagsAndAttributes uint32,
) (windows.Handle, error) {
	result, _, callErr := reopenWindowsFileProcedure.Call(
		uintptr(handle), uintptr(desiredAccess), uintptr(shareMode), uintptr(flagsAndAttributes),
	)
	reopened := windows.Handle(result)
	if reopened != windows.InvalidHandle {
		return reopened, nil
	}
	if errno, ok := callErr.(syscall.Errno); ok && errno != 0 {
		return windows.InvalidHandle, errno
	}
	return windows.InvalidHandle, errors.New("ReOpenFile failed without an error code")
}

func verifyWindowsMutationRead(
	file *os.File,
	path string,
	expectedIdentity directoryIdentity,
	expectedBytes int64,
	expectedSHA256 string,
) error {
	var opened windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(windows.Handle(file.Fd()), &opened); err != nil {
		return err
	}
	if opened.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 ||
		opened.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0 ||
		windowsFileIdentity(opened) != expectedIdentity {
		return errors.New("protected-output handoff changed the retained file object")
	}
	info, err := file.Stat()
	if err != nil {
		return err
	}
	observed, err := artifactIdentityFromOpenFile(file, info, path)
	if err != nil {
		return err
	}
	if observed.Bytes != expectedBytes || observed.SHA256 != expectedSHA256 {
		return errors.New("protected-output handoff changed the retained bytes")
	}
	_, err = file.Seek(0, io.SeekStart)
	return err
}

func deleteWindowsMutationBridge(bridge *os.File) error {
	if bridge == nil {
		return nil
	}
	deleteHandle, err := reopenWindowsFile(
		windows.Handle(bridge.Fd()),
		windows.DELETE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		windows.FILE_FLAG_OPEN_REPARSE_POINT,
	)
	if err != nil {
		return errors.Join(fmt.Errorf("reopen protected output for rollback deletion: %w", err), bridge.Close())
	}
	deleteErr := markWindowsHandleForDeletion(deleteHandle)
	if deleteErr != nil {
		deleteErr = fmt.Errorf("mark protected output for rollback deletion: %w", deleteErr)
	}
	return errors.Join(
		deleteErr,
		windows.CloseHandle(deleteHandle),
		bridge.Close(),
	)
}

func deleteWindowsMutationFinal(final *os.File) error {
	if final == nil {
		return nil
	}
	bridgeHandle, err := reopenWindowsFile(
		windows.Handle(final.Fd()),
		windows.GENERIC_READ,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		windows.FILE_FLAG_OPEN_REPARSE_POINT,
	)
	if err != nil {
		return errors.Join(fmt.Errorf("open protected-output rollback bridge: %w", err), final.Close())
	}
	bridge := os.NewFile(uintptr(bridgeHandle), final.Name())
	return errors.Join(final.Close(), deleteWindowsMutationBridge(bridge))
}

func (output *windowsMutationOutput) adopt() (byteConsumptionAuthority, error) {
	if output == nil || !output.sealed || output.adopted || output.finalized || output.retained == nil {
		return nil, errors.New("protected output is not a sealed, unadopted transaction")
	}
	output.adopted = true
	return output.retained, nil
}

func (output *windowsMutationOutput) finalize() {
	if output != nil && output.sealed && output.adopted && !output.finalized {
		output.finalized = true
	}
}

func (output *windowsMutationOutput) Abort(ctx context.Context) error {
	if output == nil || output.finalized {
		return nil
	}
	var errs []error
	if output.retained != nil {
		if authority, ok := output.retained.(*windowsConsumptionAuthority); ok {
			authority.mu.Lock()
			files := append([]protectedWindowsFile(nil), authority.files...)
			authority.files = nil
			authority.mu.Unlock()
			for _, protected := range files {
				errs = append(errs, deleteWindowsMutationFinal(protected.file))
			}
			errs = append(errs, authority.closeWithoutVerify())
		} else {
			errs = append(errs, output.retained.Close())
		}
		output.retained = nil
	}
	if output.file != nil {
		handle := windows.Handle(output.file.Fd())
		errs = append(errs, markWindowsHandleForDeletion(handle), output.file.Close())
		output.file = nil
	}
	for _, directory := range output.directories {
		errs = append(errs, windows.CloseHandle(directory.handle))
	}
	output.directories = nil
	output.sealed = false
	output.adopted = false
	return errors.Join(errors.Join(errs...), context.Cause(ctx))
}

func acquireConsumptionAuthority(
	targets []snapshotValidationTarget,
	roots []string,
) (byteConsumptionAuthority, error) {
	if len(targets) == 0 || len(roots) == 0 {
		return nil, errors.New("consumption authority requires byte targets and directory roots")
	}
	authority := &windowsConsumptionAuthority{}
	fail := func(operationErr error) (byteConsumptionAuthority, error) {
		return nil, errors.Join(operationErr, authority.closeWithoutVerify())
	}
	directories, err := windowsAuthorityDirectories(targets, roots)
	if err != nil {
		return fail(err)
	}
	for _, directory := range directories {
		handle, err := openWindowsDirectory(directory, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE)
		if err != nil {
			return fail(fmt.Errorf("retain consumption directory %s: %w", directory, err))
		}
		identity, err := windowsDirectoryIdentity(handle)
		if err != nil {
			return fail(errors.Join(err, windows.CloseHandle(handle)))
		}
		authority.directories = append(authority.directories, protectedWindowsDirectory{
			handle: handle, path: directory, identity: identity,
		})
	}
	for _, target := range targets {
		file, info, identity, err := openWindowsConsumptionFile(target.PhysicalPath)
		if err != nil {
			return fail(fmt.Errorf("retain consumption byte %s: %w", target.LogicalPath, err))
		}
		expected := ArtifactFile{Path: target.LogicalPath, Bytes: target.Bytes, SHA256: target.SHA256}
		observed, hashErr := artifactIdentityFromOpenFile(file, info, target.LogicalPath)
		if hashErr != nil || observed != expected {
			return fail(errors.Join(
				fmt.Errorf("consumption byte %s changed while authority was acquired", target.LogicalPath),
				hashErr, file.Close(),
			))
		}
		authority.files = append(authority.files, protectedWindowsFile{
			file: file, path: target.PhysicalPath, identity: identity,
		})
	}
	if err := authority.Verify(); err != nil {
		return fail(err)
	}
	return authority, nil
}

func windowsAuthorityDirectories(
	targets []snapshotValidationTarget,
	roots []string,
) ([]string, error) {
	directories := make(map[string]string)
	rootAliasCache := make(map[string]bool)
	cleanRoots := make([]string, 0, len(roots))
	for _, root := range roots {
		info, err := os.Lstat(root)
		if err != nil || !info.IsDir() || isReparsePointInfo(info) {
			return nil, errors.Join(fmt.Errorf("consumption root %s is not a real directory", root), err)
		}
		clean := filepath.Clean(root)
		cleanRoots = append(cleanRoots, clean)
		directories[platformPathKey(clean)] = clean
	}
	for _, target := range targets {
		current := filepath.Dir(target.PhysicalPath)
		matched := false
		for _, root := range cleanRoots {
			relative, inside := relativeWithin(root, current)
			if !inside {
				continue
			}
			for relative != "." {
				directories[platformPathKey(filepath.Clean(current))] = filepath.Clean(current)
				parent := filepath.Dir(current)
				parentRelative := filepath.Dir(relative)
				if platformPathKey(parent) == platformPathKey(current) || parentRelative == relative {
					return nil, fmt.Errorf("consumption target %s escaped authority root %s", target.LogicalPath, root)
				}
				current = parent
				relative = parentRelative
			}
			candidateKey := platformPathKey(filepath.Clean(current))
			rootKey := platformPathKey(root)
			if candidateKey != rootKey {
				aliasKey := candidateKey + "\x00" + rootKey
				aliases, known := rootAliasCache[aliasKey]
				if !known {
					aliases = samePath(current, root)
					rootAliasCache[aliasKey] = aliases
				}
				if !aliases {
					return nil, fmt.Errorf("consumption target %s escaped authority root %s", target.LogicalPath, root)
				}
			}
			matched = true
			break
		}
		if !matched {
			return nil, fmt.Errorf("consumption target %s is outside every authority root", target.LogicalPath)
		}
	}
	result := make([]string, 0, len(directories))
	for _, directory := range directories {
		result = append(result, directory)
	}
	sort.Strings(result)
	return result, nil
}

func openWindowsConsumptionFile(path string) (*os.File, os.FileInfo, directoryIdentity, error) {
	return openWindowsConsumptionFileShared(path, windows.FILE_SHARE_READ)
}

func openWindowsConsumptionFileShared(
	path string,
	share uint32,
) (*os.File, os.FileInfo, directoryIdentity, error) {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, nil, directoryIdentity{}, err
	}
	handle, err := windows.CreateFile(
		name, windows.GENERIC_READ, share, nil, windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0,
	)
	if err != nil {
		return nil, nil, directoryIdentity{}, err
	}
	var opened windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &opened); err != nil {
		return nil, nil, directoryIdentity{}, errors.Join(err, windows.CloseHandle(handle))
	}
	if opened.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 ||
		opened.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0 {
		return nil, nil, directoryIdentity{}, errors.Join(
			errors.New("consumption byte is not a regular non-reparse file"), windows.CloseHandle(handle),
		)
	}
	file := os.NewFile(uintptr(handle), path)
	info, err := file.Stat()
	if err != nil {
		return nil, nil, directoryIdentity{}, errors.Join(err, file.Close())
	}
	return file, info, windowsFileIdentity(opened), nil
}

func (authority *windowsConsumptionAuthority) Verify() error {
	if authority == nil {
		return errors.New("consumption authority is nil")
	}
	authority.mu.Lock()
	defer authority.mu.Unlock()
	if authority.closed {
		return errors.New("consumption authority is closed")
	}
	var errs []error
	errs = append(errs, authority.verifyPublicationWatchLocked())
	for _, directory := range authority.directories {
		identity, err := directoryIdentityAt(directory.path)
		if err != nil || identity != directory.identity {
			errs = append(errs, fmt.Errorf(
				"consumption directory %s no longer names its retained authority: %w", directory.path, err,
			))
		}
	}
	for _, protected := range authority.files {
		identity, err := windowsFileIdentityAt(protected.path)
		if err != nil || identity != protected.identity {
			errs = append(errs, fmt.Errorf(
				"consumption path %s no longer names its retained authority: %w", protected.path, err,
			))
		}
	}
	return errors.Join(errs...)
}

func (authority *windowsConsumptionAuthority) preparePublicationRename(source string) error {
	if err := authority.Verify(); err != nil {
		return err
	}
	authority.mu.Lock()
	defer authority.mu.Unlock()
	if authority.closed {
		return errors.New("consumption authority is closed")
	}
	for _, protected := range authority.files {
		if _, inside := relativeWithin(source, protected.path); !inside {
			return fmt.Errorf("sealed publication byte %s is outside its staging root", protected.path)
		}
	}
	if err := authority.startPublicationWatchLocked(source); err != nil {
		return err
	}
	// Windows refuses to rename a directory with open descendants even when
	// those handles share delete access. The subtree change ledger is started
	// while the exact file authorities still deny mutation; only then are the
	// descendant handles released for the no-replace rename. Any byte or
	// namespace event in that boundary makes the monotonic seal fail closed.
	var errs []error
	for _, protected := range authority.files {
		errs = append(errs, protected.file.Close())
	}
	authority.files = nil
	for _, directory := range authority.directories {
		errs = append(errs, windows.CloseHandle(directory.handle))
	}
	authority.directories = nil
	authority.publicationSource = filepath.Clean(source)
	return errors.Join(errs...)
}

func (authority *windowsConsumptionAuthority) completePublicationRename(destination string) error {
	authority.mu.Lock()
	if authority.closed {
		authority.mu.Unlock()
		return errors.New("consumption authority is closed")
	}
	if authority.publicationSource == "" {
		authority.mu.Unlock()
		return errors.New("publication rename was not prepared")
	}
	_ = destination
	authority.publicationSource = ""
	authority.mu.Unlock()
	return authority.Verify()
}

func (authority *windowsConsumptionAuthority) startPublicationWatchLocked(source string) error {
	var expected *protectedWindowsDirectory
	for index := range authority.directories {
		if samePath(authority.directories[index].path, source) {
			expected = &authority.directories[index]
			break
		}
	}
	if expected == nil {
		return errors.New("publication root is not covered by its retained directory authorities")
	}
	encoded, err := windows.UTF16PtrFromString(source)
	if err != nil {
		return err
	}
	handle, err := windows.CreateFile(
		encoded,
		windows.FILE_LIST_DIRECTORY|windows.FILE_READ_ATTRIBUTES|windows.SYNCHRONIZE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_FLAG_OVERLAPPED,
		0,
	)
	if err != nil {
		return err
	}
	identity, err := windowsDirectoryIdentity(handle)
	if err != nil || identity != expected.identity {
		return errors.Join(
			errors.New("publication mutation ledger retained a substituted stage"),
			err, windows.CloseHandle(handle),
		)
	}
	event, err := windows.CreateEvent(nil, 1, 0, nil)
	if err != nil {
		return errors.Join(err, windows.CloseHandle(handle))
	}
	const publicationWatchBufferBytes = 64 << 10
	authority.publicationWatchHandle = handle
	authority.publicationWatchEvent = event
	authority.publicationWatchOverlapped = windows.Overlapped{HEvent: event}
	authority.publicationWatchBuffer = make([]byte, publicationWatchBufferBytes)
	const mutationMask = windows.FILE_NOTIFY_CHANGE_FILE_NAME | windows.FILE_NOTIFY_CHANGE_DIR_NAME |
		windows.FILE_NOTIFY_CHANGE_ATTRIBUTES | windows.FILE_NOTIFY_CHANGE_SIZE |
		windows.FILE_NOTIFY_CHANGE_LAST_WRITE | windows.FILE_NOTIFY_CHANGE_CREATION |
		windows.FILE_NOTIFY_CHANGE_SECURITY
	err = windows.ReadDirectoryChanges(
		handle,
		&authority.publicationWatchBuffer[0],
		uint32(len(authority.publicationWatchBuffer)),
		true,
		mutationMask,
		nil,
		&authority.publicationWatchOverlapped,
		0,
	)
	if err != nil && !errors.Is(err, windows.ERROR_IO_PENDING) {
		return errors.Join(err, authority.closePublicationWatchLocked())
	}
	authority.publicationWatchPending = true
	return nil
}

func (authority *windowsConsumptionAuthority) verifyPublicationWatchLocked() error {
	if !authority.publicationWatchPending {
		return nil
	}
	var transferred uint32
	err := windows.GetOverlappedResult(
		authority.publicationWatchHandle,
		&authority.publicationWatchOverlapped,
		&transferred,
		false,
	)
	if errors.Is(err, windows.ERROR_IO_INCOMPLETE) {
		return nil
	}
	authority.publicationWatchPending = false
	if err != nil {
		return fmt.Errorf("read publication mutation ledger: %w", err)
	}
	return fmt.Errorf("staged publication mutated after sealing (%d notification bytes)", transferred)
}

func (authority *windowsConsumptionAuthority) closePublicationWatchLocked() error {
	if authority.publicationWatchHandle == 0 || authority.publicationWatchHandle == windows.InvalidHandle {
		return nil
	}
	cancelErr := windows.CancelIoEx(authority.publicationWatchHandle, &authority.publicationWatchOverlapped)
	if errors.Is(cancelErr, windows.ERROR_NOT_FOUND) || errors.Is(cancelErr, windows.ERROR_OPERATION_ABORTED) {
		cancelErr = nil
	}
	handleErr := windows.CloseHandle(authority.publicationWatchHandle)
	eventErr := windows.CloseHandle(authority.publicationWatchEvent)
	authority.publicationWatchHandle = windows.InvalidHandle
	authority.publicationWatchEvent = windows.InvalidHandle
	authority.publicationWatchBuffer = nil
	authority.publicationWatchPending = false
	return errors.Join(cancelErr, handleErr, eventErr)
}

func windowsFileIdentityAt(path string) (directoryIdentity, error) {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return directoryIdentity{}, err
	}
	handle, err := windows.CreateFile(
		name, windows.FILE_READ_ATTRIBUTES,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0,
	)
	if err != nil {
		return directoryIdentity{}, err
	}
	var info windows.ByHandleFileInformation
	infoErr := windows.GetFileInformationByHandle(handle, &info)
	closeErr := windows.CloseHandle(handle)
	if err := errors.Join(infoErr, closeErr); err != nil {
		return directoryIdentity{}, err
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 ||
		info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0 {
		return directoryIdentity{}, errors.New("consumption path is not a regular non-reparse file")
	}
	return windowsFileIdentity(info), nil
}

func (authority *windowsConsumptionAuthority) VerifyProcessStart(
	evidence protocol.StartEvidence,
	executable string,
) (bool, error) {
	if err := authority.Verify(); err != nil {
		return false, err
	}
	authority.mu.Lock()
	defer authority.mu.Unlock()
	var expected *protectedWindowsFile
	for index := range authority.files {
		if samePath(authority.files[index].path, executable) {
			expected = &authority.files[index]
			break
		}
	}
	if expected == nil {
		return false, nil
	}
	if evidence.Platform != protocol.PlatformWindowsJob {
		return true, errors.New("contained process start evidence has the wrong platform")
	}
	expectedIdentity, err := windowsProtocolObjectIdentity(windows.Handle(expected.file.Fd()))
	if err != nil {
		return true, fmt.Errorf("identify retained executable authority: %w", err)
	}
	if evidence.Executable != expectedIdentity {
		return true, errors.New("contained process start evidence differs from its retained executable authority")
	}
	instance, err := strconv.ParseUint(evidence.ProcessInstance, 10, 64)
	if err != nil {
		return true, fmt.Errorf("parse contained process instance: %w", err)
	}
	handle, err := windows.OpenProcess(
		windows.PROCESS_QUERY_LIMITED_INFORMATION,
		false,
		uint32(evidence.ProcessID),
	)
	if err != nil {
		return true, fmt.Errorf("open contained process: %w", err)
	}
	var creation, exit, kernel, user windows.Filetime
	timeErr := windows.GetProcessTimes(handle, &creation, &exit, &kernel, &user)
	buffer := make([]uint16, 32_768)
	size := uint32(len(buffer))
	queryErr := windows.QueryFullProcessImageName(handle, 0, &buffer[0], &size)
	closeErr := windows.CloseHandle(handle)
	if err := errors.Join(timeErr, queryErr, closeErr); err != nil {
		return true, fmt.Errorf("identify contained process instance and image: %w", err)
	}
	observedInstance := uint64(creation.HighDateTime)<<32 | uint64(creation.LowDateTime)
	if observedInstance != instance {
		return true, errors.New("contained process instance differs from authenticated start evidence")
	}
	imagePath := windows.UTF16ToString(buffer[:size])
	imageIdentity, err := windowsProtocolObjectIdentityAt(imagePath)
	if err != nil {
		return true, fmt.Errorf("identify contained process image bytes: %w", err)
	}
	if imageIdentity != expectedIdentity || imageIdentity != evidence.Executable {
		return true, errors.New("contained process image differs from its retained executable authority")
	}
	return true, nil
}

type windowsProtocolFileIDInfo struct {
	volume uint64
	object [16]byte
}

func windowsProtocolObjectIdentity(handle windows.Handle) (protocol.ObjectIdentity, error) {
	var identity windowsProtocolFileIDInfo
	if err := windows.GetFileInformationByHandleEx(
		handle,
		windows.FileIdInfo,
		(*byte)(unsafe.Pointer(&identity)),
		uint32(unsafe.Sizeof(identity)),
	); err != nil {
		return protocol.ObjectIdentity{}, err
	}
	return protocol.NewObjectIdentity128(identity.volume, identity.object), nil
}

func windowsProtocolObjectIdentityAt(path string) (protocol.ObjectIdentity, error) {
	encoded, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return protocol.ObjectIdentity{}, err
	}
	handle, err := windows.CreateFile(
		encoded,
		windows.FILE_READ_ATTRIBUTES,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT,
		0,
	)
	if err != nil {
		return protocol.ObjectIdentity{}, err
	}
	identity, identityErr := windowsProtocolObjectIdentity(handle)
	closeErr := windows.CloseHandle(handle)
	return identity, errors.Join(identityErr, closeErr)
}

func (authority *windowsConsumptionAuthority) Close() error {
	if authority == nil {
		return nil
	}
	verifyErr := authority.Verify()
	authority.mu.Lock()
	defer authority.mu.Unlock()
	return errors.Join(verifyErr, authority.closeWithoutVerifyLocked())
}

func (authority *windowsConsumptionAuthority) closeWithoutVerify() error {
	authority.mu.Lock()
	defer authority.mu.Unlock()
	return authority.closeWithoutVerifyLocked()
}

func (authority *windowsConsumptionAuthority) closeWithoutVerifyLocked() error {
	if authority.closed {
		return nil
	}
	authority.closed = true
	var errs []error
	errs = append(errs, authority.closePublicationWatchLocked())
	for _, protected := range authority.files {
		errs = append(errs, protected.file.Close())
	}
	authority.files = nil
	for _, directory := range authority.directories {
		errs = append(errs, windows.CloseHandle(directory.handle))
	}
	authority.directories = nil
	return errors.Join(errs...)
}

type windowsFileBasicInfo struct {
	creationTime   int64
	lastAccessTime int64
	lastWriteTime  int64
	changeTime     int64
	attributes     uint32
}

type windowsFileRenameInformation struct {
	flags          uint32
	rootDirectory  windows.Handle
	fileNameLength uint32
	fileName       [1]uint16
}

func openOutputRootAuthority(path string) (*outputRootAuthority, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve evidence output root: %w", err)
	}
	parent := filepath.Dir(absolute)
	if !samePath(parent, absolute) {
		if err := os.MkdirAll(parent, 0o700); err != nil {
			return nil, fmt.Errorf("create evidence output parent: %w", err)
		}
	}
	handle, created, err := openOrCreateWindowsOutputRoot(absolute)
	if err != nil {
		return nil, fmt.Errorf("open evidence output authority: %w", err)
	}
	resolved, err := resolveDirectoryAuthority(absolute)
	if err != nil {
		return nil, errors.Join(
			fmt.Errorf("resolve evidence output authority: %w", err),
			windows.CloseHandle(handle),
		)
	}
	identity, err := windowsDirectoryIdentity(handle)
	if err != nil {
		return nil, errors.Join(
			fmt.Errorf("identify evidence output authority: %w", err),
			windows.CloseHandle(handle),
		)
	}
	if err := requireSecureWindowsAuthority(handle); err != nil {
		return nil, errors.Join(
			fmt.Errorf("validate evidence output security: %w", err),
			windows.CloseHandle(handle),
		)
	}
	resolvedIdentity, err := directoryIdentityAt(resolved)
	if err != nil || resolvedIdentity != identity {
		return nil, errors.Join(
			errors.New("resolved evidence output path does not identify its retained authority"),
			err,
			windows.CloseHandle(handle),
		)
	}
	if created {
		entries, readErr := readWindowsDirectory(handle, resolved)
		if readErr != nil || len(entries) != 0 {
			return nil, errors.Join(
				errors.New("new evidence output root was not empty after secure creation"),
				readErr,
				windows.CloseHandle(handle),
			)
		}
	}
	return &outputRootAuthority{path: resolved, identity: identity, handle: handle}, nil
}

func openOrCreateWindowsOutputRoot(path string) (windows.Handle, bool, error) {
	descriptor, err := privateWindowsDirectoryDescriptor()
	if err != nil {
		return windows.InvalidHandle, false, err
	}
	ntPath := `\??\` + path
	if uncPath, found := strings.CutPrefix(path, `\\`); found {
		ntPath = `\??\UNC\` + uncPath
	}
	objectName, err := windows.NewNTUnicodeString(ntPath)
	if err != nil {
		return windows.InvalidHandle, false, err
	}
	attributes := windows.OBJECT_ATTRIBUTES{
		Length:             uint32(unsafe.Sizeof(windows.OBJECT_ATTRIBUTES{})),
		ObjectName:         objectName,
		Attributes:         windows.OBJ_CASE_INSENSITIVE,
		SecurityDescriptor: descriptor,
	}
	var status windows.IO_STATUS_BLOCK
	allocationSize := int64(0)
	handle := windows.InvalidHandle
	createErr := windows.NtCreateFile(
		&handle,
		windows.FILE_LIST_DIRECTORY|windows.FILE_TRAVERSE|windows.FILE_READ_ATTRIBUTES|
			windows.READ_CONTROL|windows.SYNCHRONIZE|windows.FILE_GENERIC_WRITE,
		&attributes,
		&status,
		&allocationSize,
		windows.FILE_ATTRIBUTE_NORMAL,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE,
		windows.FILE_CREATE,
		windows.FILE_DIRECTORY_FILE|windows.FILE_OPEN_REPARSE_POINT|windows.FILE_SYNCHRONOUS_IO_NONALERT,
		0,
		0,
	)
	if createErr == nil {
		return handle, true, nil
	}
	handle, openErr := openWindowsDirectoryAccess(
		path,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE,
		windows.FILE_LIST_DIRECTORY|windows.FILE_TRAVERSE|windows.FILE_READ_ATTRIBUTES|
			windows.READ_CONTROL|windows.SYNCHRONIZE|windows.FILE_GENERIC_WRITE,
	)
	if openErr != nil {
		return windows.InvalidHandle, false, errors.Join(createErr, openErr)
	}
	return handle, false, nil
}

func privateWindowsDirectoryDescriptor() (*windows.SECURITY_DESCRIPTOR, error) {
	token := windows.GetCurrentProcessToken()
	user, err := token.GetTokenUser()
	if err != nil {
		return nil, err
	}
	return windows.SecurityDescriptorFromString(fmt.Sprintf(
		"D:P(A;OICI;FA;;;%s)(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)",
		user.User.Sid.String(),
	))
}

func openTreeAuthority(path string) (*stageDirectoryAuthority, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	info, err := os.Lstat(absolute)
	if err != nil {
		return nil, err
	}
	if isReparsePointInfo(info) || !info.IsDir() {
		return nil, fmt.Errorf("artifact tree %s is not a real directory", absolute)
	}
	handle, err := openWindowsDirectory(absolute, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE)
	if err != nil {
		return nil, err
	}
	identity, err := windowsDirectoryIdentity(handle)
	if err != nil {
		return nil, errors.Join(err, windows.CloseHandle(handle))
	}
	return &stageDirectoryAuthority{
		path: absolute, name: filepath.Base(absolute), identity: identity, handle: handle,
		leaseHandle: windows.InvalidHandle,
	}, nil
}

func requireSecureWindowsAuthority(handle windows.Handle) error {
	descriptor, err := windows.GetSecurityInfo(
		handle,
		windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION,
	)
	if err != nil {
		return err
	}
	if descriptor == nil {
		return errors.New("evidence directory has no security descriptor")
	}
	owner, _, err := descriptor.Owner()
	if err != nil {
		return err
	}
	token := windows.GetCurrentProcessToken()
	user, err := token.GetTokenUser()
	if err != nil {
		return err
	}
	if owner == nil || !owner.Equals(user.User.Sid) {
		return errors.New("evidence output root must be owned by the current process user")
	}
	dacl, _, err := descriptor.DACL()
	if err != nil {
		return err
	}
	if dacl == nil {
		return errors.New("evidence directory has a null DACL")
	}
	trusted := map[string]struct{}{
		user.User.Sid.String(): {},
		"S-1-5-18":             {}, // LocalSystem
		"S-1-5-32-544":         {}, // BUILTIN\Administrators
		"S-1-3-0":              {}, // Creator Owner
		"S-1-3-4":              {}, // Owner Rights
	}
	for index := uint16(0); index < dacl.AceCount; index++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, uint32(index), &ace); err != nil {
			return err
		}
		switch ace.Header.AceType {
		case windows.ACCESS_DENIED_ACE_TYPE:
			continue
		case windows.ACCESS_ALLOWED_ACE_TYPE:
		default:
			return fmt.Errorf("evidence directory DACL contains unsupported ACE type %d", ace.Header.AceType)
		}
		if ace.Mask&windowsMutationAccess == 0 {
			continue
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		if !sid.IsValid() {
			return errors.New("evidence directory DACL contains an invalid SID")
		}
		if _, ok := trusted[sid.String()]; !ok {
			return fmt.Errorf("evidence directory grants mutation access to untrusted principal %s", sid.String())
		}
	}
	return nil
}

func directoryIdentityAt(path string) (identity directoryIdentity, resultErr error) {
	handle, err := openWindowsDirectory(
		path, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
	)
	if err != nil {
		return directoryIdentity{}, err
	}
	defer func() { resultErr = errors.Join(resultErr, windows.CloseHandle(handle)) }()
	return windowsDirectoryIdentity(handle)
}

func openWindowsDirectory(path string, share uint32) (windows.Handle, error) {
	return openWindowsDirectoryAccess(
		path, share,
		windows.FILE_LIST_DIRECTORY|windows.FILE_TRAVERSE|windows.FILE_READ_ATTRIBUTES|
			windows.READ_CONTROL|windows.SYNCHRONIZE,
	)
}

func openWindowsDirectoryAccess(path string, share uint32, access uint32) (windows.Handle, error) {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return windows.InvalidHandle, err
	}
	return windows.CreateFile(
		name,
		access,
		share,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT,
		0,
	)
}

func windowsDirectoryIdentity(handle windows.Handle) (directoryIdentity, error) {
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &info); err != nil {
		return directoryIdentity{}, err
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return directoryIdentity{}, errors.New("filesystem authority is a reparse point")
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 {
		return directoryIdentity{}, errors.New("filesystem authority is not a directory")
	}
	return directoryIdentity{
		volume: uint64(info.VolumeSerialNumber),
		object: uint64(info.FileIndexHigh)<<32 | uint64(info.FileIndexLow),
	}, nil
}

func (authority *outputRootAuthority) verifyPath() error {
	if authority == nil || authority.handle == windows.InvalidHandle {
		return errors.New("evidence output authority is closed")
	}
	identity, err := directoryIdentityAt(authority.path)
	if err != nil {
		return fmt.Errorf("reidentify evidence output path: %w", err)
	}
	if identity != authority.identity {
		return errors.New("evidence output path no longer names the retained directory authority")
	}
	return nil
}

func (authority *outputRootAuthority) close() error {
	if authority == nil || authority.handle == windows.InvalidHandle {
		return nil
	}
	handle := authority.handle
	authority.handle = windows.InvalidHandle
	return windows.CloseHandle(handle)
}

func (authority *outputRootAuthority) createChildAuthority(name string) (*stageDirectoryAuthority, error) {
	if err := requireDirectChildName(name); err != nil {
		return nil, err
	}
	if err := authority.verifyPath(); err != nil {
		return nil, err
	}
	handle, err := openWindowsRelativeAccess(
		authority.handle, name, windows.FILE_CREATE,
		windows.FILE_DIRECTORY_FILE|windows.FILE_OPEN_REPARSE_POINT|windows.FILE_SYNCHRONOUS_IO_NONALERT,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE,
		windows.FILE_LIST_DIRECTORY|windows.FILE_TRAVERSE|windows.FILE_READ_ATTRIBUTES|
			windows.READ_CONTROL|windows.SYNCHRONIZE,
	)
	if err != nil {
		return nil, err
	}
	identity, err := windowsDirectoryIdentity(handle)
	if err != nil {
		return nil, errors.Join(err, windows.CloseHandle(handle))
	}
	if err := requireSecureWindowsAuthority(handle); err != nil {
		return nil, errors.Join(err, windows.CloseHandle(handle))
	}
	return &stageDirectoryAuthority{
		path: filepath.Join(authority.path, name), name: name, identity: identity, handle: handle,
		leaseHandle: windows.InvalidHandle,
	}, nil
}

func (authority *outputRootAuthority) openChildAuthority(name string) (*stageDirectoryAuthority, error) {
	if authority == nil || authority.handle == windows.InvalidHandle {
		return nil, errors.New("evidence output authority is closed")
	}
	if err := requireDirectChildName(name); err != nil {
		return nil, err
	}
	handle, err := openWindowsRelativeAccess(
		authority.handle, name, windows.FILE_OPEN,
		windows.FILE_DIRECTORY_FILE|windows.FILE_OPEN_REPARSE_POINT|windows.FILE_SYNCHRONOUS_IO_NONALERT,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE,
		windows.FILE_LIST_DIRECTORY|windows.FILE_TRAVERSE|windows.FILE_READ_ATTRIBUTES|
			windows.READ_CONTROL|windows.SYNCHRONIZE,
	)
	if err != nil {
		return nil, err
	}
	identity, err := windowsDirectoryIdentity(handle)
	if err != nil {
		return nil, errors.Join(err, windows.CloseHandle(handle))
	}
	if err := requireSecureWindowsAuthority(handle); err != nil {
		return nil, errors.Join(err, windows.CloseHandle(handle))
	}
	return &stageDirectoryAuthority{
		path: filepath.Join(authority.path, name), name: name, identity: identity, handle: handle,
		leaseHandle: windows.InvalidHandle,
	}, nil
}

func (authority *outputRootAuthority) openRecoveryChildAuthority(name string) (*stageDirectoryAuthority, error) {
	if authority == nil || authority.handle == windows.InvalidHandle {
		return nil, errors.New("evidence output authority is closed")
	}
	if err := requireDirectChildName(name); err != nil {
		return nil, err
	}
	handle, err := openWindowsRelativeAccess(
		authority.handle, name, windows.FILE_OPEN,
		windows.FILE_DIRECTORY_FILE|windows.FILE_OPEN_REPARSE_POINT|windows.FILE_SYNCHRONOUS_IO_NONALERT,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		windows.FILE_LIST_DIRECTORY|windows.FILE_TRAVERSE|windows.FILE_READ_ATTRIBUTES|
			windows.READ_CONTROL|windows.SYNCHRONIZE,
	)
	if err != nil {
		return nil, err
	}
	identity, err := windowsDirectoryIdentity(handle)
	if err != nil {
		return nil, errors.Join(err, windows.CloseHandle(handle))
	}
	if err := requireSecureWindowsAuthority(handle); err != nil {
		return nil, errors.Join(err, windows.CloseHandle(handle))
	}
	return &stageDirectoryAuthority{
		path: filepath.Join(authority.path, name), name: name, identity: identity, handle: handle,
		leaseHandle: windows.InvalidHandle,
	}, nil
}

func (stage *stageDirectoryAuthority) acquireLiveLease(*outputRootAuthority) error {
	if stage == nil || stage.handle == windows.InvalidHandle {
		return errors.New("stage directory authority is closed")
	}
	// The creator handle omits FILE_SHARE_DELETE, so the kernel rejects every
	// competing rename/delete authority until this handle is closed.
	stage.liveLease = true
	return nil
}

func (stage *stageDirectoryAuthority) tryAcquireRecoveryLease(
	authority *outputRootAuthority,
) (bool, error) {
	if stage == nil || stage.handle == windows.InvalidHandle {
		return false, errors.New("stage directory authority is closed")
	}
	handle, err := openWindowsRelativeAccess(
		authority.handle, stage.name, windows.FILE_OPEN,
		windows.FILE_DIRECTORY_FILE|windows.FILE_OPEN_REPARSE_POINT|windows.FILE_SYNCHRONOUS_IO_NONALERT,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		windows.FILE_READ_ATTRIBUTES|windows.DELETE|windows.SYNCHRONIZE,
	)
	if windowsSharingViolation(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("acquire recovery delete lease: %w", err)
	}
	identity, identityErr := windowsDirectoryIdentity(handle)
	if identityErr != nil || identity != stage.identity {
		return false, errors.Join(
			errors.New("recovery delete lease names a substituted stage"), identityErr, windows.CloseHandle(handle),
		)
	}
	stage.leaseHandle = handle
	return true, nil
}

func windowsSharingViolation(err error) bool {
	if status, ok := errors.AsType[windows.NTStatus](err); ok {
		err = status.Errno()
	}
	return errors.Is(err, windows.ERROR_SHARING_VIOLATION)
}

func (stage *stageDirectoryAuthority) modTime() (time.Time, error) {
	if stage == nil || stage.handle == windows.InvalidHandle {
		return time.Time{}, errors.New("stage directory authority is closed")
	}
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(stage.handle, &info); err != nil {
		return time.Time{}, err
	}
	return time.Unix(0, info.LastWriteTime.Nanoseconds()), nil
}

func (authority *outputRootAuthority) removeRetainedChild(
	stage *stageDirectoryAuthority,
	transition func(string) error,
) error {
	if stage == nil || stage.handle == windows.InvalidHandle || stage.leaseHandle == windows.InvalidHandle {
		return errors.New("recovery removal requires a retained delete-leased child authority")
	}
	if err := stage.verifyName(authority); err != nil {
		return err
	}
	if err := emptyWindowsDirectory(stage.handle, stage.name, transition); err != nil {
		return err
	}
	if err := stage.verifyName(authority); err != nil {
		return err
	}
	return markWindowsHandleForDeletion(stage.leaseHandle)
}

func (stage *stageDirectoryAuthority) verifyName(authority *outputRootAuthority) error {
	if stage == nil || stage.handle == windows.InvalidHandle {
		return errors.New("stage directory authority is closed")
	}
	identity, err := directoryIdentityAt(filepath.Join(authority.path, stage.name))
	if err != nil {
		return err
	}
	if identity != stage.identity {
		return errors.New("stage name no longer identifies its retained directory")
	}
	return nil
}

func (stage *stageDirectoryAuthority) close() error {
	if stage == nil || stage.handle == windows.InvalidHandle {
		return nil
	}
	handle := stage.handle
	stage.handle = windows.InvalidHandle
	leaseHandle := stage.leaseHandle
	stage.leaseHandle = windows.InvalidHandle
	stage.liveLease = false
	var leaseCloseErr error
	if leaseHandle != windows.InvalidHandle {
		leaseCloseErr = windows.CloseHandle(leaseHandle)
	}
	return errors.Join(windows.CloseHandle(handle), leaseCloseErr)
}

func (stage *stageDirectoryAuthority) matchesAuthority(other *stageDirectoryAuthority) error {
	if stage == nil || other == nil || other.handle == windows.InvalidHandle {
		return errors.New("cannot compare closed directory authorities")
	}
	if stage.identity != other.identity {
		return errors.New("published authority does not identify the retained stage directory")
	}
	return nil
}

func (stage *stageDirectoryAuthority) openRegularFile(name string) (*os.File, os.FileInfo, error) {
	if filepath.Base(name) != name {
		return nil, nil, fmt.Errorf("artifact filename %s is not root-relative", name)
	}
	handle, err := openWindowsRelativeAccess(
		stage.handle, name, windows.FILE_OPEN,
		windows.FILE_NON_DIRECTORY_FILE|windows.FILE_OPEN_REPARSE_POINT|windows.FILE_SYNCHRONOUS_IO_NONALERT,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		windows.FILE_GENERIC_READ|windows.FILE_READ_ATTRIBUTES|windows.SYNCHRONIZE,
	)
	if err != nil {
		return nil, nil, err
	}
	var opened windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &opened); err != nil {
		return nil, nil, errors.Join(err, windows.CloseHandle(handle))
	}
	if opened.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 ||
		opened.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0 {
		return nil, nil, errors.Join(
			fmt.Errorf("artifact %s is not a regular non-reparse file", name),
			windows.CloseHandle(handle),
		)
	}
	file := os.NewFile(uintptr(handle), name)
	info, err := file.Stat()
	if err != nil {
		return nil, nil, errors.Join(err, file.Close())
	}
	return file, info, nil
}

func (stage *stageDirectoryAuthority) walkRegularFiles(visitor regularFileVisitor) error {
	return stage.walkEvidenceStore(&evidenceStoreWalk{meter: defaultEvidenceStoreMeter(), visitor: visitor})
}

func (stage *stageDirectoryAuthority) walkEvidenceStore(walk *evidenceStoreWalk) error {
	if stage == nil || stage.handle == windows.InvalidHandle {
		return errors.New("stage directory authority is closed")
	}
	return walkWindowsRegularFiles(stage.handle, "", walk, stage.transition)
}

func walkWindowsRegularFiles(
	parent windows.Handle,
	relative string,
	walk *evidenceStoreWalk,
	transition func(string, string) error,
) error {
	entries, err := readWindowsDirectory(parent, relative)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		childRelative := filepath.ToSlash(filepath.Join(relative, entry.Name()))
		handle, err := openWindowsRelativeAccess(
			parent, entry.Name(), windows.FILE_OPEN,
			windows.FILE_OPEN_REPARSE_POINT|windows.FILE_OPEN_FOR_BACKUP_INTENT|windows.FILE_SYNCHRONOUS_IO_NONALERT,
			windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
			windows.FILE_GENERIC_READ|windows.FILE_READ_ATTRIBUTES|windows.SYNCHRONIZE,
		)
		if err != nil {
			return err
		}
		var opened windows.ByHandleFileInformation
		if err := windows.GetFileInformationByHandle(handle, &opened); err != nil {
			return errors.Join(err, windows.CloseHandle(handle))
		}
		if opened.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
			return errors.Join(
				fmt.Errorf("artifact %s is a reparse point", childRelative),
				windows.CloseHandle(handle),
			)
		}
		if opened.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0 {
			if err := walk.observeDirectory(childRelative); err != nil {
				return errors.Join(err, windows.CloseHandle(handle))
			}
			var walkErr error
			if transition != nil {
				walkErr = transition(childRelative, "directory-opened")
			}
			if walkErr == nil {
				walkErr = walkWindowsRegularFiles(handle, childRelative, walk, transition)
			}
			closeErr := windows.CloseHandle(handle)
			if err := errors.Join(walkErr, closeErr, verifyWindowsEntryIdentity(parent, entry.Name(), opened)); err != nil {
				return err
			}
			continue
		}
		file := os.NewFile(uintptr(handle), childRelative)
		info, statErr := file.Stat()
		visitErr := statErr
		if visitErr == nil {
			if transition != nil {
				visitErr = transition(childRelative, "file-opened")
			}
		}
		if visitErr == nil {
			visitErr = walk.observeFile(childRelative, file, info)
		}
		closeErr := file.Close()
		if err := errors.Join(visitErr, closeErr, verifyWindowsEntryIdentity(parent, entry.Name(), opened)); err != nil {
			return err
		}
	}
	return nil
}

func (stage *stageDirectoryAuthority) syncContents() error {
	if stage == nil || stage.handle == windows.InvalidHandle {
		return errors.New("stage directory authority is closed")
	}
	return syncWindowsDirectoryContents(stage.handle, "", stage.transition, defaultEvidenceStoreMeter())
}

func syncWindowsDirectoryContents(
	parent windows.Handle,
	relative string,
	transition func(string, string) error,
	meter *evidenceStoreMeter,
) error {
	entries, err := readWindowsDirectory(parent, relative)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		childRelative := filepath.ToSlash(filepath.Join(relative, entry.Name()))
		handle, err := openWindowsRelativeAccess(
			parent, entry.Name(), windows.FILE_OPEN,
			windows.FILE_OPEN_REPARSE_POINT|windows.FILE_OPEN_FOR_BACKUP_INTENT|windows.FILE_SYNCHRONOUS_IO_NONALERT,
			windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
			windows.FILE_GENERIC_READ|windows.FILE_READ_ATTRIBUTES|windows.FILE_WRITE_ATTRIBUTES|windows.SYNCHRONIZE,
		)
		if err != nil {
			return err
		}
		var opened windows.ByHandleFileInformation
		if err := windows.GetFileInformationByHandle(handle, &opened); err != nil {
			return errors.Join(err, windows.CloseHandle(handle))
		}
		if opened.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
			return errors.Join(
				fmt.Errorf("refusing to sync reparse-point artifact %s", childRelative),
				windows.CloseHandle(handle),
			)
		}
		if opened.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0 {
			if err := meter.observeDirectory(childRelative, evidenceRelativeDepth(childRelative)); err != nil {
				return errors.Join(err, windows.CloseHandle(handle))
			}
			var syncErr error
			if transition != nil {
				syncErr = transition(childRelative, "directory-opened")
			}
			if syncErr == nil {
				syncErr = syncWindowsDirectoryContents(handle, childRelative, transition, meter)
			}
			closeErr := windows.CloseHandle(handle)
			if err := errors.Join(syncErr, closeErr, verifyWindowsEntryIdentity(parent, entry.Name(), opened)); err != nil {
				return err
			}
			continue
		}
		var openedSize int64 = int64(opened.FileSizeHigh)<<32 | int64(opened.FileSizeLow)
		if err := meter.observeFile(
			childRelative, evidenceRelativeDepth(childRelative), openedSize, evidenceArtifactFile,
		); err != nil {
			return errors.Join(err, windows.CloseHandle(handle))
		}
		if transition != nil {
			if err := transition(childRelative, "file-opened"); err != nil {
				return errors.Join(err, windows.CloseHandle(handle))
			}
		}
		syncErr := syncWindowsRegularFile(parent, entry.Name(), handle, opened)
		closeErr := windows.CloseHandle(handle)
		if err := errors.Join(syncErr, closeErr, verifyWindowsEntryIdentity(parent, entry.Name(), opened)); err != nil {
			return err
		}
	}
	return nil
}

func syncWindowsRegularFile(
	parent windows.Handle,
	name string,
	attributeHandle windows.Handle,
	opened windows.ByHandleFileInformation,
) error {
	var original windowsFileBasicInfo
	if err := windows.GetFileInformationByHandleEx(
		attributeHandle, windows.FileBasicInfo, (*byte)(unsafe.Pointer(&original)), uint32(unsafe.Sizeof(original)),
	); err != nil {
		return err
	}
	writable := original
	writable.attributes &^= windows.FILE_ATTRIBUTE_READONLY
	if writable.attributes == 0 {
		writable.attributes = windows.FILE_ATTRIBUTE_NORMAL
	}
	if err := setWindowsFileBasicInfo(attributeHandle, writable); err != nil {
		return err
	}
	writeHandle, openErr := openWindowsRelativeAccess(
		parent, name, windows.FILE_OPEN,
		windows.FILE_NON_DIRECTORY_FILE|windows.FILE_OPEN_REPARSE_POINT|windows.FILE_SYNCHRONOUS_IO_NONALERT,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		windows.FILE_GENERIC_READ|windows.FILE_GENERIC_WRITE|windows.SYNCHRONIZE,
	)
	if openErr == nil {
		var writeInfo windows.ByHandleFileInformation
		openErr = windows.GetFileInformationByHandle(writeHandle, &writeInfo)
		if openErr == nil && windowsFileIdentity(writeInfo) != windowsFileIdentity(opened) {
			openErr = errors.New("artifact changed before handle-relative sync")
		}
	}
	var syncErr, closeErr error
	if openErr == nil {
		syncErr = windows.FlushFileBuffers(writeHandle)
	}
	if writeHandle != windows.InvalidHandle {
		closeErr = windows.CloseHandle(writeHandle)
	}
	restoreErr := setWindowsFileBasicInfo(attributeHandle, original)
	return errors.Join(openErr, syncErr, closeErr, restoreErr)
}

func setWindowsFileBasicInfo(handle windows.Handle, info windowsFileBasicInfo) error {
	return windows.SetFileInformationByHandle(
		handle, windows.FileBasicInfo, (*byte)(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info)),
	)
}

func verifyWindowsEntryIdentity(
	parent windows.Handle,
	name string,
	expected windows.ByHandleFileInformation,
) error {
	handle, err := openWindowsRelativeAccess(
		parent, name, windows.FILE_OPEN,
		windows.FILE_OPEN_REPARSE_POINT|windows.FILE_OPEN_FOR_BACKUP_INTENT|windows.FILE_SYNCHRONOUS_IO_NONALERT,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		windows.FILE_READ_ATTRIBUTES|windows.SYNCHRONIZE,
	)
	if err != nil {
		return err
	}
	var observed windows.ByHandleFileInformation
	observeErr := windows.GetFileInformationByHandle(handle, &observed)
	if observeErr != nil {
		return errors.Join(observeErr, windows.CloseHandle(handle))
	}
	if observed.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 ||
		windowsFileIdentity(observed) != windowsFileIdentity(expected) {
		return errors.Join(
			errors.New("filesystem entry changed during handle-relative traversal"),
			windows.CloseHandle(handle),
		)
	}
	return windows.CloseHandle(handle)
}

func windowsFileIdentity(info windows.ByHandleFileInformation) directoryIdentity {
	return directoryIdentity{
		volume: uint64(info.VolumeSerialNumber),
		object: uint64(info.FileIndexHigh)<<32 | uint64(info.FileIndexLow),
	}
}

func (authority *outputRootAuthority) readDir() ([]os.DirEntry, error) {
	if err := authority.verifyPath(); err != nil {
		return nil, err
	}
	entries, err := readWindowsDirectory(authority.handle, authority.path)
	if err != nil {
		return nil, err
	}
	meter := defaultEvidenceStoreMeter()
	if err := meter.observeRootEntries(len(entries)); err != nil {
		return nil, err
	}
	return entries, nil
}

func (authority *outputRootAuthority) removeChild(name string, transition func(string) error) error {
	if authority == nil || authority.handle == windows.InvalidHandle {
		return errors.New("evidence output authority is closed")
	}
	return removeWindowsEntryAt(authority.handle, name, name, transition)
}

func openWindowsRelative(
	parent windows.Handle,
	name string,
	disposition uint32,
	options uint32,
) (windows.Handle, error) {
	return openWindowsRelativeShared(
		parent, name, disposition, options,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
	)
}

func openWindowsRelativeShared(
	parent windows.Handle,
	name string,
	disposition uint32,
	options uint32,
	share uint32,
) (windows.Handle, error) {
	return openWindowsRelativeAccess(
		parent, name, disposition, options, share,
		windows.FILE_LIST_DIRECTORY|windows.FILE_TRAVERSE|windows.FILE_READ_ATTRIBUTES|
			windows.FILE_WRITE_ATTRIBUTES|windows.DELETE|windows.SYNCHRONIZE,
	)
}

func openWindowsRelativeAccess(
	parent windows.Handle,
	name string,
	disposition uint32,
	options uint32,
	share uint32,
	access uint32,
) (windows.Handle, error) {
	objectName, err := windows.NewNTUnicodeString(name)
	if err != nil {
		return windows.InvalidHandle, err
	}
	attributes := windows.OBJECT_ATTRIBUTES{
		Length: uint32(unsafe.Sizeof(windows.OBJECT_ATTRIBUTES{})), RootDirectory: parent, ObjectName: objectName,
	}
	var status windows.IO_STATUS_BLOCK
	allocationSize := int64(0)
	handle := windows.InvalidHandle
	err = windows.NtCreateFile(
		&handle,
		access,
		&attributes,
		&status,
		&allocationSize,
		windows.FILE_ATTRIBUTE_NORMAL,
		share,
		disposition,
		options,
		0,
		0,
	)
	return handle, err
}

func removeWindowsEntryAt(
	parent windows.Handle,
	name string,
	relative string,
	transition func(string) error,
) error {
	for range cleanupMutationLimit {
		handle, err := openWindowsRelative(
			parent, name, windows.FILE_OPEN,
			windows.FILE_OPEN_REPARSE_POINT|windows.FILE_OPEN_FOR_BACKUP_INTENT|windows.FILE_SYNCHRONOUS_IO_NONALERT,
		)
		if windowsPathAbsent(err) {
			return nil
		}
		if err != nil {
			return err
		}
		var info windows.ByHandleFileInformation
		if err := windows.GetFileInformationByHandle(handle, &info); err != nil {
			return errors.Join(err, windows.CloseHandle(handle))
		}
		if transition != nil {
			if err := transition(relative); err != nil {
				return errors.Join(err, windows.CloseHandle(handle))
			}
		}
		realDirectory := info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0 &&
			info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT == 0
		if realDirectory {
			if err := emptyWindowsDirectory(handle, relative, transition); err != nil {
				return errors.Join(err, windows.CloseHandle(handle))
			}
		}
		deleteErr := markWindowsHandleForDeletion(handle)
		closeErr := windows.CloseHandle(handle)
		if windowsDirectoryNotEmpty(deleteErr) && closeErr == nil {
			continue
		}
		if err := errors.Join(deleteErr, closeErr); err != nil {
			return err
		}
	}
	return fmt.Errorf("directory entry %s kept changing during handle-relative cleanup", relative)
}

func emptyWindowsDirectory(
	handle windows.Handle,
	relative string,
	transition func(string) error,
) error {
	entries, err := readWindowsDirectory(handle, relative)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		child := filepath.ToSlash(filepath.Join(relative, entry.Name()))
		if err := removeWindowsEntryAt(handle, entry.Name(), child, transition); err != nil {
			return err
		}
	}
	return nil
}

func readWindowsDirectory(handle windows.Handle, name string) ([]os.DirEntry, error) {
	enumeration, err := reopenWindowsDirectory(handle)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(enumeration), name)
	maximumEntries := DefaultEvidenceStoreBudget().MaxObjects
	var entries []os.DirEntry
	var readErr error
	for {
		batch, err := file.ReadDir(evidenceStoreReadBatch)
		if len(batch) > maximumEntries-len(entries) {
			readErr = fmt.Errorf("evidence directory %s exceeds %d entries", name, maximumEntries)
			break
		}
		entries = append(entries, batch...)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			readErr = err
			break
		}
	}
	return entries, errors.Join(readErr, file.Close())
}

func reopenWindowsDirectory(handle windows.Handle) (windows.Handle, error) {
	const maximumNTPathCharacters = 32_768
	path := make([]uint16, maximumNTPathCharacters)
	length, err := windows.GetFinalPathNameByHandle(handle, &path[0], uint32(len(path)), 0)
	if err != nil {
		return windows.InvalidHandle, err
	}
	if length == 0 || length >= uint32(len(path)) {
		return windows.InvalidHandle, errors.New("directory authority path exceeded the NT path limit")
	}
	reopened, err := openWindowsDirectory(
		windows.UTF16ToString(path[:length]),
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
	)
	if err != nil {
		return windows.InvalidHandle, err
	}
	expected, expectedErr := windowsDirectoryIdentity(handle)
	observed, observedErr := windowsDirectoryIdentity(reopened)
	if err := errors.Join(expectedErr, observedErr); err != nil {
		return windows.InvalidHandle, errors.Join(err, windows.CloseHandle(reopened))
	}
	if expected != observed {
		return windows.InvalidHandle, errors.Join(
			errors.New("directory changed while reopening its enumeration handle"),
			windows.CloseHandle(reopened),
		)
	}
	return reopened, nil
}

func windowsDirectoryNotEmpty(err error) bool {
	if status, ok := errors.AsType[windows.NTStatus](err); ok {
		err = status.Errno()
	}
	return errors.Is(err, windows.ERROR_DIR_NOT_EMPTY)
}

func markWindowsHandleForDeletion(handle windows.Handle) error {
	flags := uint32(
		windows.FILE_DISPOSITION_DELETE |
			windows.FILE_DISPOSITION_POSIX_SEMANTICS |
			windows.FILE_DISPOSITION_IGNORE_READONLY_ATTRIBUTE,
	)
	return windows.SetFileInformationByHandle(
		handle, windows.FileDispositionInfoEx, (*byte)(unsafe.Pointer(&flags)), uint32(unsafe.Sizeof(flags)),
	)
}

func windowsPathAbsent(err error) bool {
	if err == nil {
		return false
	}
	if status, ok := errors.AsType[windows.NTStatus](err); ok {
		err = status.Errno()
	}
	return errors.Is(err, windows.ERROR_FILE_NOT_FOUND) || errors.Is(err, windows.ERROR_PATH_NOT_FOUND)
}

func authorityChildAbsent(err error) bool {
	return windowsPathAbsent(err)
}

func platformPathKey(path string) string {
	// Preserve path spelling because Windows directories can opt into
	// case-sensitive lookup. Lower-casing would collapse distinct compiled
	// inputs; existing aliases are compared by file identity in samePath.
	clean := filepath.Clean(path)
	if len(clean) >= 2 && clean[1] == ':' {
		clean = strings.ToUpper(clean[:1]) + clean[1:]
	}
	return clean
}

func platformPathAlias(left, right string) bool {
	leftInfo, leftErr := os.Stat(left)
	rightInfo, rightErr := os.Stat(right)
	if leftErr == nil && rightErr == nil {
		// SameFile is the authority when a case-sensitive directory contains
		// two spellings that differ only by case; EqualFold alone would merge
		// those distinct objects.
		return os.SameFile(leftInfo, rightInfo)
	}
	return false
}

type memoryStatusEx struct {
	length            uint32
	memoryLoad        uint32
	totalPhysical     uint64
	availablePhysical uint64
	totalPageFile     uint64
	availablePageFile uint64
	totalVirtual      uint64
	availableVirtual  uint64
	availableExtended uint64
}

var globalMemoryStatusEx = syscall.NewLazyDLL("kernel32.dll").NewProc("GlobalMemoryStatusEx")

func physicalMemory() (uint64, string, error) {
	status := memoryStatusEx{length: uint32(unsafe.Sizeof(memoryStatusEx{}))}
	success, _, callErr := globalMemoryStatusEx.Call(uintptr(unsafe.Pointer(&status)))
	if success == 0 {
		if callErr == nil || errors.Is(callErr, syscall.Errno(0)) {
			callErr = errors.New("GlobalMemoryStatusEx returned failure")
		}
		return 0, "GlobalMemoryStatusEx", callErr
	}
	if status.totalPhysical == 0 {
		return 0, "GlobalMemoryStatusEx", errors.New("physical memory was zero")
	}
	return status.totalPhysical, "GlobalMemoryStatusEx", nil
}

func cpuModel() (model string, resultErr error) {
	key, err := registry.OpenKey(
		registry.LOCAL_MACHINE,
		`HARDWARE\DESCRIPTION\System\CentralProcessor\0`,
		registry.QUERY_VALUE,
	)
	if err != nil {
		return "", err
	}
	defer func() { resultErr = errors.Join(resultErr, key.Close()) }()
	model, _, err = key.GetStringValue("ProcessorNameString")
	if err != nil {
		return "", err
	}
	return model, nil
}

func osDescription() string {
	version := windows.RtlGetVersion()
	return fmt.Sprintf("Windows %d.%d build %d", version.MajorVersion, version.MinorVersion, version.BuildNumber)
}

func currentProcessToken() (string, error) {
	return windowsProcessToken(os.Getpid())
}

func processMatches(processID int, token string) (matches bool, resultErr error) {
	if processID <= 0 {
		return false, nil
	}
	handle, err := windows.OpenProcess(
		windows.SYNCHRONIZE|windows.PROCESS_QUERY_LIMITED_INFORMATION,
		false,
		uint32(processID),
	)
	if err != nil {
		if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
			return false, nil
		}
		return false, err
	}
	defer func() { resultErr = errors.Join(resultErr, windows.CloseHandle(handle)) }()
	result, err := windows.WaitForSingleObject(handle, 0)
	if err != nil {
		return false, err
	}
	if result != uint32(windows.WAIT_TIMEOUT) {
		return false, nil
	}
	observed, err := windowsProcessTokenFromHandle(handle)
	if err != nil {
		return false, err
	}
	return observed == token, nil
}

func windowsProcessToken(processID int) (token string, resultErr error) {
	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(processID))
	if err != nil {
		return "", err
	}
	defer func() { resultErr = errors.Join(resultErr, windows.CloseHandle(handle)) }()
	return windowsProcessTokenFromHandle(handle)
}

func windowsProcessTokenFromHandle(handle windows.Handle) (string, error) {
	var creation, exit, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(handle, &creation, &exit, &kernel, &user); err != nil {
		return "", err
	}
	value := uint64(creation.HighDateTime)<<32 | uint64(creation.LowDateTime)
	return strconv.FormatUint(value, 16), nil
}

func (authority *outputRootAuthority) sync() error {
	if authority == nil || authority.handle == windows.InvalidHandle {
		return errors.New("evidence output authority is closed")
	}
	// FlushFileBuffers on a write-authorized directory handle issues a
	// synchronous filesystem flush for its metadata. Returning the platform
	// error is essential: a filesystem without this primitive cannot claim a
	// durable content-addressed namespace publication.
	if err := windows.FlushFileBuffers(authority.handle); err != nil {
		return fmt.Errorf("flush evidence directory metadata: %w", err)
	}
	return nil
}

func (authority *outputRootAuthority) renameChildNoReplace(
	stage *stageDirectoryAuthority,
	destination string,
) error {
	if authority == nil || authority.handle == windows.InvalidHandle {
		return errors.New("evidence output authority is closed")
	}
	if err := requireDirectChildName(destination); err != nil {
		return err
	}
	if err := stage.verifyName(authority); err != nil {
		return err
	}
	if stage.transition != nil {
		if err := stage.transition("", "rename-source-verified"); err != nil {
			return err
		}
	}
	// Normal child processes need pathname access below the stage, so its
	// long-lived authority deliberately denies delete sharing. At the rename
	// boundary we release that lease, reopen the direct child with DELETE
	// authority, and compare its file ID before renaming the exact handle.
	// A name swap in the reopen window therefore fails instead of publishing
	// the replacement.
	expected := stage.identity
	if err := stage.close(); err != nil {
		return err
	}
	renameHandle, err := openWindowsRelativeAccess(
		authority.handle, stage.name, windows.FILE_OPEN,
		windows.FILE_DIRECTORY_FILE|windows.FILE_OPEN_REPARSE_POINT|windows.FILE_SYNCHRONOUS_IO_NONALERT,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		windows.FILE_READ_ATTRIBUTES|windows.DELETE|windows.SYNCHRONIZE,
	)
	if err != nil {
		return err
	}
	closeRenameHandle := func(operationErr error) error {
		return errors.Join(operationErr, windows.CloseHandle(renameHandle))
	}
	observed, err := windowsDirectoryIdentity(renameHandle)
	if err != nil {
		return closeRenameHandle(err)
	}
	if observed != expected {
		return closeRenameHandle(errors.New("stage name changed while acquiring rename authority"))
	}
	encoded, err := windows.UTF16FromString(destination)
	if err != nil {
		return closeRenameHandle(err)
	}
	nameBytes := (len(encoded) - 1) * 2
	var layout windowsFileRenameInformation
	buffer := make([]byte, int(unsafe.Offsetof(layout.fileName))+nameBytes)
	information := (*windowsFileRenameInformation)(unsafe.Pointer(&buffer[0]))
	information.rootDirectory = authority.handle
	information.fileNameLength = uint32(nameBytes)
	copy(
		(*[windows.MAX_LONG_PATH]uint16)(unsafe.Pointer(&information.fileName[0]))[:nameBytes/2:nameBytes/2],
		encoded,
	)
	var status windows.IO_STATUS_BLOCK
	renameErr := windows.NtSetInformationFile(
		renameHandle,
		&status,
		&buffer[0],
		uint32(len(buffer)),
		windows.FileRenameInformation,
	)
	return closeRenameHandle(renameErr)
}
