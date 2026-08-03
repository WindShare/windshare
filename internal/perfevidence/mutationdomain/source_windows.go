//go:build windows

package mutationdomain

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

type windowsBootstrapManifest struct {
	Roots       []string                    `json:"roots"`
	Directories []windowsBootstrapDirectory `json:"directories,omitempty"`
	Files       []windowsBootstrapFile      `json:"files"`
}

type windowsBootstrapDirectory struct {
	Root     string `json:"root"`
	Relative string `json:"relative"`
	Mode     uint32 `json:"mode"`
}

type windowsBootstrapFile struct {
	Root       string `json:"root"`
	Relative   string `json:"relative"`
	StagedLeaf string `json:"stagedLeaf"`
	Mode       uint32 `json:"mode"`
	Bytes      int64  `json:"bytes"`
	SHA256     string `json:"sha256"`
}

type retainedWindowsSource struct {
	root        windows.Handle
	directories []retainedWindowsSourceDirectory
	files       []retainedWindowsSourceFile
}

type retainedWindowsSourceDirectory struct {
	relative string
	mode     os.FileMode
	handle   windows.Handle
}

type retainedWindowsSourceFile struct {
	relative string
	mode     os.FileMode
	bytes    int64
	file     *os.File
}

func stageSealedInputs(
	privateRootPath string,
	privateRoot windows.Handle,
	roots []rootSpec,
	creator sealedObjectCreator,
) (string, error) {
	entropy, err := randomBytes(16)
	if err != nil {
		return "", err
	}
	prefix := "bootstrap-" + hex.EncodeToString(entropy)
	manifest := windowsBootstrapManifest{Roots: make([]string, 0, len(roots))}
	budget := productionMutationTraversalBudget()
	for rootIndex := range roots {
		root := &roots[rootIndex]
		if err := budget.admitCandidate(root.Name); err != nil {
			return "", fmt.Errorf("admit Windows source root %s: %w", root.Name, err)
		}
		source, err := acquireRetainedWindowsSourceWithBudget(root.HostPath, budget)
		if err != nil {
			return "", fmt.Errorf("retain Windows source root %s: %w", root.Name, err)
		}
		manifest.Roots = append(manifest.Roots, root.Name)
		var records []string
		for _, directory := range source.directories {
			manifest.Directories = append(manifest.Directories, windowsBootstrapDirectory{
				Root: root.Name, Relative: directory.relative, Mode: uint32(directory.mode.Perm()),
			})
			records = append(records, fmt.Sprintf(
				"D\x00%s\x00%o", filepath.ToSlash(directory.relative), directory.mode.Perm(),
			))
		}
		for _, file := range source.files {
			if _, err := file.file.Seek(0, io.SeekStart); err != nil {
				return "", errors.Join(fmt.Errorf("rewind retained source %s: %w", file.relative, err), source.close())
			}
			stagedLeaf := fmt.Sprintf("%s-%08x", prefix, len(manifest.Files))
			_, digest, copyErr := copySealedFile(file.file, privateRoot, stagedLeaf, creator, false)
			if copyErr != nil {
				return "", errors.Join(
					fmt.Errorf("create sealed staged leaf %s for %s: %w", stagedLeaf, file.relative, copyErr),
					source.close(),
				)
			}
			manifest.Files = append(manifest.Files, windowsBootstrapFile{
				Root: root.Name, Relative: file.relative, StagedLeaf: stagedLeaf,
				Mode: uint32(file.mode.Perm()), Bytes: file.bytes, SHA256: digest,
			})
			records = append(records, fmt.Sprintf(
				"F\x00%s\x00%d\x00%o\x00%s",
				filepath.ToSlash(file.relative), file.bytes, file.mode.Perm(), digest,
			))
		}
		if err := source.close(); err != nil {
			return "", fmt.Errorf("release retained Windows source %s: %w", root.Name, err)
		}
		sort.Strings(records)
		observed := hashBytes([]byte(strings.Join(records, "\n")))
		if root.SHA256 != "" && observed != root.SHA256 {
			return "", fmt.Errorf(
				"staged AppContainer input %s has identity %s, want %s", root.Name, observed, root.SHA256,
			)
		}
		root.SHA256 = observed
	}
	encoded, err := json.Marshal(manifest)
	if err != nil {
		return "", err
	}
	manifestLeaf := prefix + "-manifest.json"
	if _, _, err := copySealedFile(strings.NewReader(string(encoded)), privateRoot, manifestLeaf, creator, false); err != nil {
		return "", err
	}
	return filepath.Join(privateRootPath, manifestLeaf), nil
}

func acquireRetainedWindowsSourceWithBudget(path string, budget *mutationTraversalBudget) (*retainedWindowsSource, error) {
	root, information, err := openRetainedWindowsSourceObject(0, windowsNTPath(path), true)
	if err != nil {
		return nil, err
	}
	source := &retainedWindowsSource{root: root}
	fail := func(operationErr error) (*retainedWindowsSource, error) {
		return nil, errors.Join(operationErr, source.close())
	}
	if information.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return fail(errors.New("Windows source root is a reparse point"))
	}
	finalPath, err := finalWindowsHandlePath(root)
	if err != nil {
		return fail(err)
	}
	if !strings.EqualFold(normalizeWindowsPath(finalPath), normalizeWindowsPath(path)) {
		return fail(fmt.Errorf("Windows source root resolved to %s, want %s", finalPath, path))
	}
	if err := source.acquireDirectory(root, "", 0, budget); err != nil {
		return fail(err)
	}
	return source, nil
}

func (source *retainedWindowsSource) acquireDirectory(
	parent windows.Handle,
	relative string,
	depth int,
	budget *mutationTraversalBudget,
) (resultErr error) {
	duplicate, err := duplicateWindowsHandle(parent)
	if err != nil {
		return err
	}
	directory := os.NewFile(uintptr(duplicate), relative)
	defer func() { resultErr = errors.Join(resultErr, directory.Close()) }()
	seen := make(map[string]struct{})
	for {
		entries, readErr := directory.ReadDir(mutationTreeBatchSize)
		sort.Slice(entries, func(left, right int) bool { return entries[left].Name() < entries[right].Name() })
		for _, entry := range entries {
			name := entry.Name()
			if filepath.Base(name) != name || name == "." || name == ".." {
				return fmt.Errorf("Windows source enumeration returned unsafe leaf %q", name)
			}
			if _, duplicate := seen[name]; duplicate {
				return fmt.Errorf("Windows source enumeration returned duplicate leaf %q", name)
			}
			seen[name] = struct{}{}
			childRelative := name
			if relative != "" {
				childRelative = filepath.Join(relative, name)
			}
			directoryHint := entry.Type().IsDir()
			handle, information, err := openRetainedWindowsSourceObject(parent, name, directoryHint)
			if err != nil {
				return fmt.Errorf("retain Windows source %s: %w", childRelative, err)
			}
			isDirectory := information.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0
			if information.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 || isDirectory != directoryHint {
				_ = windows.CloseHandle(handle)
				return fmt.Errorf("Windows source %s changed type or is a reparse point", childRelative)
			}
			file := os.NewFile(uintptr(handle), childRelative)
			info, statErr := file.Stat()
			if statErr != nil {
				return errors.Join(statErr, file.Close())
			}
			if isDirectory {
				if err := budget.admitObject(childRelative, depth+1, 0); err != nil {
					return errors.Join(err, file.Close())
				}
				source.directories = append(source.directories, retainedWindowsSourceDirectory{
					relative: childRelative, mode: info.Mode(), handle: handle,
				})
				if err := source.acquireDirectory(handle, childRelative, depth+1, budget); err != nil {
					return err
				}
				continue
			}
			if !info.Mode().IsRegular() || information.NumberOfLinks != 1 {
				_ = file.Close()
				return fmt.Errorf("Windows source %s is not a single-link regular file", childRelative)
			}
			if err := budget.admitObject(childRelative, depth+1, info.Size()); err != nil {
				return errors.Join(err, file.Close())
			}
			source.files = append(source.files, retainedWindowsSourceFile{
				relative: childRelative, mode: info.Mode(), bytes: info.Size(), file: file,
			})
		}
		if errors.Is(readErr, io.EOF) {
			return nil
		}
		if readErr != nil {
			return readErr
		}
	}
}

func openRetainedWindowsSourceObject(
	root windows.Handle,
	name string,
	directory bool,
) (windows.Handle, windows.ByHandleFileInformation, error) {
	objectName, err := windows.NewNTUnicodeString(name)
	if err != nil {
		return windows.InvalidHandle, windows.ByHandleFileInformation{}, err
	}
	attributes := &windows.OBJECT_ATTRIBUTES{
		Length: uint32(unsafe.Sizeof(windows.OBJECT_ATTRIBUTES{})), RootDirectory: root,
		ObjectName: objectName, Attributes: windows.OBJ_CASE_INSENSITIVE | windows.OBJ_DONT_REPARSE,
	}
	desired := uint32(windows.FILE_READ_DATA | windows.FILE_READ_ATTRIBUTES | windows.SYNCHRONIZE)
	options := uint32(windows.FILE_NON_DIRECTORY_FILE | windows.FILE_OPEN_REPARSE_POINT | windows.FILE_SYNCHRONOUS_IO_NONALERT)
	if directory {
		desired = windows.FILE_LIST_DIRECTORY | windows.FILE_READ_ATTRIBUTES | windows.FILE_TRAVERSE | windows.SYNCHRONIZE
		options = windows.FILE_DIRECTORY_FILE | windows.FILE_OPEN_REPARSE_POINT | windows.FILE_SYNCHRONOUS_IO_NONALERT
	}
	var handle windows.Handle
	var status windows.IO_STATUS_BLOCK
	err = windows.NtCreateFile(
		&handle, desired, attributes, &status, nil, 0, windows.FILE_SHARE_READ, windows.FILE_OPEN, options, 0, 0,
	)
	if err != nil {
		return windows.InvalidHandle, windows.ByHandleFileInformation{}, err
	}
	var information windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &information); err != nil {
		return windows.InvalidHandle, windows.ByHandleFileInformation{}, errors.Join(err, windows.CloseHandle(handle))
	}
	return handle, information, nil
}

func duplicateWindowsHandle(source windows.Handle) (windows.Handle, error) {
	var duplicate windows.Handle
	err := windows.DuplicateHandle(
		windows.CurrentProcess(), source, windows.CurrentProcess(), &duplicate, 0, false, windows.DUPLICATE_SAME_ACCESS,
	)
	return duplicate, err
}

func (source *retainedWindowsSource) close() error {
	if source == nil {
		return nil
	}
	var errs []error
	for _, file := range source.files {
		if file.file != nil {
			errs = append(errs, file.file.Close())
		}
	}
	for _, directory := range source.directories {
		if directory.handle != 0 && directory.handle != windows.InvalidHandle {
			errs = append(errs, windows.CloseHandle(directory.handle))
		}
	}
	if source.root != 0 && source.root != windows.InvalidHandle {
		errs = append(errs, windows.CloseHandle(source.root))
	}
	source.files = nil
	source.directories = nil
	source.root = 0
	return errors.Join(errs...)
}

func copySealedFile(
	source io.Reader,
	parent windows.Handle,
	name string,
	creator sealedObjectCreator,
	retain bool,
) (*os.File, string, error) {
	handle, err := creator.create(parent, name, false)
	if err != nil {
		return nil, "", err
	}
	return copySealedFileHandle(source, handle, name, retain)
}

func copySealedFileHandle(
	source io.Reader,
	handle windows.Handle,
	name string,
	retain bool,
) (*os.File, string, error) {
	file := os.NewFile(uintptr(handle), name)
	hasher := sha256.New()
	_, copyErr := io.Copy(io.MultiWriter(file, hasher), source)
	flushErr := windows.FlushFileBuffers(handle)
	_, seekErr := file.Seek(0, io.SeekStart)
	if err := errors.Join(copyErr, flushErr, seekErr); err != nil {
		return nil, "", errors.Join(err, file.Close())
	}
	digest := hex.EncodeToString(hasher.Sum(nil))
	if retain {
		return file, digest, nil
	}
	return nil, digest, file.Close()
}
