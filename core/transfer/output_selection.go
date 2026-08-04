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
	plan           outputSelectionPlan
}

type outputSelectionPlan interface {
	DirectoryCount() uint64
	FileCount() uint64
	VisitRecords(func(selectionPlanRecord) error) error
}

type memoryOutputSelectionPlan struct {
	records     []selectionPlanRecord
	directories uint64
	files       uint64
}

func (plan *memoryOutputSelectionPlan) DirectoryCount() uint64 { return plan.directories }
func (plan *memoryOutputSelectionPlan) FileCount() uint64      { return plan.files }
func (plan *memoryOutputSelectionPlan) VisitRecords(visit func(selectionPlanRecord) error) error {
	for _, record := range plan.records {
		if err := visit(record); err != nil {
			return err
		}
	}
	return nil
}

func NewOutputSelection(
	share catalog.ShareInstance,
	root catalog.DirectoryID,
	rootGeneration catalog.DirectoryGeneration,
	directories []OutputSelectionDirectory,
	files []OutputSelectionFile,
) (OutputSelection, error) {
	if share.IsZero() || root.IsZero() || rootGeneration.IsZero() ||
		uint64(len(directories))+uint64(len(files)) > maximumSelectionClaims {
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
	nodeClaims := map[catalog.NodeID]struct{}{root.NodeID(): {}}
	for _, directory := range ownedDirectories {
		if !validSelectionPath(directory.Path) || directory.DirectoryID.IsZero() || directory.Generation.IsZero() {
			return OutputSelection{}, ErrInvalidOutputSelection
		}
		if _, duplicate := nodeClaims[directory.DirectoryID.NodeID()]; duplicate {
			return OutputSelection{}, ErrInvalidOutputSelection
		}
		nodeClaims[directory.DirectoryID.NodeID()] = struct{}{}
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
		if _, duplicate := nodeClaims[file.FileID.NodeID()]; duplicate {
			return OutputSelection{}, ErrInvalidOutputSelection
		}
		nodeClaims[file.FileID.NodeID()] = struct{}{}
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

	records := make([]selectionPlanRecord, 0, len(ownedDirectories)+len(ownedFiles))
	for _, directory := range ownedDirectories {
		records = append(records, selectionPlanRecord{
			kind: selectionPlanDirectoryKind, active: true, path: directory.Path,
			directory: plannedDirectory{
				directory: directory.DirectoryID, generation: directory.Generation,
				path: directory.Path, modified: directory.ModifiedTime,
			},
		})
	}
	for _, file := range ownedFiles {
		records = append(records, selectionPlanRecord{
			kind: selectionPlanFileKind, active: true, path: file.Path,
			file: plannedFile{
				file: file.FileID, path: file.Path, expectedSize: file.ExpectedSize,
				modified: file.ModifiedTime, parentDirectory: file.ParentDirectoryID,
				parentGeneration: file.ParentGeneration,
			},
		})
	}
	sort.Slice(records, func(left, right int) bool {
		if records[left].path != records[right].path {
			return records[left].path < records[right].path
		}
		return records[left].kind < records[right].kind
	})
	return newOutputSelectionFromPlan(
		share, root, rootGeneration,
		&memoryOutputSelectionPlan{
			records: records, directories: uint64(len(ownedDirectories)), files: uint64(len(ownedFiles)),
		},
	)
}

func newOutputSelectionFromPlan(
	share catalog.ShareInstance,
	root catalog.DirectoryID,
	rootGeneration catalog.DirectoryGeneration,
	plan outputSelectionPlan,
) (OutputSelection, error) {
	if share.IsZero() || root.IsZero() || rootGeneration.IsZero() || plan == nil ||
		plan.DirectoryCount()+plan.FileCount() > maximumSelectionClaims {
		return OutputSelection{}, ErrInvalidOutputSelection
	}
	if err := validateOutputSelectionPlan(root, rootGeneration, plan); err != nil {
		return OutputSelection{}, err
	}
	selection := OutputSelection{
		share: share, root: root, rootGeneration: rootGeneration, plan: plan,
	}
	identity, err := hashOutputSelection(selection)
	if err != nil {
		return OutputSelection{}, err
	}
	selection.identity = identity
	return selection, nil
}

type outputSelectionDirectoryAuthority struct {
	path       string
	directory  catalog.DirectoryID
	generation catalog.DirectoryGeneration
}

func validateOutputSelectionPlan(
	root catalog.DirectoryID,
	rootGeneration catalog.DirectoryGeneration,
	plan outputSelectionPlan,
) error {
	var directories, files uint64
	var previousPath string
	var ancestry []outputSelectionDirectoryAuthority
	err := plan.VisitRecords(func(record selectionPlanRecord) error {
		if !record.active || !validSelectionPath(record.path) ||
			(previousPath != "" && record.path <= previousPath) {
			return ErrInvalidOutputSelection
		}
		previousPath = record.path
		parentPath := selectionParentPath(record.path)
		for len(ancestry) > 0 && ancestry[len(ancestry)-1].path != parentPath {
			ancestry = ancestry[:len(ancestry)-1]
		}
		if parentPath != "" && len(ancestry) == 0 {
			return ErrInvalidOutputSelection
		}
		switch record.kind {
		case selectionPlanDirectoryKind:
			if record.directory.path != record.path || record.directory.directory.IsZero() ||
				record.directory.generation.IsZero() {
				return ErrInvalidOutputSelection
			}
			directories++
			ancestry = append(ancestry, outputSelectionDirectoryAuthority{
				path: record.path, directory: record.directory.directory,
				generation: record.directory.generation,
			})
		case selectionPlanFileKind:
			if record.file.path != record.path || record.file.file.IsZero() ||
				record.file.parentDirectory.IsZero() || record.file.parentGeneration.IsZero() ||
				record.file.expectedSize > catalog.MaxFileSize {
				return ErrInvalidOutputSelection
			}
			if parentPath == "" {
				if record.file.parentDirectory != root || record.file.parentGeneration != rootGeneration {
					return ErrInvalidOutputSelection
				}
			} else {
				parent := ancestry[len(ancestry)-1]
				if record.file.parentDirectory != parent.directory ||
					record.file.parentGeneration != parent.generation {
					return ErrInvalidOutputSelection
				}
			}
			files++
		default:
			return ErrInvalidOutputSelection
		}
		return nil
	})
	if err != nil {
		return err
	}
	if directories != plan.DirectoryCount() || files != plan.FileCount() {
		return ErrInvalidOutputSelection
	}
	return nil
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
	result := make([]OutputSelectionDirectory, 0, boundedSelectionCapacity(selection.DirectoryCount()))
	if err := selection.VisitDirectories(func(directory OutputSelectionDirectory) error {
		result = append(result, directory)
		return nil
	}); err != nil {
		panic(err)
	}
	return result
}
func (selection OutputSelection) Files() []OutputSelectionFile {
	result := make([]OutputSelectionFile, 0, boundedSelectionCapacity(selection.FileCount()))
	if err := selection.VisitFiles(func(file OutputSelectionFile) error {
		result = append(result, file)
		return nil
	}); err != nil {
		panic(err)
	}
	return result
}

func (selection OutputSelection) DirectoryCount() uint64 {
	if selection.plan == nil {
		return 0
	}
	return selection.plan.DirectoryCount()
}

func (selection OutputSelection) FileCount() uint64 {
	if selection.plan == nil {
		return 0
	}
	return selection.plan.FileCount()
}

func (selection OutputSelection) VisitDirectories(
	visit func(OutputSelectionDirectory) error,
) error {
	if selection.plan == nil || visit == nil {
		return ErrInvalidOutputSelection
	}
	return selection.plan.VisitRecords(func(record selectionPlanRecord) error {
		if record.kind != selectionPlanDirectoryKind {
			return nil
		}
		return visit(OutputSelectionDirectory{
			Path: record.path, DirectoryID: record.directory.directory,
			Generation: record.directory.generation, ModifiedTime: record.directory.modified,
		})
	})
}

func (selection OutputSelection) VisitFiles(visit func(OutputSelectionFile) error) error {
	if selection.plan == nil || visit == nil {
		return ErrInvalidOutputSelection
	}
	return selection.plan.VisitRecords(func(record selectionPlanRecord) error {
		if record.kind != selectionPlanFileKind {
			return nil
		}
		return visit(OutputSelectionFile{
			Path: record.path, FileID: record.file.file,
			ParentDirectoryID: record.file.parentDirectory,
			ParentGeneration:  record.file.parentGeneration,
			ExpectedSize:      record.file.expectedSize, ModifiedTime: record.file.modified,
		})
	})
}

func boundedSelectionCapacity(count uint64) int {
	if count > uint64(^uint(0)>>1) {
		return 0
	}
	return int(count)
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

func hashOutputSelection(selection OutputSelection) (SelectionIdentity, error) {
	hash := sha256.New()
	writeSelectionBytes(hash, []byte("windshare/output-selection/v1"))
	writeSelectionBytes(hash, selection.share.Bytes())
	writeSelectionBytes(hash, selection.root.Bytes())
	writeSelectionBytes(hash, selection.rootGeneration.Bytes())
	var count [8]byte
	binary.BigEndian.PutUint64(count[:], selection.DirectoryCount()+selection.FileCount())
	_, _ = hash.Write(count[:])
	err := selection.plan.VisitRecords(func(record selectionPlanRecord) error {
		if record.kind == selectionPlanDirectoryKind {
			_, _ = hash.Write([]byte{1})
			writeSelectionBytes(hash, []byte(record.path))
			writeSelectionBytes(hash, record.directory.directory.Bytes())
			writeSelectionBytes(hash, record.directory.generation.Bytes())
			writeSelectionModifiedTime(hash, record.directory.modified)
			return nil
		}
		_, _ = hash.Write([]byte{2})
		writeSelectionBytes(hash, []byte(record.path))
		writeSelectionBytes(hash, record.file.file.Bytes())
		writeSelectionBytes(hash, record.file.parentDirectory.Bytes())
		writeSelectionBytes(hash, record.file.parentGeneration.Bytes())
		var size [8]byte
		binary.BigEndian.PutUint64(size[:], record.file.expectedSize)
		_, _ = hash.Write(size[:])
		writeSelectionModifiedTime(hash, record.file.modified)
		return nil
	})
	if err != nil {
		return SelectionIdentity{}, err
	}
	var identity SelectionIdentity
	copy(identity[:], hash.Sum(nil))
	return identity, nil
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
