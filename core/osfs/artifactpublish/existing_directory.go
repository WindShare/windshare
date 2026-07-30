package artifactpublish

import "github.com/windshare/windshare/core/osfs/internal/outputcap"

const (
	// ExistingDirectoryOutputName is deliberately singular. A successful
	// create-only install is therefore the only name that can authorize upload.
	ExistingDirectoryOutputName = "sealed"
)

// ExistingDirectoryFile binds one already-written regular file to its exact
// portable path, bigint-safe byte length, and content digest.
type ExistingDirectoryFile struct {
	RelativePath string
	ByteLength   uint64
	SHA256       string
}

// ExistingDirectoryInventory is a complete recursive namespace authority.
// Root is implicit; every other directory, including empty directories, must be
// listed explicitly so an unmentioned entry can never cross the seal boundary.
type ExistingDirectoryInventory struct {
	Directories []string
	Files       []ExistingDirectoryFile
}

// ExistingDirectoryStagingRequest creates the private, same-parent directory
// namespace before Node writes any bytes. Native creation is required on
// Windows because chmod cannot establish the hidden ACL authority used by the
// handle-relative publisher.
type ExistingDirectoryStagingRequest struct {
	ParentPath             string
	StagingName            string
	Inventory              ExistingDirectoryInventory
	ManifestPath           string
	ExpectedManifestSHA256 string
}

// ExistingDirectoryStagingReceipt is an opaque restart-revalidatable identity.
// Possessing its bytes does not grant cleanup authority; native reopening and
// exact inventory verification must still match before any removal.
type ExistingDirectoryStagingReceipt struct {
	identity outputcap.PersistentDirectoryIdentity
}

// ExistingDirectoryCleanupOutcome distinguishes a completed cleanup from a
// harmlessly absent staging name and an ambiguous namespace that was untouched.
type ExistingDirectoryCleanupOutcome string

const (
	ExistingDirectoryCleanupAbsent    ExistingDirectoryCleanupOutcome = "absent"
	ExistingDirectoryCleanupCompleted ExistingDirectoryCleanupOutcome = "completed"
	ExistingDirectoryCleanupAmbiguous ExistingDirectoryCleanupOutcome = "ambiguous"
)

type ExistingDirectoryCleanupRequest struct {
	ParentPath             string
	StagingName            string
	Inventory              ExistingDirectoryInventory
	ManifestPath           string
	ExpectedManifestSHA256 string
	Receipt                ExistingDirectoryStagingReceipt
}

// NewExistingDirectoryStagingReceipt reconstructs only the opaque comparison
// claim returned by a prior prepare invocation.
func NewExistingDirectoryStagingReceipt(encoded []byte) ExistingDirectoryStagingReceipt {
	return ExistingDirectoryStagingReceipt{identity: outputcap.NewPersistentDirectoryIdentity(encoded)}
}

func (receipt ExistingDirectoryStagingReceipt) Bytes() []byte {
	return receipt.identity.Bytes()
}

func (receipt ExistingDirectoryStagingReceipt) IsZero() bool {
	return receipt.identity.IsZero()
}

// ExistingDirectoryRequest installs an invocation-owned private sibling that
// its producer has already made quiescent. This package proves only filesystem
// authority; the in-process producer-quiescence witness belongs to the caller.
type ExistingDirectoryRequest struct {
	ParentPath             string
	OutputName             string
	StagingName            string
	Inventory              ExistingDirectoryInventory
	ManifestPath           string
	ExpectedManifestSHA256 string
	SnapshotPaths          []string
	Receipt                ExistingDirectoryStagingReceipt
}

// ExistingDirectoryVerificationRequest authenticates an already-sealed final
// directory without granting mutation authority.
type ExistingDirectoryVerificationRequest struct {
	ParentPath             string
	OutputName             string
	Inventory              ExistingDirectoryInventory
	ManifestPath           string
	ExpectedManifestSHA256 string
	SnapshotPaths          []string
}

// ExistingDirectorySnapshot contains bounded bytes reread from the exact final
// directory during the same recursive verification that authenticates it.
type ExistingDirectorySnapshot struct {
	RelativePath string
	Bytes        []byte
	SHA256       string
}

// ExistingDirectoryResult is returned only after exact final identity,
// inventory, content, and manifest authority have all been revalidated.
type ExistingDirectoryResult struct {
	ManifestSHA256 string
	Snapshots      []ExistingDirectorySnapshot
}

// PublishExistingDirectory atomically installs one complete existing tree
// without replacing any final namespace entry.
func PublishExistingDirectory(request ExistingDirectoryRequest) (ExistingDirectoryResult, error) {
	return publisher{openPrivate: openPrivateNativePlatform}.publishExistingDirectory(request)
}

// PrepareExistingDirectoryStaging creates an exact empty private directory tree
// under the held publication parent. The later publisher accepts only the same
// invocation-shaped staging name and a complete file inventory.
func PrepareExistingDirectoryStaging(
	request ExistingDirectoryStagingRequest,
) (ExistingDirectoryStagingReceipt, error) {
	return publisher{openPrivate: openPrivateNativePlatform}.prepareExistingDirectoryStaging(request)
}

func CleanupExistingDirectoryStaging(
	request ExistingDirectoryCleanupRequest,
) (ExistingDirectoryCleanupOutcome, error) {
	return publisher{openPrivate: openPrivateNativePlatform}.cleanupExistingDirectoryStaging(request)
}

// VerifyExistingDirectory authenticates one previously sealed tree and returns
// only the caller-selected bounded snapshots.
func VerifyExistingDirectory(request ExistingDirectoryVerificationRequest) (ExistingDirectoryResult, error) {
	return publisher{openPrivate: openPrivateNativePlatform}.verifyExistingDirectory(request)
}
