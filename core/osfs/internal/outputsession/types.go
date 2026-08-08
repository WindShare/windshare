package outputsession

import (
	"context"
	"crypto/sha256"
	"errors"
	"unicode/utf8"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/fault"
)

const (
	MaximumDirectoryClaims        uint64 = 1_000_000
	MaximumNodeClaims             uint64 = catalog.DefaultShareCommittedEntries + 1
	MaximumDirectoryMetadataBytes uint64 = 64 << 20
	MaximumActiveFileClaims       uint64 = 32

	directoryNodeIndexKeyBytes    uint64 = catalog.IdentityBytes
	directoryReceiptIndexKeyBytes uint64 = sha256.Size
)

type ClaimID uint64

type Limits struct {
	DirectoryClaims        uint64
	NodeClaims             uint64
	DirectoryMetadataBytes uint64
	ActiveFileClaims       uint64
}

func DefaultLimits() Limits {
	return Limits{
		DirectoryClaims:        MaximumDirectoryClaims,
		NodeClaims:             MaximumNodeClaims,
		DirectoryMetadataBytes: MaximumDirectoryMetadataBytes,
		ActiveFileClaims:       MaximumActiveFileClaims,
	}
}

func (limits Limits) valid() bool {
	return limits.DirectoryClaims > 0 && limits.DirectoryClaims <= MaximumDirectoryClaims &&
		limits.NodeClaims > 0 && limits.NodeClaims <= MaximumNodeClaims &&
		limits.DirectoryClaims <= limits.NodeClaims &&
		limits.DirectoryMetadataBytes > 0 && limits.DirectoryMetadataBytes <= MaximumDirectoryMetadataBytes &&
		limits.ActiveFileClaims > 0 && limits.ActiveFileClaims <= MaximumActiveFileClaims &&
		limits.ActiveFileClaims <= limits.NodeClaims
}

type MutationCut uint8

const (
	MutationNoChange MutationCut = iota + 1
	MutationStable
	MutationAmbiguous
)

func (cut MutationCut) valid() bool {
	return cut >= MutationNoChange && cut <= MutationAmbiguous
}

type DirectoryDisposition uint8

const (
	DirectoryCallerProvidedRoot DirectoryDisposition = iota + 1
	DirectoryAuthorityCreatedRoot
	DirectoryAuthorityCreatedDescendant
	DirectoryPreexistingDescendant
)

func (disposition DirectoryDisposition) validFor(root bool) bool {
	if root {
		return disposition == DirectoryCallerProvidedRoot || disposition == DirectoryAuthorityCreatedRoot
	}
	return disposition == DirectoryAuthorityCreatedDescendant ||
		disposition == DirectoryPreexistingDescendant
}

// DirectoryClaim is the executor's immutable view of one already-reserved edge.
// ClaimID is process-local correlation, never native or durable authority.
type DirectoryClaim struct {
	id         ClaimID
	directory  transfer.OutputDirectory
	locatorKey string
	parent     ClaimID
}

func (claim DirectoryClaim) ID() ClaimID                         { return claim.id }
func (claim DirectoryClaim) Directory() transfer.OutputDirectory { return claim.directory }
func (claim DirectoryClaim) LocatorKey() string                  { return claim.locatorKey }
func (claim DirectoryClaim) ParentID() ClaimID                   { return claim.parent }
func (claim DirectoryClaim) IsRoot() bool                        { return claim.parent == 0 }

type FileClaim struct {
	id         ClaimID
	file       transfer.OutputFile
	locatorKey string
	parent     ClaimID
}

func (claim FileClaim) ID() ClaimID               { return claim.id }
func (claim FileClaim) File() transfer.OutputFile { return claim.file }
func (claim FileClaim) LocatorKey() string        { return claim.locatorKey }
func (claim FileClaim) ParentID() ClaimID         { return claim.parent }

type DirectoryMaterialization struct {
	Cut         MutationCut
	Disposition DirectoryDisposition
}

type DirectoryFinalizationKind uint8

const (
	DirectoryFinalizationFinalized DirectoryFinalizationKind = iota + 1
	DirectoryFinalizationIsolatedFailure
)

type DirectoryFinalization struct {
	Cut     MutationCut
	Kind    DirectoryFinalizationKind
	Failure fault.Fault
}

func FinalizedDirectory() DirectoryFinalization {
	return DirectoryFinalization{Cut: MutationStable, Kind: DirectoryFinalizationFinalized}
}

func IsolatedDirectory(failure fault.Fault) (DirectoryFinalization, error) {
	code, output := failure.OutputCode()
	if !output || failure.Scope() != fault.ScopeDirectoryLocal || code != fault.OutputDirectoryMetadata {
		return DirectoryFinalization{}, ErrExecutorContract
	}
	return DirectoryFinalization{
		Cut: MutationStable, Kind: DirectoryFinalizationIsolatedFailure, Failure: failure,
	}, nil
}

type FileBeginObservation struct {
	Cut         MutationCut
	Transaction FileTransactionExecutor
	Durable     transfer.VerifiedDurableRanges
	Settlement  transfer.FileSettlement
}

type FileTransactionExecutor interface {
	Binding() transfer.OutputFileBinding
	WriteRange(context.Context, uint64, []byte) (MutationCut, error)
	Checkpoint(context.Context) (transfer.VerifiedDurableRanges, MutationCut, error)
	Commit(context.Context) (transfer.FileSettlement, MutationCut, error)
	Pause(context.Context, transfer.FilePauseReason) (transfer.FileSettlement, MutationCut, error)
	Retire(context.Context, transfer.FileRetireReason) (transfer.FileSettlement, MutationCut, error)
}

type LocatorCanonicalizer interface {
	// CanonicalLocatorKey is pure and must not inspect or mutate an external
	// namespace. That lets the session reserve the full key and byte charge in
	// one locked transition before executor I/O.
	CanonicalLocatorKey(string) (string, error)
}

type DirectoryExecutor interface {
	MaterializeDirectory(context.Context, DirectoryClaim) (DirectoryMaterialization, error)
	FinalizeDirectory(context.Context, DirectoryClaim) (DirectoryFinalization, error)
}

type FileExecutor interface {
	BeginFile(context.Context, FileClaim) (FileBeginObservation, error)
}

type ResourceReleaser interface {
	ReleaseOutputSession(context.Context) error
}

type ResourceReleaserFunc func(context.Context) error

func (function ResourceReleaserFunc) ReleaseOutputSession(ctx context.Context) error {
	if function == nil {
		return ErrInvalidConfiguration
	}
	return function(ctx)
}

type Config struct {
	Intent        transfer.TransferIntent
	SessionID     transfer.OutputSessionID
	Capabilities  transfer.OutputCapabilities
	ReceiptSecret []byte
	Limits        Limits
	Locator       LocatorCanonicalizer
	Directories   DirectoryExecutor
	Files         FileExecutor
	Resources     ResourceReleaser
	Trace         TraceSink
}

func (config Config) validate() (transfer.DirectoryAdmissionScope, Limits, error) {
	scope, err := transfer.NewDirectoryAdmissionScope(config.Intent)
	if err != nil || config.SessionID.IsZero() || config.Intent.BackendID() == "" ||
		len(config.ReceiptSecret) != sha256.Size || allZero(config.ReceiptSecret) ||
		config.Locator == nil || config.Directories == nil || config.Files == nil || config.Resources == nil {
		return transfer.DirectoryAdmissionScope{}, Limits{}, errors.Join(ErrInvalidConfiguration, err)
	}
	capabilities, err := transfer.NewOutputCapabilities(config.Capabilities)
	if err != nil || capabilities.Mode != config.Intent.Format() {
		return transfer.DirectoryAdmissionScope{}, Limits{}, errors.Join(ErrInvalidConfiguration, err)
	}
	limits := config.Limits
	if limits == (Limits{}) {
		limits = DefaultLimits()
	}
	if !limits.valid() {
		return transfer.DirectoryAdmissionScope{}, Limits{}, ErrInvalidConfiguration
	}
	return scope, limits, nil
}

func allZero(value []byte) bool {
	var combined byte
	for _, item := range value {
		combined |= item
	}
	return combined == 0
}

func validCanonicalLocatorKey(path, key string) bool {
	return utf8.ValidString(key) && (path == "" || key != "")
}

func directoryBaseMetadataBytes(directory transfer.OutputDirectory) uint64 {
	// This is a stable accounting contract over bytes the Go ledger retains, not
	// a heap estimate. The claim charges its raw DirectoryID and generation, the
	// NodeID index charges its distinct fixed key copy, and the future receipt
	// index key is reserved before materialization. Claim-count limits separately
	// bound struct/map overhead and fixed ModifiedTime copies.
	bytes := uint64(2*catalog.IdentityBytes) + directoryNodeIndexKeyBytes + directoryReceiptIndexKeyBytes
	bytes += uint64(len(directory.Path))
	if !directory.ParentAdmission.IsZero() {
		bytes += sha256.Size
	}
	return bytes
}

func directoryMetadataBytes(directory transfer.OutputDirectory, locatorKey string) uint64 {
	// Path/name and locator indexes retain immutable string headers over the same
	// backing bytes as their claims, so each UTF-8 byte sequence is charged once.
	return directoryBaseMetadataBytes(directory) + uint64(len(locatorKey))
}
