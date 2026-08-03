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
	"strings"
	"sync"
	"syscall"

	"golang.org/x/sys/windows"
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
