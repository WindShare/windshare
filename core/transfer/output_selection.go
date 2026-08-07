package transfer

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"slices"
	"sort"
	"strings"

	"github.com/windshare/windshare/core/catalog"
)

const SelectionIdentityBytes = sha256.Size

var ErrInvalidOutputSelection = errors.New("transfer output selection is invalid")

type SelectionIdentity [SelectionIdentityBytes]byte

func SelectionIdentityFromBytes(raw []byte) (SelectionIdentity, error) {
	if len(raw) != SelectionIdentityBytes {
		return SelectionIdentity{}, ErrInvalidOutputSelection
	}
	var identity SelectionIdentity
	copy(identity[:], raw)
	if identity.IsZero() {
		return SelectionIdentity{}, ErrInvalidOutputSelection
	}
	return identity, nil
}

func (identity SelectionIdentity) Bytes() []byte { return append([]byte(nil), identity[:]...) }
func (identity SelectionIdentity) IsZero() bool  { return identity == SelectionIdentity{} }

// OutputSelectionDirectory binds both the authenticated generation and metadata
// because either change alters the terminal observation for one run.
type OutputSelectionDirectory struct {
	Path         string
	DirectoryID  catalog.DirectoryID
	Generation   catalog.DirectoryGeneration
	ModifiedTime catalog.ModifiedTime
}

// OutputSelectionFile binds a canonical output locator to the catalog generation
// that authenticated the file entry. The later revision lease remains a distinct
// binding because output admission must finish before OpenRevision is allowed.
type OutputSelectionFile struct {
	Path              string
	FileID            catalog.FileID
	ParentDirectoryID catalog.DirectoryID
	ParentGeneration  catalog.DirectoryGeneration
	ExpectedSize      uint64
	ModifiedTime      catalog.ModifiedTime
}

type OutputSelection struct {
	identity             SelectionIdentity
	selectionObservation SelectionObservationV1
	terminalObservation  TerminalSelectionObservationV1
	share                catalog.ShareInstance
	root                 catalog.DirectoryID
	rootGeneration       catalog.DirectoryGeneration
	directories          []OutputSelectionDirectory
	files                []OutputSelectionFile
}

func NewOutputSelection(
	share catalog.ShareInstance,
	root catalog.DirectoryID,
	rootGeneration catalog.DirectoryGeneration,
	directories []OutputSelectionDirectory,
	files []OutputSelectionFile,
) (OutputSelection, error) {
	if share.IsZero() || root.IsZero() || rootGeneration.IsZero() ||
		len(directories) > catalog.DefaultShareCommittedEntries ||
		len(files) > catalog.DefaultShareCommittedEntries-len(directories) {
		return OutputSelection{}, ErrInvalidOutputSelection
	}
	ownedDirectories := slices.Clone(directories)
	ownedFiles := slices.Clone(files)
	sort.Slice(ownedDirectories, func(left, right int) bool {
		return ownedDirectories[left].Path < ownedDirectories[right].Path
	})
	sort.Slice(ownedFiles, func(left, right int) bool {
		return ownedFiles[left].Path < ownedFiles[right].Path
	})
	if err := validateOutputSelection(root, rootGeneration, ownedDirectories, ownedFiles); err != nil {
		return OutputSelection{}, err
	}
	selection := OutputSelection{
		share: share, root: root, rootGeneration: rootGeneration,
		directories: ownedDirectories, files: ownedFiles,
	}
	selection.identity = hashOutputSelection(selection)
	return selection, nil
}

type outputSelectionClaimSet struct {
	paths map[string]struct{}
	nodes map[catalog.NodeID]struct{}
}

func validateOutputSelection(
	root catalog.DirectoryID,
	rootGeneration catalog.DirectoryGeneration,
	directories []OutputSelectionDirectory,
	files []OutputSelectionFile,
) error {
	claims := outputSelectionClaimSet{
		paths: make(map[string]struct{}, len(directories)+len(files)),
		nodes: map[catalog.NodeID]struct{}{root.NodeID(): {}},
	}
	directoryByPath, err := indexOutputSelectionDirectories(directories, &claims)
	if err != nil {
		return err
	}
	if err := validateOutputSelectionFiles(
		root, rootGeneration, files, directoryByPath, &claims,
	); err != nil {
		return err
	}
	return validateOutputSelectionDirectoryParents(directories, directoryByPath)
}

func (claims *outputSelectionClaimSet) add(path string, node catalog.NodeID) error {
	if !validSelectionPath(path) || node.IsZero() {
		return ErrInvalidOutputSelection
	}
	if _, duplicate := claims.nodes[node]; duplicate {
		return ErrInvalidOutputSelection
	}
	if _, duplicate := claims.paths[path]; duplicate {
		return ErrInvalidOutputSelection
	}
	claims.nodes[node] = struct{}{}
	claims.paths[path] = struct{}{}
	return nil
}

func indexOutputSelectionDirectories(
	directories []OutputSelectionDirectory,
	claims *outputSelectionClaimSet,
) (map[string]OutputSelectionDirectory, error) {
	directoryByPath := make(map[string]OutputSelectionDirectory, len(directories))
	for _, directory := range directories {
		if directory.Generation.IsZero() {
			return nil, ErrInvalidOutputSelection
		}
		if err := claims.add(directory.Path, directory.DirectoryID.NodeID()); err != nil {
			return nil, err
		}
		directoryByPath[directory.Path] = directory
	}
	return directoryByPath, nil
}

func validateOutputSelectionFiles(
	root catalog.DirectoryID,
	rootGeneration catalog.DirectoryGeneration,
	files []OutputSelectionFile,
	directoryByPath map[string]OutputSelectionDirectory,
	claims *outputSelectionClaimSet,
) error {
	for _, file := range files {
		if file.ParentDirectoryID.IsZero() || file.ParentGeneration.IsZero() ||
			file.ExpectedSize > catalog.MaxFileSize {
			return ErrInvalidOutputSelection
		}
		if err := claims.add(file.Path, file.FileID.NodeID()); err != nil {
			return err
		}
		if err := validateOutputSelectionFileParent(
			root, rootGeneration, file, directoryByPath,
		); err != nil {
			return err
		}
	}
	return nil
}

func validateOutputSelectionFileParent(
	root catalog.DirectoryID,
	rootGeneration catalog.DirectoryGeneration,
	file OutputSelectionFile,
	directoryByPath map[string]OutputSelectionDirectory,
) error {
	parentPath := selectionParentPath(file.Path)
	if parentPath == "" {
		if file.ParentDirectoryID != root || file.ParentGeneration != rootGeneration {
			return ErrInvalidOutputSelection
		}
		return nil
	}
	parent, ok := directoryByPath[parentPath]
	if !ok || parent.DirectoryID != file.ParentDirectoryID || parent.Generation != file.ParentGeneration {
		return ErrInvalidOutputSelection
	}
	return nil
}

func validateOutputSelectionDirectoryParents(
	directories []OutputSelectionDirectory,
	directoryByPath map[string]OutputSelectionDirectory,
) error {
	for _, directory := range directories {
		parentPath := selectionParentPath(directory.Path)
		if parentPath == "" {
			continue
		}
		if _, ok := directoryByPath[parentPath]; !ok {
			return ErrInvalidOutputSelection
		}
	}
	return nil
}

// Identity is a terminal catalog observation. It is not a durable output key.
func (selection OutputSelection) Identity() SelectionIdentity { return selection.identity }
func (selection OutputSelection) SelectionObservation() SelectionObservationV1 {
	return selection.selectionObservation
}

// TerminalObservation returns the full catalog observation only for audit or
// diagnostics. It is intentionally unsuitable as a checkpoint namespace.
func (selection OutputSelection) TerminalObservation() TerminalSelectionObservationV1 {
	return selection.terminalObservation
}
func (selection OutputSelection) ShareInstance() catalog.ShareInstance { return selection.share }
func (selection OutputSelection) SyntheticRoot() catalog.DirectoryID   { return selection.root }
func (selection OutputSelection) RootGeneration() catalog.DirectoryGeneration {
	return selection.rootGeneration
}
func (selection OutputSelection) Directories() []OutputSelectionDirectory {
	return slices.Clone(selection.directories)
}
func (selection OutputSelection) Files() []OutputSelectionFile {
	return slices.Clone(selection.files)
}
func (selection OutputSelection) DirectoryCount() uint64 { return uint64(len(selection.directories)) }
func (selection OutputSelection) FileCount() uint64      { return uint64(len(selection.files)) }

func (selection OutputSelection) VisitDirectories(
	visit func(OutputSelectionDirectory) error,
) error {
	if selection.identity.IsZero() || visit == nil {
		return ErrInvalidOutputSelection
	}
	for _, directory := range selection.directories {
		if err := visit(directory); err != nil {
			return err
		}
	}
	return nil
}

func (selection OutputSelection) VisitFiles(visit func(OutputSelectionFile) error) error {
	if selection.identity.IsZero() || visit == nil {
		return ErrInvalidOutputSelection
	}
	for _, file := range selection.files {
		if err := visit(file); err != nil {
			return err
		}
	}
	return nil
}

func validSelectionPath(path string) bool {
	if path == "" {
		return false
	}
	canonical, err := catalog.CanonicalPath(path)
	return err == nil && canonical == path
}

func selectionParentPath(path string) string {
	index := strings.LastIndexByte(path, '/')
	if index < 0 {
		return ""
	}
	return path[:index]
}

func hashOutputSelection(selection OutputSelection) SelectionIdentity {
	hash := sha256.New()
	writeSelectionBytes(hash, []byte("windshare/output-selection/v1"))
	writeSelectionBytes(hash, selection.share.Bytes())
	writeSelectionBytes(hash, selection.root.Bytes())
	writeSelectionBytes(hash, selection.rootGeneration.Bytes())
	var count [8]byte
	binary.BigEndian.PutUint64(count[:], selection.DirectoryCount()+selection.FileCount())
	_, _ = hash.Write(count[:])

	directoryIndex, fileIndex := 0, 0
	for directoryIndex < len(selection.directories) || fileIndex < len(selection.files) {
		if fileIndex == len(selection.files) ||
			(directoryIndex < len(selection.directories) &&
				selection.directories[directoryIndex].Path < selection.files[fileIndex].Path) {
			hashOutputSelectionDirectory(hash, selection.directories[directoryIndex])
			directoryIndex++
			continue
		}
		hashOutputSelectionFile(hash, selection.files[fileIndex])
		fileIndex++
	}
	var identity SelectionIdentity
	copy(identity[:], hash.Sum(nil))
	return identity
}

func hashOutputSelectionDirectory(hash selectionHash, directory OutputSelectionDirectory) {
	_, _ = hash.Write([]byte{1})
	writeSelectionBytes(hash, []byte(directory.Path))
	writeSelectionBytes(hash, directory.DirectoryID.Bytes())
	writeSelectionBytes(hash, directory.Generation.Bytes())
	writeSelectionModifiedTime(hash, directory.ModifiedTime)
}

func hashOutputSelectionFile(hash selectionHash, file OutputSelectionFile) {
	_, _ = hash.Write([]byte{2})
	writeSelectionBytes(hash, []byte(file.Path))
	writeSelectionBytes(hash, file.FileID.Bytes())
	writeSelectionBytes(hash, file.ParentDirectoryID.Bytes())
	writeSelectionBytes(hash, file.ParentGeneration.Bytes())
	var size [8]byte
	binary.BigEndian.PutUint64(size[:], file.ExpectedSize)
	_, _ = hash.Write(size[:])
	writeSelectionModifiedTime(hash, file.ModifiedTime)
}

type selectionHash interface {
	Write([]byte) (int, error)
}

func writeSelectionBytes(hash selectionHash, value []byte) {
	var length [8]byte
	binary.BigEndian.PutUint64(length[:], uint64(len(value)))
	_, _ = hash.Write(length[:])
	_, _ = hash.Write(value)
}

func writeSelectionModifiedTime(hash selectionHash, modified catalog.ModifiedTime) {
	present := byte(0)
	if modified.Present() {
		present = 1
	}
	_, _ = hash.Write([]byte{present})
	var seconds [8]byte
	binary.BigEndian.PutUint64(seconds[:], uint64(modified.Seconds()))
	_, _ = hash.Write(seconds[:])
	var nanoseconds [4]byte
	binary.BigEndian.PutUint32(nanoseconds[:], modified.Nanoseconds())
	_, _ = hash.Write(nanoseconds[:])
	_, _ = hash.Write([]byte{byte(modified.Precision())})
}
