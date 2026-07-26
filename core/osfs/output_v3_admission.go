package osfs

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"strings"

	"github.com/windshare/windshare/core/osfs/internal/resumestate"
	"github.com/windshare/windshare/core/transfer"
)

type outputSelectionAdmission struct {
	selection  transfer.OutputSelection
	files      map[string]transfer.OutputSelectionFile
	dirs       map[string]transfer.OutputSelectionDirectory
	ancestry   outputAncestrySnapshot
	validation *outputAncestryValidation
	resuming   bool
}

// OpenSelection is the post-discovery authority boundary used by transfer.
// Keeping the output-root policy on this object prevents the transfer job from
// constructing filesystem state before the complete canonical selection exists.
func (authority *FilesystemOutputAuthority) OpenSelection(
	ctx context.Context,
	selection transfer.OutputSelection,
) (transfer.OutputSession, error) {
	session, err := authority.openSelection(ctx, selection)
	if err != nil {
		return nil, err
	}
	return session, nil
}

func (authority *FilesystemOutputAuthority) openSelection(
	ctx context.Context,
	requested transfer.OutputSelection,
) (*filesystemOutputSession, error) {
	if authority == nil || authority.platformFactory == nil || authority.sessionIDs == nil || authority.objectIDs == nil ||
		authority.rootPath == "" {
		return nil, transfer.ErrInvalidOutputBinding
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	canonical := requested.CanonicalSelection()
	selection, err := canonical.BindPlan(requested)
	if err != nil || selection.ResumeIntent().IsZero() ||
		selection.ResumeIntent() != canonical.ResumeIntent() {
		return nil, errors.Join(transfer.ErrInvalidOutputSelection, err)
	}
	platform, err := authority.platformFactory(authority.rootPath, authority.createRoot)
	if err != nil {
		return nil, rootOutputFault("certify output filesystem", err)
	}
	platformOwned := true
	defer func() {
		if platformOwned {
			_ = platform.Close()
		}
	}()
	authority.trace(FilesystemOutputTrace{
		Operation: TraceFilesystemCertified, ResumeIntent: selection.ResumeIntent(),
		Certification: filesystemOutputCertificationFromState(platform.Certification()),
	})
	if err := validateReservedOutputSelection(platform, selection); err != nil {
		return nil, classifyFrozenSelectionFault(platform, selection, err)
	}
	admission, err := preflightOutputSelectionAdmission(platform, selection)
	if err != nil {
		return nil, classifyFrozenSelectionFault(platform, selection, err)
	}
	if err := preflightOutputSelectionParents(platform, selection); err != nil {
		return nil, classifyFrozenSelectionFault(
			platform,
			selection,
			frozenSelectionAdmissionFault("preflight selected output parents", err, false),
		)
	}
	if err := validateOutputCreateAuthority(platform.Root()); err != nil {
		return nil, rootOutputFault("validate output root mutation authority", err)
	}
	if err := preflightOutputSelectionAuthorities(platform, selection); err != nil {
		return nil, classifyFrozenSelectionFault(
			platform,
			selection,
			frozenSelectionAdmissionFault("validate selected output mutation authority", err, false),
		)
	}
	if err := platform.ProbeRecoverableFeatures(); err != nil {
		authority.trace(FilesystemOutputTrace{
			Operation: TraceFeatureProbeCompleted, ResumeIntent: selection.ResumeIntent(),
			Certification: filesystemOutputCertificationFromState(platform.Certification()), Failed: true,
		})
		return nil, rootOutputFault("probe output filesystem", err)
	}
	authority.trace(FilesystemOutputTrace{
		Operation: TraceFeatureProbeCompleted, ResumeIntent: selection.ResumeIntent(),
		Certification: filesystemOutputCertificationFromState(platform.Certification()),
	})
	if err := platform.ValidateSelectionMetadata(selection); err != nil {
		return nil, classifyFrozenSelectionFault(
			platform,
			selection,
			frozenSelectionAdmissionFault("validate selected output metadata representation", err, false),
		)
	}
	resumeIntentPresent, err := matchingOutputResumeIntentExists(platform, selection.ResumeIntent())
	if err != nil {
		return nil, err
	}
	var ancestryValidation *outputAncestryValidation
	admission.resuming = resumeIntentPresent
	ancestryBoundary := outputAncestryAdmissionBoundary(resumeIntentPresent)
	// NTFS exposes persistent Object IDs through CREATE_OR_GET only. Preparing
	// every freshly opened ancestry authority is therefore also the restart read:
	// an existing ID is reused, while a replacement may acquire only invisible
	// Object-ID/USN metadata before the header binding rejects it. A resume never
	// materializes missing user directories or writes WindShare state/content here.
	if resumeIntentPresent {
		ancestryValidation, err = prepareOutputSelectionAncestry(platform, selection)
	} else {
		ancestryValidation, err = prepareFreshOutputSelectionAncestry(platform, selection)
	}
	if err != nil {
		claimCount := 0
		if paths, pathErr := canonicalOutputAncestryPaths(selection); pathErr == nil {
			claimCount = len(paths)
		}
		authority.traceOutputAncestry(
			selection, transfer.OutputSessionID{}, resumestate.LocatorDigest{}, outputAncestrySnapshot{},
			claimCount, ancestryBoundary, outputAncestryTraceDecision(err),
		)
		return nil, outputAncestrySessionFault(
			"capture output ancestry", err, resumeIntentPresent,
		)
	}
	admission.ancestry = ancestryValidation.snapshot
	admission.validation = ancestryValidation
	authority.traceOutputAncestry(
		selection, transfer.OutputSessionID{}, resumestate.LocatorDigest{}, admission.ancestry,
		len(admission.ancestry.entries), ancestryBoundary, FilesystemOutputAncestryPrepared,
	)
	control, _, err := authority.openOrBootstrapControl(platform)
	if err != nil {
		return nil, errors.Join(err, authority.closeOutputAdmissionAncestry(&admission))
	}
	authority.trace(FilesystemOutputTrace{
		Operation: TraceControlBootstrap, ResumeIntent: selection.ResumeIntent(),
		Certification: filesystemOutputCertificationFromState(platform.Certification()),
	})
	session, _, _, err := authority.openOutputSession(ctx, platform, control, admission)
	if err != nil {
		_ = control.Close()
		return nil, errors.Join(err, authority.closeOutputAdmissionAncestry(&admission))
	}
	platformOwned = false
	if err := authority.closeOutputAdmissionAncestry(&admission); err != nil {
		return nil, errors.Join(err, session.closeHandles())
	}
	return session, nil
}

func matchingOutputResumeIntentExists(
	platform outputV3Platform,
	intent transfer.ResumeIntent,
) (present bool, resultErr error) {
	if platform == nil || intent.IsZero() {
		return false, transfer.ErrInvalidOutputSelection
	}
	control, err := openInstalledControl(platform.Root(), platform)
	if isMissing(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer func() {
		if closeErr := control.Close(); closeErr != nil {
			present = false
			resultErr = errors.Join(
				resultErr,
				outputFault(transfer.OutputFaultRoot, transfer.OutputFaultStateIO, closeErr),
			)
		}
	}()
	intentName := resumestate.ResumeNamespaceName(intent)
	kind, err := observeExactOutputEntry(control.sessions, intentName)
	if err != nil {
		return false, intentOutputFault("inspect matching resume-intent namespace", err)
	}
	if kind == outputV3EntryAbsent {
		return false, nil
	}
	if kind != outputV3EntryDirectory {
		return false, intentOutputFault("classify matching resume-intent namespace", errOutputIntentUnsafe)
	}
	directory, err := control.sessions.OpenDirectory(intentName, true)
	if err != nil {
		return false, intentOutputFault("open matching resume-intent namespace", err)
	}
	if err := directory.Close(); err != nil {
		return false, outputFault(transfer.OutputFaultSession, transfer.OutputFaultStateIO, err)
	}
	return true, nil
}

func frozenSelectionAdmissionFault(operation string, cause error, requiresPause bool) error {
	fault := outputFault(
		transfer.OutputFaultSession,
		transfer.OutputFaultNamespaceUnsafe,
		fmt.Errorf("%s: %w", operation, cause),
	)
	if !requiresPause {
		return fault
	}
	return transfer.NewOutputSessionError(fault, true)
}

func classifyFrozenSelectionFault(
	platform outputV3Platform,
	selection transfer.OutputSelection,
	fault error,
) error {
	if fault == nil || selection.ResumeIntent().IsZero() || errors.Is(fault, transfer.ErrInvalidOutputSelection) {
		return fault
	}
	resumeIntentPresent, stateErr := matchingOutputResumeIntentExists(platform, selection.ResumeIntent())
	if stateErr != nil {
		// A failed observation cannot prove this is a fresh selection. Preserve the
		// typed state/root fault and require the caller to leave any durable intent
		// untouched until its lifecycle can be established safely.
		return transfer.NewOutputSessionError(
			errors.Join(stateErr, fault),
			true,
		)
	}
	// This exact-entry observation is the lifecycle linearization point. A later
	// concurrent creator owns its own admission; it does not retroactively turn
	// this already-fresh rejection into a paused durable session.
	if !resumeIntentPresent {
		return fault
	}
	return transfer.NewOutputSessionError(fault, true)
}

func (authority *FilesystemOutputAuthority) closeOutputAdmissionAncestry(
	admission *outputSelectionAdmission,
) error {
	if admission == nil || admission.validation == nil {
		return nil
	}
	err := admission.validation.Close()
	admission.validation = nil
	if err == nil {
		return nil
	}
	authority.traceOutputAncestry(
		admission.selection, transfer.OutputSessionID{}, resumestate.LocatorDigest{}, admission.ancestry,
		len(admission.ancestry.entries), outputAncestryAdmissionBoundary(admission.resuming),
		outputAncestryTraceDecision(errors.Join(errOutputAncestryUnsafe, err)),
	)
	return outputAncestryPauseFault("close output ancestry admission guard", err)
}

var reservedOutputRootPrefixes = []string{
	".windshare-output",
	".wsresume-output",
}

func validateReservedOutputSelection(platform outputV3Platform, selection transfer.OutputSelection) error {
	reservedKeys := make([]string, len(reservedOutputRootPrefixes))
	for index, reserved := range reservedOutputRootPrefixes {
		key, err := platform.CanonicalComponentKey(reserved)
		if err != nil {
			return frozenSelectionAdmissionFault("canonicalize reserved output namespace", err, false)
		}
		reservedKeys[index] = key
	}
	validate := func(path string) error {
		first := path
		if separator := strings.IndexByte(path, '/'); separator >= 0 {
			first = path[:separator]
		}
		key, err := platform.CanonicalComponentKey(first)
		if err != nil {
			return err
		}
		// Both operands must pass through the same filesystem key function. NTFS
		// keys use its ordinal upcase table, while ext4 keys preserve bytes; mixing a
		// lowercase constant with an NTFS key would silently admit the control tree.
		for _, reservedKey := range reservedKeys {
			if strings.HasPrefix(key, reservedKey) {
				return errReservedOutputPath
			}
		}
		return nil
	}
	for _, directory := range selection.Directories() {
		if err := validate(directory.Path); err != nil {
			return frozenSelectionAdmissionFault("validate selected directory reservation", err, false)
		}
	}
	for _, file := range selection.Files() {
		if err := validate(file.Path); err != nil {
			return frozenSelectionAdmissionFault("validate selected file reservation", err, false)
		}
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
	platform outputV3Platform,
	selection transfer.OutputSelection,
) (outputSelectionAdmission, error) {
	if selection.Identity().IsZero() || selection.ResumeIntent().IsZero() {
		return outputSelectionAdmission{}, transfer.ErrInvalidOutputSelection
	}
	admission := outputSelectionAdmission{
		selection: selection,
		files:     make(map[string]transfer.OutputSelectionFile, len(selection.Files())),
		dirs:      make(map[string]transfer.OutputSelectionDirectory, len(selection.Directories())),
	}
	aliases := make(map[string]string, len(selection.Files())+len(selection.Directories()))
	for _, directory := range selection.Directories() {
		key, err := platform.CanonicalLocatorKey(directory.Path)
		if err != nil {
			return outputSelectionAdmission{}, frozenSelectionAdmissionFault(
				"canonicalize selected directory locator", err, false,
			)
		}
		if previous, exists := aliases[key]; exists && previous != directory.Path {
			return outputSelectionAdmission{}, frozenSelectionAdmissionFault(
				"validate selected directory locator aliases",
				fmt.Errorf("platform-equivalent output locators %q and %q", previous, directory.Path),
				false,
			)
		}
		if err := platform.ValidateModifiedTime(directory.ModifiedTime); err != nil {
			return outputSelectionAdmission{}, frozenSelectionAdmissionFault(
				"validate selected directory modified time", err, false,
			)
		}
		aliases[key] = directory.Path
		admission.dirs[directory.Path] = directory
	}
	for _, file := range selection.Files() {
		key, err := platform.CanonicalLocatorKey(file.Path)
		if err != nil {
			return outputSelectionAdmission{}, frozenSelectionAdmissionFault(
				"canonicalize selected file locator", err, false,
			)
		}
		if previous, exists := aliases[key]; exists && previous != file.Path {
			return outputSelectionAdmission{}, frozenSelectionAdmissionFault(
				"validate selected file locator aliases",
				fmt.Errorf("platform-equivalent output locators %q and %q", previous, file.Path),
				false,
			)
		}
		if err := platform.ValidateModifiedTime(file.ModifiedTime); err != nil {
			return outputSelectionAdmission{}, frozenSelectionAdmissionFault(
				"validate selected file modified time", err, false,
			)
		}
		aliases[key] = file.Path
		admission.files[file.Path] = file
	}
	return admission, nil
}

// materializeOutputSelection runs only after both native probes have accepted
// the frozen selection and while the placement guard that will own its ancestry
// snapshot remains live. It may create requested user directories, but it never
// discovers a new static locator, alias, or metadata-shape failure.
func materializeOutputSelection(root outputV3Directory, selection transfer.OutputSelection) error {
	for _, selected := range selection.Directories() {
		if err := materializeSelectedOutputDirectory(root, selected); err != nil {
			return err
		}
	}
	for _, selected := range selection.Files() {
		parent, _, err := reopenFinalParent(root, selected.Path)
		if err != nil {
			return frozenSelectionAdmissionFault("open selected file parent", err, false)
		}
		authorityErr := validateOutputCreateAuthority(parent)
		if err := errors.Join(authorityErr, parent.Close()); err != nil {
			return frozenSelectionAdmissionFault("validate selected file parent mutation authority", err, false)
		}
	}
	return nil
}

func materializeSelectedOutputDirectory(
	root outputV3Directory,
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
	root outputV3Directory,
	canonical string,
	retained outputV3Directory,
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
	if !exact || kind != outputV3EntryDirectory {
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
func preflightOutputSelectionParents(platform outputV3Platform, selection transfer.OutputSelection) error {
	selectionEntries := len(selection.Directories()) + len(selection.Files())
	seenFirstComponents := make(map[string]struct{}, selectionEntries)
	firstComponents := make([]string, 0, selectionEntries)
	observeFirst := func(path string) {
		first := path
		if index := strings.IndexByte(path, '/'); index >= 0 {
			first = path[:index]
		}
		if _, seen := seenFirstComponents[first]; seen {
			return
		}
		seenFirstComponents[first] = struct{}{}
		firstComponents = append(firstComponents, first)
	}
	for _, selected := range selection.Directories() {
		observeFirst(selected.Path)
	}
	for _, selected := range selection.Files() {
		observeFirst(selected.Path)
	}
	if batch, ok := platform.Root().(outputV3PublicEntryNamesValidator); ok {
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
	for _, selected := range selection.Directories() {
		if err := preflightOutputDirectoryPath(platform.Root(), selected.Path); err != nil {
			return err
		}
	}
	for _, selected := range selection.Files() {
		if index := strings.LastIndexByte(selected.Path, '/'); index >= 0 {
			if err := preflightOutputDirectoryPath(platform.Root(), selected.Path[:index]); err != nil {
				return err
			}
		}
	}
	return nil
}

// preflightOutputSelectionAuthorities proves every selected-descendant mutation
// boundary before a native probe creates scratch state. The exact output-root
// authority is checked separately because a root placement failure has a wider
// fault scope. Missing descendants stop at the last fixed parent: materialization
// creates the remainder and repeats these checks to close the admission race.
func preflightOutputSelectionAuthorities(platform outputV3Platform, selection transfer.OutputSelection) error {
	for _, selected := range selection.Directories() {
		if err := preflightOutputDirectoryAuthorities(
			platform.Root(), selected.Path, false, selected.ModifiedTime.Present(),
		); err != nil {
			return err
		}
	}
	for _, selected := range selection.Files() {
		parentPath := ""
		if separator := strings.LastIndexByte(selected.Path, '/'); separator >= 0 {
			parentPath = selected.Path[:separator]
		}
		if err := preflightOutputDirectoryAuthorities(platform.Root(), parentPath, true, false); err != nil {
			return err
		}
	}
	return nil
}

func preflightOutputDirectoryAuthorities(
	root outputV3Directory,
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

func preflightExistingOutputComponent(root outputV3Directory, name string) error {
	return root.ValidatePublicEntryName(name)
}

func preflightOutputDirectoryPath(root outputV3Directory, canonical string) error {
	if canonical == "" {
		return ErrPathEscape
	}
	walk, err := walkOutputDirectoryPath(root, canonical, false)
	if err != nil {
		return err
	}
	return walk.close()
}

func openOutputDirectoryPath(root outputV3Directory, canonical string, create bool) (outputV3Directory, error) {
	if canonical == "" {
		return nil, ErrPathEscape
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
	directory     outputV3Directory
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
	root outputV3Directory,
	canonical string,
	create bool,
) (walk outputDirectoryPathWalk, resultErr error) {
	if canonical == "" {
		return outputDirectoryPathWalk{}, ErrPathEscape
	}
	walk.directory = root
	for _, component := range strings.Split(canonical, "/") {
		if component == "" || component == "." || component == ".." {
			return outputDirectoryPathWalk{}, errors.Join(ErrPathEscape, walk.close())
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
				next, _, err = ensureOutputDirectory(walk.directory, component, false)
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

func validateOutputCreateAuthority(directory outputV3Directory) error {
	validator, ok := directory.(outputV3CreateAuthorityValidator)
	if !ok {
		return nil
	}
	return validator.ValidateCreateAuthority()
}

func validateOutputMetadataAuthority(directory outputV3Directory) error {
	validator, ok := directory.(outputV3MetadataAuthorityValidator)
	if !ok {
		return nil
	}
	return validator.ValidateMetadataAuthority()
}

func reopenFinalParent(root outputV3Directory, locator string) (outputV3Directory, string, error) {
	index := strings.LastIndexByte(locator, '/')
	if index < 0 {
		if locator == "" {
			return nil, "", ErrPathEscape
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
		return nil, "", ErrPathEscape
	}
	return parent, name, nil
}

func duplicateOutputRoot(root outputV3Directory, leaf string) (outputV3Directory, string, error) {
	duplicate, err := root.Duplicate()
	if err != nil {
		return nil, "", err
	}
	same, err := duplicate.SameDirectory(root)
	if err != nil || !same {
		return nil, "", errors.Join(errOutputV3Unsafe, err, duplicate.Close())
	}
	return duplicate, leaf, nil
}
