package osfs

import (
	"bytes"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/windshare/windshare/core/osfs/internal/resumestate"
	"github.com/windshare/windshare/core/transfer"
)

var (
	errOutputAncestryUnsafe          = errors.New("osfs: output ancestry is unsafe")
	errOutputAncestryMismatch        = errors.New("osfs: output ancestry identity mismatch")
	errOutputAncestryAuthorityDenied = errors.New("osfs: output ancestry authority denied")
)

func outputAncestryPauseFault(operation string, cause error) error {
	if cause == nil {
		return nil
	}
	return outputAncestrySessionFault(operation, cause, true)
}

// outputAncestryOperationFault preserves the distinction between a positive
// placement contradiction and a temporary inability to prove the placement.
// Only the former revokes namespace authority; operational failures pause the
// matching session without manufacturing unsafe-namespace evidence.
func outputAncestryOperationFault(operation string, cause error) error {
	if cause == nil {
		return nil
	}
	if errors.Is(cause, errOutputAncestryUnsafe) ||
		(errors.Is(cause, errOutputV3Unsafe) && !errors.Is(cause, errOutputAncestryAuthorityDenied)) {
		return outputAncestryPauseFault(operation, cause)
	}
	return transfer.NewOutputSessionError(
		outputFault(
			transfer.OutputFaultSession,
			transfer.OutputFaultStateIO,
			fmt.Errorf("%s: %w", operation, cause),
		),
		true,
	)
}

func outputAncestrySessionFault(operation string, cause error, requiresPause bool) error {
	fault := outputFault(
		transfer.OutputFaultSession,
		transfer.OutputFaultNamespaceUnsafe,
		errors.Join(errOutputIntentUnsafe, fmt.Errorf("%s: %w", operation, cause)),
	)
	if !requiresPause {
		return fault
	}
	return transfer.NewOutputSessionError(fault, true)
}

func outputAncestryCleanupFault(operation string, cause error) error {
	if cause == nil {
		return nil
	}
	return transfer.NewOutputSessionError(
		outputFault(
			transfer.OutputFaultSession,
			transfer.OutputFaultStateIO,
			fmt.Errorf("%s: %w", operation, cause),
		),
		true,
	)
}

type outputAncestryAuthority uint8

const (
	outputAncestryNoAuthority outputAncestryAuthority = iota
	outputAncestryCreateAuthority
	outputAncestryMetadataAuthority
)

type outputAncestryRequirement struct {
	path      string
	authority outputAncestryAuthority
}

type outputAncestryEntry struct {
	path  string
	claim []byte
}

type outputAncestrySnapshot struct {
	binding resumestate.OutputAncestryBinding
	entries []outputAncestryEntry
}

func outputAncestryTraceDecision(err error) FilesystemOutputAncestryDecision {
	switch {
	case err == nil:
		return FilesystemOutputAncestryMatched
	case errors.Is(err, errOutputAncestryMismatch):
		return FilesystemOutputAncestryMismatch
	case errors.Is(err, errOutputAncestryAuthorityDenied):
		return FilesystemOutputAncestryAuthorityDenied
	case errors.Is(err, errOutputAncestryUnsafe), errors.Is(err, errOutputV3Unsafe):
		return FilesystemOutputAncestryStructuralUnsafe
	default:
		// A raw I/O failure proves only that the current operation could not
		// establish authority. It is not positive structural evidence.
		return FilesystemOutputAncestryAuthorityDenied
	}
}

func outputAncestryAdmissionBoundary(resuming bool) FilesystemOutputAncestryBoundary {
	if resuming {
		return FilesystemOutputAncestryRestart
	}
	return FilesystemOutputAncestryAdmission
}

func (authority *FilesystemOutputAuthority) traceOutputAncestry(
	selection transfer.OutputSelection,
	sessionID transfer.OutputSessionID,
	locator resumestate.LocatorDigest,
	snapshot outputAncestrySnapshot,
	claimCount int,
	boundary FilesystemOutputAncestryBoundary,
	decision FilesystemOutputAncestryDecision,
) {
	if claimCount < 0 {
		claimCount = 0
	}
	authority.trace(FilesystemOutputTrace{
		Operation: TraceAncestryValidation, ResumeIntent: selection.ResumeIntent(),
		SessionID: sessionID, LocatorDigest: outputLocatorDigestFromState(locator), SelectionIdentity: selection.Identity(),
		OutputAncestryDigest: filesystemOutputAncestryDigestFromState(snapshot.binding), AncestryBoundary: boundary,
		AncestryDecision: decision, AncestryClaimCount: uint32(claimCount),
		Failed: decision != FilesystemOutputAncestryPrepared && decision != FilesystemOutputAncestryMatched,
	})
}

func (session *filesystemOutputSession) traceOutputAncestry(
	boundary FilesystemOutputAncestryBoundary,
	locator resumestate.LocatorDigest,
	err error,
) {
	if session == nil {
		return
	}
	session.owner.traceOutputAncestry(
		session.selection, session.sessionID, locator, session.ancestry,
		len(session.ancestry.entries), boundary, outputAncestryTraceDecision(err),
	)
}

func (snapshot outputAncestrySnapshot) claim(path string) ([]byte, bool) {
	index, found := slices.BinarySearchFunc(snapshot.entries, path, func(entry outputAncestryEntry, target string) int {
		return strings.Compare(entry.path, target)
	})
	if !found {
		return nil, false
	}
	return append([]byte(nil), snapshot.entries[index].claim...), true
}

func (snapshot outputAncestrySnapshot) matches(expected outputAncestrySnapshot) bool {
	if snapshot.binding != expected.binding || len(snapshot.entries) != len(expected.entries) {
		return false
	}
	for index := range snapshot.entries {
		if snapshot.entries[index].path != expected.entries[index].path ||
			!bytes.Equal(snapshot.entries[index].claim, expected.entries[index].claim) {
			return false
		}
	}
	return true
}

type outputAncestryValidation struct {
	platform    outputV3Platform
	selection   transfer.OutputSelection
	snapshot    outputAncestrySnapshot
	guard       outputV3PublicOperationGuard
	directories map[string]outputV3Directory
	openedPaths []string
}

func (validation *outputAncestryValidation) directory(path string) (outputV3Directory, error) {
	if validation == nil || validation.guard == nil {
		return nil, errors.Join(errOutputAncestryUnsafe, errors.New("output ancestry validation is absent"))
	}
	directory, found := validation.directories[path]
	if !found || directory == nil {
		return nil, errors.Join(errOutputAncestryUnsafe, fmt.Errorf("output ancestry path %q is not retained", path))
	}
	return directory, nil
}

func (validation *outputAncestryValidation) revalidateRetainedDirectory(
	path string,
	authority outputAncestryAuthority,
) error {
	directory, err := validation.directory(path)
	if err != nil {
		return err
	}
	switch authority {
	case outputAncestryNoAuthority:
	case outputAncestryCreateAuthority:
		err = validateOutputCreateAuthority(directory)
	case outputAncestryMetadataAuthority:
		err = validateOutputMetadataAuthority(directory)
	default:
		err = resumestate.ErrInvalidState
	}
	if err != nil {
		return errors.Join(errOutputAncestryAuthorityDenied, err)
	}
	current, err := directory.IdentityClaim()
	if err != nil {
		return classifyOutputAncestryEvidence(err)
	}
	expected, found := validation.snapshot.claim(path)
	if !found || !bytes.Equal(current, expected) {
		return errors.Join(
			errOutputAncestryUnsafe, errOutputAncestryMismatch,
			fmt.Errorf("retained output ancestry path %q changed", path),
		)
	}
	return nil
}

// Revalidate repeats the exact path walk beneath the retained native guard.
// This catches a post-operation placement change without weakening the guard's
// cleanup failure into authority for quarantine or repair.
func (validation *outputAncestryValidation) Revalidate(requirement outputAncestryRequirement) error {
	if validation == nil || validation.guard == nil {
		return errors.Join(errOutputAncestryUnsafe, errors.New("output ancestry validation is absent"))
	}
	rootBinding, err := validation.platform.RootBinding()
	if err != nil {
		return classifyOutputAncestryEvidence(err)
	}
	current, directories, opened, err := collectOutputAncestry(
		validation.guard.Root(), rootBinding, validation.selection, requirement,
	)
	closeErr := closeOutputAncestryDirectories(directories, opened)
	if err != nil {
		return errors.Join(classifyOutputAncestryEvidence(err), closeErr)
	}
	if closeErr != nil {
		return closeErr
	}
	if !current.matches(validation.snapshot) {
		return errors.Join(
			errOutputAncestryUnsafe, errOutputAncestryMismatch,
			errors.New("output ancestry changed during operation"),
		)
	}
	return nil
}

func (validation *outputAncestryValidation) Close() error {
	if validation == nil {
		return nil
	}
	closeErr := closeOutputAncestryDirectories(validation.directories, validation.openedPaths)
	validation.directories = nil
	validation.openedPaths = nil
	if validation.guard != nil {
		closeErr = errors.Join(closeErr, validation.guard.Close())
		validation.guard = nil
	}
	return closeErr
}

func prepareOutputSelectionAncestry(
	platform outputV3Platform,
	selection transfer.OutputSelection,
) (*outputAncestryValidation, error) {
	return captureOutputSelectionAncestry(platform, selection, outputAncestryRequirement{})
}

func prepareFreshOutputSelectionAncestry(
	platform outputV3Platform,
	selection transfer.OutputSelection,
) (*outputAncestryValidation, error) {
	return captureOutputSelectionAncestryWithGuardedPreparation(
		platform,
		selection,
		outputAncestryRequirement{},
		func(root outputV3Directory) error {
			return materializeOutputSelection(root, selection)
		},
	)
}

func validateOutputSelectionAncestry(
	platform outputV3Platform,
	selection transfer.OutputSelection,
	expected outputAncestrySnapshot,
	requirement outputAncestryRequirement,
) (*outputAncestryValidation, error) {
	validation, err := captureOutputSelectionAncestry(platform, selection, requirement)
	if err != nil {
		return nil, err
	}
	if !validation.snapshot.matches(expected) {
		return nil, errors.Join(
			errOutputAncestryUnsafe, errOutputAncestryMismatch,
			errors.New("current output ancestry differs from admitted ancestry"),
			validation.Close(),
		)
	}
	return validation, nil
}

func (session *filesystemOutputSession) validateOutputAncestry(
	requirement outputAncestryRequirement,
) (*outputAncestryValidation, error) {
	if session == nil || session.platform == nil || session.ancestry.binding.IsZero() {
		return nil, errors.Join(errOutputAncestryUnsafe, transfer.ErrInvalidOutputBinding)
	}
	if session.stateSnapshot().Header().OutputAncestry() != session.ancestry.binding {
		return nil, errors.Join(
			errOutputAncestryUnsafe, errOutputAncestryMismatch,
			errors.New("session ancestry authority changed"),
		)
	}
	return validateOutputSelectionAncestry(
		session.platform, session.selection, session.ancestry, requirement,
	)
}

func closeOutputAncestryValidation(validation *outputAncestryValidation) error {
	if validation == nil {
		return nil
	}
	return validation.Close()
}

func outputLocatorParentPath(locator string) string {
	separator := strings.LastIndexByte(locator, '/')
	if separator < 0 {
		return ""
	}
	return locator[:separator]
}

func outputLocatorParentAndLeaf(locator string) (string, string, error) {
	separator := strings.LastIndexByte(locator, '/')
	if separator < 0 {
		if locator == "" {
			return "", "", ErrPathEscape
		}
		return "", locator, nil
	}
	if separator == len(locator)-1 {
		return "", "", ErrPathEscape
	}
	return locator[:separator], locator[separator+1:], nil
}

func captureOutputSelectionAncestry(
	platform outputV3Platform,
	selection transfer.OutputSelection,
	requirement outputAncestryRequirement,
) (*outputAncestryValidation, error) {
	return captureOutputSelectionAncestryWithGuardedPreparation(platform, selection, requirement, nil)
}

func captureOutputSelectionAncestryWithGuardedPreparation(
	platform outputV3Platform,
	selection transfer.OutputSelection,
	requirement outputAncestryRequirement,
	prepare func(outputV3Directory) error,
) (*outputAncestryValidation, error) {
	if platform == nil || selection.Identity().IsZero() {
		return nil, errors.Join(errOutputAncestryUnsafe, transfer.ErrInvalidOutputSelection)
	}
	guard, err := acquireOutputPublicOperationGuard(platform)
	if err != nil {
		return nil, classifyOutputAncestryEvidence(err)
	}
	fail := func(cause error) (*outputAncestryValidation, error) {
		return nil, errors.Join(classifyOutputAncestryEvidence(cause), guard.Close())
	}
	guardRoot := guard.Root()
	if guardRoot == nil {
		return fail(errors.Join(errOutputAncestryUnsafe, errors.New("public operation guard returned no root")))
	}
	sameRoot, compareErr := guardRoot.SameDirectory(platform.Root())
	if compareErr != nil {
		return fail(compareErr)
	}
	if !sameRoot {
		return fail(errors.Join(
			errOutputAncestryUnsafe,
			errOutputAncestryMismatch,
			errors.New("public operation guard root differs from certified root"),
		))
	}
	rootBinding, err := platform.RootBinding()
	if err != nil {
		return fail(err)
	}
	// Running preparation inside this boundary ensures the directories it creates
	// are the directories claimed by the retained authority, rather than merely
	// sharing a pathname observed before the placement guard existed.
	if prepare != nil {
		if err := prepare(guardRoot); err != nil {
			return fail(err)
		}
	}
	snapshot, directories, opened, err := collectOutputAncestry(
		guardRoot, rootBinding, selection, requirement,
	)
	if err != nil {
		return fail(errors.Join(err, closeOutputAncestryDirectories(directories, opened)))
	}
	return &outputAncestryValidation{
		platform: platform, selection: selection, snapshot: snapshot, guard: guard,
		directories: directories, openedPaths: opened,
	}, nil
}

func collectOutputAncestry(
	root outputV3Directory,
	rootBinding resumestate.OutputRootBinding,
	selection transfer.OutputSelection,
	requirement outputAncestryRequirement,
) (outputAncestrySnapshot, map[string]outputV3Directory, []string, error) {
	paths, err := canonicalOutputAncestryPaths(selection)
	if err != nil {
		return outputAncestrySnapshot{}, nil, nil, err
	}
	directories := make(map[string]outputV3Directory, len(paths))
	opened := make([]string, 0, len(paths)-1)
	claims := make([]resumestate.OutputAncestryIdentityClaim, 0, len(paths))
	entries := make([]outputAncestryEntry, 0, len(paths))
	for _, path := range paths {
		directory := root
		if path != "" {
			directory, err = openOutputDirectoryPath(root, path, false)
			if err != nil {
				if isMissing(err) {
					err = errors.Join(
						errOutputAncestryUnsafe,
						errOutputAncestryMismatch,
						fmt.Errorf("admitted ancestry path %q is absent: %w", path, err),
					)
				} else {
					err = classifyOutputAncestryEvidence(err)
				}
				return outputAncestrySnapshot{}, directories, opened, err
			}
			opened = append(opened, path)
		}
		directories[path] = directory
		if requirement.path == path {
			switch requirement.authority {
			case outputAncestryNoAuthority:
			case outputAncestryCreateAuthority:
				err = validateOutputCreateAuthority(directory)
			case outputAncestryMetadataAuthority:
				err = validateOutputMetadataAuthority(directory)
			default:
				err = resumestate.ErrInvalidState
			}
			if err != nil {
				return outputAncestrySnapshot{}, directories, opened,
					errors.Join(errOutputAncestryAuthorityDenied, err)
			}
		}
		// A newly opened NTFS authority has no trustworthy read-only lookup: File
		// IDs can be reused, so only CREATE_OR_GET can recover the persistent Object
		// ID belonging to this exact pinned handle. Linux preparation is read-only.
		// IdentityClaim is reserved for a later check on this retained handle.
		identity, err := directory.PrepareIdentityClaim()
		if err != nil {
			return outputAncestrySnapshot{}, directories, opened, classifyOutputAncestryEvidence(err)
		}
		identity = append([]byte(nil), identity...)
		claims = append(claims, resumestate.OutputAncestryIdentityClaim{
			CanonicalPath: path, IdentityClaim: identity,
		})
		entries = append(entries, outputAncestryEntry{path: path, claim: identity})
	}
	if requirement.authority != outputAncestryNoAuthority {
		if _, found := directories[requirement.path]; !found {
			return outputAncestrySnapshot{}, directories, opened,
				errors.Join(errOutputAncestryUnsafe, fmt.Errorf("required ancestry path %q is absent", requirement.path))
		}
	}
	binding, err := resumestate.NewOutputAncestryBinding(rootBinding, selection.Identity(), claims)
	if err != nil {
		return outputAncestrySnapshot{}, directories, opened, err
	}
	return outputAncestrySnapshot{binding: binding, entries: entries}, directories, opened, nil
}

func canonicalOutputAncestryPaths(selection transfer.OutputSelection) ([]string, error) {
	if selection.Identity().IsZero() {
		return nil, transfer.ErrInvalidOutputSelection
	}
	paths := map[string]struct{}{"": {}}
	addClosure := func(path string) error {
		for path != "" {
			paths[path] = struct{}{}
			separator := strings.LastIndexByte(path, '/')
			if separator < 0 {
				path = ""
			} else {
				path = path[:separator]
			}
		}
		return nil
	}
	for _, directory := range selection.Directories() {
		if err := addClosure(directory.Path); err != nil {
			return nil, err
		}
	}
	for _, file := range selection.Files() {
		separator := strings.LastIndexByte(file.Path, '/')
		if separator >= 0 {
			if err := addClosure(file.Path[:separator]); err != nil {
				return nil, err
			}
		}
	}
	ordered := make([]string, 0, len(paths))
	for path := range paths {
		ordered = append(ordered, path)
	}
	slices.Sort(ordered)
	return ordered, nil
}

func classifyOutputAncestryEvidence(err error) error {
	if err == nil || errors.Is(err, errOutputAncestryUnsafe) {
		return err
	}
	if errors.Is(err, errOutputV3Unsafe) && !errors.Is(err, errOutputAncestryAuthorityDenied) {
		return errors.Join(errOutputAncestryUnsafe, err)
	}
	return err
}

func closeOutputAncestryDirectories(directories map[string]outputV3Directory, opened []string) error {
	var result error
	for index := len(opened) - 1; index >= 0; index-- {
		path := opened[index]
		if directory := directories[path]; directory != nil {
			result = errors.Join(result, directory.Close())
		}
	}
	return result
}

type borrowedOutputPublicOperationGuard struct {
	root outputV3Directory
}

func (guard *borrowedOutputPublicOperationGuard) Root() outputV3Directory { return guard.root }
func (guard *borrowedOutputPublicOperationGuard) Close() error            { return nil }

func acquireOutputPublicOperationGuard(platform outputV3Platform) (outputV3PublicOperationGuard, error) {
	guard, err := platform.AcquirePublicOperationGuard()
	if err != nil {
		return nil, err
	}
	if guard == nil {
		return nil, errors.New("output platform returned a nil public operation guard")
	}
	return guard, nil
}
