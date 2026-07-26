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
// because either change alters the exact output plan represented by resume intent.
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
	identity       SelectionIdentity
	resumeIntent   ResumeIntent
	canonical      CanonicalSelectionV1
	share          catalog.ShareInstance
	root           catalog.DirectoryID
	rootGeneration catalog.DirectoryGeneration
	directories    []OutputSelectionDirectory
	files          []OutputSelectionFile
}

func NewOutputSelection(
	share catalog.ShareInstance,
	root catalog.DirectoryID,
	rootGeneration catalog.DirectoryGeneration,
	directories []OutputSelectionDirectory,
	files []OutputSelectionFile,
) (OutputSelection, error) {
	if share.IsZero() || root.IsZero() || rootGeneration.IsZero() ||
		len(directories)+len(files) > maxSelectionIdentityClaims {
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

	paths := make(map[string]struct{}, len(ownedDirectories)+len(ownedFiles))
	directoryByPath := make(map[string]OutputSelectionDirectory, len(ownedDirectories))
	for _, directory := range ownedDirectories {
		if !validSelectionPath(directory.Path) || directory.DirectoryID.IsZero() || directory.Generation.IsZero() {
			return OutputSelection{}, ErrInvalidOutputSelection
		}
		if _, duplicate := paths[directory.Path]; duplicate {
			return OutputSelection{}, ErrInvalidOutputSelection
		}
		paths[directory.Path] = struct{}{}
		directoryByPath[directory.Path] = directory
	}
	for _, file := range ownedFiles {
		if !validSelectionPath(file.Path) || file.FileID.IsZero() || file.ParentDirectoryID.IsZero() ||
			file.ParentGeneration.IsZero() || file.ExpectedSize > catalog.MaxFileSize {
			return OutputSelection{}, ErrInvalidOutputSelection
		}
		if _, duplicate := paths[file.Path]; duplicate {
			return OutputSelection{}, ErrInvalidOutputSelection
		}
		paths[file.Path] = struct{}{}
		parentPath := selectionParentPath(file.Path)
		if parentPath == "" {
			if file.ParentDirectoryID != root || file.ParentGeneration != rootGeneration {
				return OutputSelection{}, ErrInvalidOutputSelection
			}
			continue
		}
		parent, ok := directoryByPath[parentPath]
		if !ok || parent.DirectoryID != file.ParentDirectoryID || parent.Generation != file.ParentGeneration {
			return OutputSelection{}, ErrInvalidOutputSelection
		}
	}
	for _, directory := range ownedDirectories {
		parentPath := selectionParentPath(directory.Path)
		if parentPath == "" {
			continue
		}
		if _, ok := directoryByPath[parentPath]; !ok {
			return OutputSelection{}, ErrInvalidOutputSelection
		}
	}

	selection := OutputSelection{
		share: share, root: root, rootGeneration: rootGeneration,
		directories: ownedDirectories, files: ownedFiles,
	}
	selection.identity = hashOutputSelection(selection)
	return selection, nil
}

func (selection OutputSelection) Identity() SelectionIdentity { return selection.identity }
func (selection OutputSelection) ResumeIntent() ResumeIntent  { return selection.resumeIntent }
func (selection OutputSelection) CanonicalSelection() CanonicalSelectionV1 {
	return selection.canonical
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

type selectionIdentityRecord struct {
	path      string
	directory *OutputSelectionDirectory
	file      *OutputSelectionFile
}

func hashOutputSelection(selection OutputSelection) SelectionIdentity {
	hash := sha256.New()
	writeSelectionBytes(hash, []byte("windshare/output-selection/v1"))
	writeSelectionBytes(hash, selection.share.Bytes())
	writeSelectionBytes(hash, selection.root.Bytes())
	writeSelectionBytes(hash, selection.rootGeneration.Bytes())
	records := make([]selectionIdentityRecord, 0, len(selection.directories)+len(selection.files))
	for index := range selection.directories {
		records = append(records, selectionIdentityRecord{
			path: selection.directories[index].Path, directory: &selection.directories[index],
		})
	}
	for index := range selection.files {
		records = append(records, selectionIdentityRecord{
			path: selection.files[index].Path, file: &selection.files[index],
		})
	}
	sort.Slice(records, func(left, right int) bool {
		if records[left].path != records[right].path {
			return records[left].path < records[right].path
		}
		return records[left].directory != nil
	})
	var count [8]byte
	binary.BigEndian.PutUint64(count[:], uint64(len(records)))
	_, _ = hash.Write(count[:])
	for _, record := range records {
		if record.directory != nil {
			_, _ = hash.Write([]byte{1})
			writeSelectionBytes(hash, []byte(record.directory.Path))
			writeSelectionBytes(hash, record.directory.DirectoryID.Bytes())
			writeSelectionBytes(hash, record.directory.Generation.Bytes())
			writeSelectionModifiedTime(hash, record.directory.ModifiedTime)
			continue
		}
		_, _ = hash.Write([]byte{2})
		writeSelectionBytes(hash, []byte(record.file.Path))
		writeSelectionBytes(hash, record.file.FileID.Bytes())
		writeSelectionBytes(hash, record.file.ParentDirectoryID.Bytes())
		writeSelectionBytes(hash, record.file.ParentGeneration.Bytes())
		var size [8]byte
		binary.BigEndian.PutUint64(size[:], record.file.ExpectedSize)
		_, _ = hash.Write(size[:])
		writeSelectionModifiedTime(hash, record.file.ModifiedTime)
	}
	var identity SelectionIdentity
	copy(identity[:], hash.Sum(nil))
	return identity
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
