// Package directoryauthority materializes and settles one output directory edge
// at a time while retaining the native authority needed to revalidate later work.
package directoryauthority

import (
	"errors"
	"fmt"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/osfs/internal/outputsession"
	"github.com/windshare/windshare/core/transfer/fault"
)

const (
	// DefaultParentSnapshotEntryLimit matches the catalog's direct-child bound.
	// A destination with more entries cannot be indexed without accepting more
	// namespace work than one frozen directory generation can justify.
	DefaultParentSnapshotEntryLimit = catalog.MaxDirectoryEntries
	// MaximumPlatformAliasesPerEntry reflects the one alternate DOS 8.3 name
	// exposed by the Windows native directory record.
	MaximumPlatformAliasesPerEntry = 1

	reservedControlComponent = ".windshare-output"
	// Catalog paths cannot contain NUL, so the synthetic root can participate in
	// the output-local locator ledger without colliding with a platform locator.
	rootLocatorKey = "\x00windshare/output-root"
)

var (
	ErrInvalidConfiguration      = errors.New("directory authority configuration is invalid")
	ErrAuthorityClosed           = errors.New("directory authority is closed")
	ErrInvalidLocator            = errors.New("directory authority locator is invalid")
	ErrInvalidClaim              = errors.New("directory authority claim is invalid")
	ErrClaimConflict             = errors.New("directory authority claim conflicts with retained authority")
	ErrParentUnavailable         = errors.New("directory authority parent is unavailable")
	ErrParentSnapshotUnavailable = errors.New("directory authority parent snapshot is unavailable")
	ErrPlatformEquivalentLocator = errors.New("directory authority locator has a platform-equivalent collision")
	ErrEntryCollision            = errors.New("directory authority target entry collides with existing output")
	ErrRetainedAuthorityChanged  = errors.New("directory authority retained path changed")
	ErrMetadataReconcile         = errors.New("directory metadata outcome cannot be reconciled")

	// ErrNoMutation marks an error observed before this claim changed the public
	// namespace. The session ledger may roll back its pending reservation.
	ErrNoMutation = errors.New("directory authority made no namespace mutation")
	// ErrMutationAmbiguous marks an operation whose public or durable outcome
	// cannot be proved. The session must retain the claim and pause.
	ErrMutationAmbiguous = errors.New("directory authority mutation is ambiguous")
)

// ClaimID is only a session-local lookup key for retained native state. Catalog
// NodeID ownership remains exclusively in outputsession's global claim ledger.
type ClaimID = outputsession.ClaimID

func validClaimID(id ClaimID) bool { return id != 0 }

// locatorKey carries the path-local details needed after outputsession has
// reserved its comparable canonical key. It is never a second global ledger.
type locatorKey struct {
	authority     *Authority
	canonicalPath string
	canonicalKey  string
	leaf          string
	leafKey       string
}

func (key locatorKey) isRoot() bool { return key.canonicalPath == "" && key.valid() }

func (key locatorKey) valid() bool {
	return key.authority != nil && key.canonicalKey != "" &&
		(key.canonicalPath == "" || key.leaf != "" && key.leafKey != "")
}

// directoryClaim is the filesystem-only projection of outputsession's immutable
// claim. Catalog identity is intentionally not copied into this local authority.
type directoryClaim struct {
	authority *Authority
	id        ClaimID
	parentID  ClaimID
	locator   locatorKey
	modified  catalog.ModifiedTime
}

func (claim directoryClaim) valid() bool {
	if claim.authority == nil || claim.authority != claim.locator.authority ||
		!validClaimID(claim.id) || !claim.locator.valid() {
		return false
	}
	if claim.locator.isRoot() {
		return claim.parentID == 0
	}
	return validClaimID(claim.parentID) && claim.parentID != claim.id
}

func sameDirectoryClaim(left, right directoryClaim) bool {
	return left.authority == right.authority && left.id == right.id && left.parentID == right.parentID &&
		left.locator == right.locator && left.modified == right.modified
}

type DirectoryDisposition = outputsession.DirectoryDisposition

const (
	DirectoryCallerProvidedRoot         = outputsession.DirectoryCallerProvidedRoot
	DirectoryAuthorityCreatedRoot       = outputsession.DirectoryAuthorityCreatedRoot
	DirectoryAuthorityCreatedDescendant = outputsession.DirectoryAuthorityCreatedDescendant
	DirectoryPreexistingDescendant      = outputsession.DirectoryPreexistingDescendant
)

func validDirectoryDisposition(disposition DirectoryDisposition) bool {
	return disposition >= DirectoryCallerProvidedRoot && disposition <= DirectoryPreexistingDescendant
}

func directoryMetadataOwned(disposition DirectoryDisposition) bool {
	return disposition == DirectoryAuthorityCreatedRoot ||
		disposition == DirectoryAuthorityCreatedDescendant
}

// directoryMaterialization contains no native handle or restart authority.
// Those remain retained inside Authority until Close.
type directoryMaterialization struct {
	claimID     ClaimID
	disposition DirectoryDisposition
	reconciled  bool
}

func (result directoryMaterialization) valid() bool {
	return validClaimID(result.claimID) && validDirectoryDisposition(result.disposition)
}

type directoryFinalization struct {
	claimID    ClaimID
	kind       outputsession.DirectoryFinalizationKind
	failure    fault.Fault
	reconciled bool
}

func (result directoryFinalization) valid() bool {
	if !validClaimID(result.claimID) {
		return false
	}
	switch result.kind {
	case outputsession.DirectoryFinalizationFinalized:
		return result.failure.IsZero()
	case outputsession.DirectoryFinalizationIsolatedFailure:
		code, ok := result.failure.OutputCode()
		return ok && result.failure.Scope() == fault.ScopeDirectoryLocal && code == fault.OutputDirectoryMetadata
	default:
		return false
	}
}

// PublicEntryName records every spelling by which one immutable parent snapshot
// can resolve an entry. Aliases are bounded by the native Windows record shape.
type PublicEntryName struct {
	Name    string
	Aliases []string
}

// ParentNamespaceSnapshotter is injected because outputcap currently exposes
// long-name enumeration but not Windows long/short-name pairs. The default uses
// Directory.Names; a platform adapter can provide the richer snapshot without
// widening the shared capability surface during this implementation wave.
type ParentNamespaceSnapshotter interface {
	SnapshotPublicEntryNames(outputcap.Directory, int) ([]PublicEntryName, error)
}

// Platform is the narrow existing outputcap surface used by this consumer.
type Platform interface {
	AcquirePublicOperationGuard() (outputcap.PublicOperationGuard, error)
	RootOpenDisposition() outputcap.RootOpenDisposition
	ValidateModifiedTime(catalog.ModifiedTime) error
	CanonicalLocatorKey(string) (string, error)
	CanonicalComponentKey(string) (string, error)
}

// Config supplies bounded namespace inspection and an optional post-lock trace.
type Config struct {
	ParentSnapshotEntryLimit int
	Snapshotter              ParentNamespaceSnapshotter
	Trace                    func(TraceEvent)
}

// TraceOperation and TraceOutcome are stable structured observability values.
type TraceOperation uint8

const (
	TraceMaterializeDirectory TraceOperation = iota + 1
	TraceFinalizeDirectory
)

type TraceOutcome uint8

const (
	TraceSucceeded TraceOutcome = iota + 1
	TraceIsolatedFailure
	TraceNoMutation
	TraceMutationAmbiguous
)

// TraceEvent intentionally omits canonical paths and locator keys.
type TraceEvent struct {
	Operation   TraceOperation
	Outcome     TraceOutcome
	ClaimID     ClaimID
	ParentID    ClaimID
	Disposition DirectoryDisposition
	Cached      bool
}

func noMutation(cause error) error {
	if cause == nil {
		cause = ErrInvalidClaim
	}
	return errors.Join(ErrNoMutation, cause)
}

func mutationAmbiguous(cause error) error {
	if cause == nil {
		cause = ErrRetainedAuthorityChanged
	}
	return errors.Join(ErrMutationAmbiguous, cause)
}

func mutationCut(err error) outputsession.MutationCut {
	if errors.Is(err, ErrNoMutation) {
		return outputsession.MutationNoChange
	}
	return outputsession.MutationAmbiguous
}

func metadataFault() fault.Fault {
	value, err := fault.NewOutput(fault.ScopeDirectoryLocal, fault.OutputDirectoryMetadata)
	if err != nil {
		panic(fmt.Sprintf("construct directory metadata fault: %v", err))
	}
	return value
}
