package outputruntime

import (
	"errors"
	"fmt"
	"io/fs"
	"strings"

	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/osfs/internal/outputfault"
	"github.com/windshare/windshare/core/osfs/internal/outputnamespace"
	"github.com/windshare/windshare/core/transfer"
)

var reservedOutputRootPrefixes = []string{
	".windshare-output",
	".wsresume-output",
}

func validateReservedOutputSelection(platform outputcap.Platform, selection transfer.OutputSelection) error {
	reservedKeys := make([]string, len(reservedOutputRootPrefixes))
	for index, reserved := range reservedOutputRootPrefixes {
		key, err := platform.CanonicalComponentKey(reserved)
		if err != nil {
			return frozenSelectionAdmissionFault("canonicalize reserved output namespace", err, false)
		}
		reservedKeys[index] = key
	}
	validate := func(path string) error {
		first, _, _ := strings.Cut(path, "/")
		key, err := platform.CanonicalComponentKey(first)
		if err != nil {
			return err
		}
		// Both operands must pass through the same filesystem key function. NTFS
		// keys use its ordinal upcase table, while ext4 keys preserve bytes; mixing a
		// lowercase constant with an NTFS key would silently admit the control tree.
		for _, reservedKey := range reservedKeys {
			if strings.HasPrefix(key, reservedKey) {
				return outputfault.ErrReservedPath
			}
		}
		return nil
	}
	if err := selection.VisitDirectories(func(directory transfer.OutputSelectionDirectory) error {
		return validate(directory.Path)
	}); err != nil {
		return frozenSelectionAdmissionFault("validate selected directory reservation", err, false)
	}
	if err := selection.VisitFiles(func(file transfer.OutputSelectionFile) error {
		return validate(file.Path)
	}); err != nil {
		return frozenSelectionAdmissionFault("validate selected file reservation", err, false)
	}
	return nil
}

// preflightOutputSelectionAdmission builds the immutable selection authority
// without mutating the output namespace. Keeping every pure platform-key and
// metadata-shape rejection here guarantees probes cannot run for a selection
// whose complete static meaning was never admissible. OpenSelection subsequently
// routes a rejection through the exact intent observation so this function stays
// pure while preexisting state still receives a preservation pause.
func preflightOutputSelectionAdmission(
	platform outputcap.Platform,
	selection transfer.OutputSelection,
) (outputSelectionAdmission, error) {
	if selection.Identity().IsZero() || selection.ResumeIntent().IsZero() {
		return outputSelectionAdmission{}, transfer.ErrInvalidOutputSelection
	}
	admission := outputSelectionAdmission{
		selection: selection,
		files:     make(map[string]transfer.OutputSelectionFile, int(selection.FileCount())),
		dirs:      make(map[string]transfer.OutputSelectionDirectory, int(selection.DirectoryCount())),
	}
	aliases := make(map[string]string, int(selection.FileCount()+selection.DirectoryCount()))
	if err := selection.VisitDirectories(func(directory transfer.OutputSelectionDirectory) error {
		key, err := platform.CanonicalLocatorKey(directory.Path)
		if err != nil {
			return frozenSelectionAdmissionFault(
				"canonicalize selected directory locator", err, false,
			)
		}
		if previous, exists := aliases[key]; exists && previous != directory.Path {
			return frozenSelectionAdmissionFault(
				"validate selected directory locator aliases",
				fmt.Errorf("platform-equivalent output locators %q and %q", previous, directory.Path),
				false,
			)
		}
		if err := platform.ValidateModifiedTime(directory.ModifiedTime); err != nil {
			return frozenSelectionAdmissionFault(
				"validate selected directory modified time", err, false,
			)
		}
		aliases[key] = directory.Path
		admission.dirs[directory.Path] = directory
		return nil
	}); err != nil {
		return outputSelectionAdmission{}, err
	}
	if err := selection.VisitFiles(func(file transfer.OutputSelectionFile) error {
		key, err := platform.CanonicalLocatorKey(file.Path)
		if err != nil {
			return frozenSelectionAdmissionFault(
				"canonicalize selected file locator", err, false,
			)
		}
		if previous, exists := aliases[key]; exists && previous != file.Path {
			return frozenSelectionAdmissionFault(
				"validate selected file locator aliases",
				fmt.Errorf("platform-equivalent output locators %q and %q", previous, file.Path),
				false,
			)
		}
		if err := platform.ValidateModifiedTime(file.ModifiedTime); err != nil {
			return frozenSelectionAdmissionFault(
				"validate selected file modified time", err, false,
			)
		}
		aliases[key] = file.Path
		admission.files[file.Path] = file
		return nil
	}); err != nil {
		return outputSelectionAdmission{}, err
	}
	return admission, nil
}

// materializeOutputSelection runs only after both native probes have accepted
// the frozen selection and while the placement guard that will own its ancestry
// snapshot remains live. It may create requested user directories, but it never
// discovers a new static locator, alias, or metadata-shape failure.
func materializeOutputSelection(root outputcap.Directory, selection transfer.OutputSelection) error {
	if err := selection.VisitDirectories(func(selected transfer.OutputSelectionDirectory) error {
		return materializeSelectedOutputDirectory(root, selected)
	}); err != nil {
		return err
	}
	return selection.VisitFiles(func(selected transfer.OutputSelectionFile) error {
		parent, _, err := reopenFinalParent(root, selected.Path)
		if err != nil {
			return frozenSelectionAdmissionFault("open selected file parent", err, false)
		}
		authorityErr := validateOutputCreateAuthority(parent)
		if err := errors.Join(authorityErr, parent.Close()); err != nil {
			return frozenSelectionAdmissionFault("validate selected file parent mutation authority", err, false)
		}
		return nil
	})
}

func materializeSelectedOutputDirectory(
	root outputcap.Directory,
	selected transfer.OutputSelectionDirectory,
) (resultErr error) {
	directory, err := openOutputDirectoryPath(root, selected.Path, true)
	if err != nil {
		return frozenSelectionAdmissionFault("materialize selected directory", err, false)
	}
	defer func() {
		if closeErr := directory.Close(); closeErr != nil {
			resultErr = errors.Join(
				resultErr,
				frozenSelectionAdmissionFault("close materialized selected directory", closeErr, false),
			)
		}
	}()
	if selected.ModifiedTime.Present() {
		if err := validateOutputMetadataAuthority(directory); err != nil {
			return frozenSelectionAdmissionFault(
				"validate selected directory metadata authority", err, false,
			)
		}
	}
	if err := directory.Sync(); err != nil {
		return frozenSelectionAdmissionFault("sync selected directory", err, false)
	}
	if err := exactReopenMaterializedOutputDirectory(root, selected.Path, directory); err != nil {
		return frozenSelectionAdmissionFault("exact-reopen selected directory", err, false)
	}
	return nil
}

func exactReopenMaterializedOutputDirectory(
	root outputcap.Directory,
	canonical string,
	retained outputcap.Directory,
) (resultErr error) {
	parent, leaf, err := reopenFinalParent(root, canonical)
	if err != nil {
		return err
	}
	defer func() {
		resultErr = errors.Join(resultErr, parent.Close())
	}()
	kind, exact, err := parent.ClassifyExactEntry(leaf)
	if err != nil {
		return err
	}
	if !exact || kind != outputcap.EntryDirectory {
		return errors.Join(
			errOutputAncestryUnsafe,
			errOutputAncestryMismatch,
			fmt.Errorf("materialized output directory %q is absent or has the wrong type", canonical),
		)
	}
	reopened, err := parent.OpenDirectory(leaf, false)
	if err != nil {
		if isMissing(err) {
			err = errors.Join(errOutputAncestryUnsafe, errOutputAncestryMismatch, err)
		}
		return errors.Join(err, closeOutputV3Directory(reopened))
	}
	defer func() {
		resultErr = errors.Join(resultErr, reopened.Close())
	}()
	same, err := retained.SameDirectory(reopened)
	if err != nil {
		return err
	}
	if !same {
		return errors.Join(
			errOutputAncestryUnsafe,
			errOutputAncestryMismatch,
			fmt.Errorf("materialized output directory %q was replaced", canonical),
		)
	}
	return nil
}

// Resolve every currently existing selected parent before the recoverability
// probe mutates its private scratch namespace. Besides narrowing race windows,
// this lets Windows reject a DOS alias of the reserved control directory using
// the actual long leaf behind an open handle.
func preflightOutputSelectionParents(platform outputcap.Platform, selection transfer.OutputSelection) error {
	selectionEntries := int(selection.DirectoryCount() + selection.FileCount())
	seenFirstComponents := make(map[string]struct{}, selectionEntries)
	firstComponents := make([]string, 0, selectionEntries)
	observeFirst := func(path string) {
		first, _, _ := strings.Cut(path, "/")
		if _, seen := seenFirstComponents[first]; seen {
			return
		}
		seenFirstComponents[first] = struct{}{}
		firstComponents = append(firstComponents, first)
	}
	if err := selection.VisitDirectories(func(selected transfer.OutputSelectionDirectory) error {
		observeFirst(selected.Path)
		return nil
	}); err != nil {
		return err
	}
	if err := selection.VisitFiles(func(selected transfer.OutputSelectionFile) error {
		observeFirst(selected.Path)
		return nil
	}); err != nil {
		return err
	}
	if batch, ok := platform.Root().(outputcap.PublicEntryNamesValidator); ok {
		if err := batch.ValidatePublicEntryNames(firstComponents); err != nil {
			return err
		}
	} else {
		for _, first := range firstComponents {
			if err := preflightExistingOutputComponent(platform.Root(), first); err != nil {
				return err
			}
		}
	}
	if err := selection.VisitDirectories(func(selected transfer.OutputSelectionDirectory) error {
		return preflightOutputDirectoryPath(platform.Root(), selected.Path)
	}); err != nil {
		return err
	}
	return selection.VisitFiles(func(selected transfer.OutputSelectionFile) error {
		if index := strings.LastIndexByte(selected.Path, '/'); index >= 0 {
			return preflightOutputDirectoryPath(platform.Root(), selected.Path[:index])
		}
		return nil
	})
}

// preflightOutputSelectionAuthorities proves every selected-descendant mutation
// boundary before a native probe creates scratch state. The exact output-root
// authority is checked separately because a root placement failure has a wider
// fault scope. Missing descendants stop at the last fixed parent: materialization
// creates the remainder and repeats these checks to close the admission race.
func preflightOutputSelectionAuthorities(platform outputcap.Platform, selection transfer.OutputSelection) error {
	if err := selection.VisitDirectories(func(selected transfer.OutputSelectionDirectory) error {
		return preflightOutputDirectoryAuthorities(
			platform.Root(), selected.Path, false, selected.ModifiedTime.Present(),
		)
	}); err != nil {
		return err
	}
	return selection.VisitFiles(func(selected transfer.OutputSelectionFile) error {
		parentPath := ""
		if separator := strings.LastIndexByte(selected.Path, '/'); separator >= 0 {
			parentPath = selected.Path[:separator]
		}
		return preflightOutputDirectoryAuthorities(platform.Root(), parentPath, true, false)
	})
}

func preflightOutputDirectoryAuthorities(
	root outputcap.Directory,
	canonical string,
	requireFinalCreate bool,
	requireFinalMetadata bool,
) (resultErr error) {
	if canonical == "" {
		if requireFinalCreate {
			resultErr = errors.Join(resultErr, validateOutputCreateAuthority(root))
		}
		if requireFinalMetadata {
			resultErr = errors.Join(resultErr, validateOutputMetadataAuthority(root))
		}
		return resultErr
	}
	walk, err := walkOutputDirectoryPath(root, canonical, false)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, walk.close()) }()
	if !walk.complete {
		return validateOutputCreateAuthority(walk.directory)
	}
	if requireFinalCreate {
		resultErr = errors.Join(resultErr, validateOutputCreateAuthority(walk.directory))
	}
	if requireFinalMetadata {
		resultErr = errors.Join(resultErr, validateOutputMetadataAuthority(walk.directory))
	}
	return resultErr
}

func preflightExistingOutputComponent(root outputcap.Directory, name string) error {
	return root.ValidatePublicEntryName(name)
}

func preflightOutputDirectoryPath(root outputcap.Directory, canonical string) error {
	if canonical == "" {
		return outputfault.ErrPathEscape
	}
	walk, err := walkOutputDirectoryPath(root, canonical, false)
	if err != nil {
		return err
	}
	return walk.close()
}

func openOutputDirectoryPath(root outputcap.Directory, canonical string, create bool) (outputcap.Directory, error) {
	if canonical == "" {
		return nil, outputfault.ErrPathEscape
	}
	walk, err := walkOutputDirectoryPath(root, canonical, create)
	if err != nil {
		return nil, err
	}
	if !walk.complete {
		return nil, errors.Join(fs.ErrNotExist, walk.close())
	}
	directory := walk.directory
	walk.directory, walk.ownsDirectory = nil, false
	return directory, nil
}

type outputDirectoryPathWalk struct {
	directory     outputcap.Directory
	complete      bool
	ownsDirectory bool
}

func (walk *outputDirectoryPathWalk) close() error {
	if walk == nil || !walk.ownsDirectory || walk.directory == nil {
		return nil
	}
	err := walk.directory.Close()
	walk.directory, walk.ownsDirectory = nil, false
	return err
}

// walkOutputDirectoryPath centralizes the no-follow component transition. The
// returned fixed directory is the final component when complete, or the last
// existing parent when a read-only walk encounters a missing component.
func walkOutputDirectoryPath(
	root outputcap.Directory,
	canonical string,
	create bool,
) (walk outputDirectoryPathWalk, resultErr error) {
	if canonical == "" {
		return outputDirectoryPathWalk{}, outputfault.ErrPathEscape
	}
	walk.directory = root
	for component := range strings.SplitSeq(canonical, "/") {
		if component == "" || component == "." || component == ".." {
			return outputDirectoryPathWalk{}, errors.Join(outputfault.ErrPathEscape, walk.close())
		}
		next, err := walk.directory.OpenDirectory(component, false)
		if errors.Is(err, fs.ErrNotExist) && !create {
			if next != nil {
				_ = next.Close()
			}
			return walk, nil
		}
		if errors.Is(err, fs.ErrNotExist) {
			if authorityErr := validateOutputCreateAuthority(walk.directory); authorityErr != nil {
				err = authorityErr
			} else {
				result, ensureErr := outputnamespace.EnsureDirectory(walk.directory, component, false)
				next, err = result.Directory, ensureErr
			}
		}
		if err != nil {
			if next != nil {
				_ = next.Close()
			}
			return outputDirectoryPathWalk{}, errors.Join(err, walk.close())
		}
		if closeErr := walk.close(); closeErr != nil {
			return outputDirectoryPathWalk{}, errors.Join(closeErr, next.Close())
		}
		walk.directory, walk.ownsDirectory = next, true
	}
	walk.complete = true
	return walk, nil
}

func validateOutputCreateAuthority(directory outputcap.Directory) error {
	validator, ok := directory.(outputcap.CreateAuthorityValidator)
	if !ok {
		return nil
	}
	return validator.ValidateCreateAuthority()
}

func validateOutputMetadataAuthority(directory outputcap.Directory) error {
	validator, ok := directory.(outputcap.MetadataAuthorityValidator)
	if !ok {
		return nil
	}
	return validator.ValidateMetadataAuthority()
}

func reopenFinalParent(root outputcap.Directory, locator string) (outputcap.Directory, string, error) {
	index := strings.LastIndexByte(locator, '/')
	if index < 0 {
		if locator == "" {
			return nil, "", outputfault.ErrPathEscape
		}
		// Reopening the root through the platform keeps the same-volume check in
		// one place even for a root-level file.
		return duplicateOutputRoot(root, locator)
	}
	parent, err := openOutputDirectoryPath(root, locator[:index], false)
	if err != nil {
		return nil, "", err
	}
	name := locator[index+1:]
	if name == "" {
		_ = parent.Close()
		return nil, "", outputfault.ErrPathEscape
	}
	return parent, name, nil
}

func duplicateOutputRoot(root outputcap.Directory, leaf string) (outputcap.Directory, string, error) {
	duplicate, err := root.Duplicate()
	if err != nil {
		return nil, "", err
	}
	same, err := duplicate.SameDirectory(root)
	if err != nil || !same {
		return nil, "", errors.Join(outputcap.ErrUnsafeNamespace, err, duplicate.Close())
	}
	return duplicate, leaf, nil
}
