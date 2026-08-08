package osfs

import (
	"context"
	"errors"
	"path/filepath"

	"github.com/windshare/windshare/core/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/core/osfs/internal/directoryauthority"
	"github.com/windshare/windshare/core/osfs/internal/resumeauthority"
	"github.com/windshare/windshare/core/transfer"
)

type ResumeStateAttentionReason = resumeauthority.AttentionReason

const (
	ResumeStateAttentionMissingOwnership     = resumeauthority.AttentionMissingOwnership
	ResumeStateAttentionReplacement          = resumeauthority.AttentionReplacement
	ResumeStateAttentionUnknownChildren      = resumeauthority.AttentionUnknownChildren
	ResumeStateAttentionCorruptBinding       = resumeauthority.AttentionCorruptBinding
	ResumeStateAttentionAmbiguousPublication = resumeauthority.AttentionAmbiguousPublication
)

type ResumeStateAttention = resumeauthority.Attention

type ResumeStateListStatus = resumeauthority.ListStatus

const (
	ResumeStateAvailable      = resumeauthority.ListAvailable
	ResumeStateNeedsAttention = resumeauthority.ListNeedsAttention
)

// ResumeStateRef is a live, single-use capability bound to its inventory. Its
// serialization methods intentionally fail, so callers cannot persist an item
// number and later reinterpret it as deletion authority.
type ResumeStateRef = resumeauthority.Reference
type ResumeStateSummary = resumeauthority.Summary
type ResumeStateInventory = resumeauthority.Inventory

type ResumeStateDiscardStatus = resumeauthority.DiscardStatus

const (
	ResumeStateDiscarded             = resumeauthority.Discarded
	ResumeStateAlreadyAbsent         = resumeauthority.AlreadyAbsent
	ResumeStateDiscardNeedsAttention = resumeauthority.DiscardNeedsAttention
)

type ResumeStateDiscardResult = resumeauthority.DiscardResult

var (
	ErrResumeStateBusy               = resumeauthority.ErrBusy
	ErrResumeStateInventoryClosed    = resumeauthority.ErrInventoryClosed
	ErrResumeStateRefConsumed        = resumeauthority.ErrReferenceConsumed
	ErrResumeStateRefNotSerializable = resumeauthority.ErrReferenceNotSerializable
)

// ResumeStateAuthority is intentionally separate from transfer.OutputSession.
// Pause and Complete therefore cannot enumerate or discard recovery state.
type ResumeStateAuthority interface {
	ListResumeState(context.Context) (*ResumeStateInventory, error)
	Discard(context.Context, ResumeStateRef) (ResumeStateDiscardResult, error)
}

// FilesystemResumeStateAuthority opens a fresh certified native root for every
// inventory. The platform lifetime then belongs to that inventory, so an opaque
// reference remains live across preview, confirmation, and Discard without a
// process-global root handle.
type FilesystemResumeStateAuthority struct {
	rootPath string
}

func NewFilesystemResumeStateAuthority(
	root FilesystemResumeRoot,
) (*FilesystemResumeStateAuthority, error) {
	if root.RootPath == "" || !filepath.IsAbs(root.RootPath) {
		return nil, transfer.ErrInvalidOutputBinding
	}
	return &FilesystemResumeStateAuthority{rootPath: filepath.Clean(root.RootPath)}, nil
}

func (authority *FilesystemResumeStateAuthority) ListResumeState(
	ctx context.Context,
) (*ResumeStateInventory, error) {
	if authority == nil || authority.rootPath == "" || ctx == nil {
		return nil, transfer.ErrInvalidOutputBinding
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	platform, err := openNativeOutputPlatform(authority.rootPath, false)
	if err != nil {
		return nil, err
	}
	platformOwned := true
	defer func() {
		if platformOwned {
			_ = platform.Close()
		}
	}()
	if platform.Root() == nil || platform.Durability() != transfer.DurabilityProcessRestart {
		return nil, transfer.ErrInvalidOutputBinding
	}
	rootBinding, err := platform.RootBinding()
	if err != nil || rootBinding.IsZero() {
		return nil, errors.Join(transfer.ErrInvalidOutputBinding, err)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	certification, err := checkpointmodel.NewCertificationID(string(platform.Certification()))
	if err != nil {
		return nil, errors.Join(transfer.ErrInvalidOutputBinding, err)
	}
	store, err := resumeauthority.NewNativeResumeRepository(resumeauthority.NativeResumeConfig{
		Root:          platform.Root(),
		BackendID:     transfer.NativeFilesystemOutputBackendID,
		Certification: certification,
		RootIdentity:  rootBinding.Bytes(),
	})
	if err != nil {
		return nil, err
	}
	publications, err := directoryauthority.NewPublicationObserver(platform)
	if err != nil {
		return nil, err
	}
	native, err := resumeauthority.NewNativeRepository(store, publications, platform.Close)
	if err != nil {
		return nil, err
	}
	inventory, err := resumeauthority.List(ctx, native)
	if err != nil {
		return nil, err
	}
	platformOwned = false
	return inventory, nil
}

func (authority *FilesystemResumeStateAuthority) Discard(
	ctx context.Context,
	reference ResumeStateRef,
) (ResumeStateDiscardResult, error) {
	if authority == nil || authority.rootPath == "" {
		return ResumeStateDiscardResult{}, transfer.ErrInvalidOutputBinding
	}
	return resumeauthority.Discard(ctx, reference)
}

var _ ResumeStateAuthority = (*FilesystemResumeStateAuthority)(nil)
