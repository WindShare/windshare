package resumeauthority

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io/fs"

	"github.com/windshare/windshare/core/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/core/osfs/internal/checkpointstore"
	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/transfer"
)

// NativeResumeConfig contains independently certified live-root facts. The
// persisted root disposition is intentionally absent: it is recovered only from
// the exact ownership marker and never guessed from current path existence.
type NativeResumeConfig struct {
	Root          outputcap.Directory
	BackendID     transfer.OutputBackendID
	Certification checkpointmodel.CertificationID
	RootIdentity  []byte
}

type NativeResumeRepository struct {
	config NativeResumeConfig
}

func NewNativeResumeRepository(config NativeResumeConfig) (NativeResumeRepository, error) {
	config.RootIdentity = bytes.Clone(config.RootIdentity)
	_, backendErr := transfer.NewOutputBackendID(string(config.BackendID))
	_, certificationErr := checkpointmodel.NewCertificationID(string(config.Certification))
	if config.Root == nil || len(config.RootIdentity) != sha256.Size ||
		backendErr != nil || certificationErr != nil {
		return NativeResumeRepository{}, errors.Join(
			transfer.ErrInvalidOutputBinding, backendErr, certificationErr,
		)
	}
	return NativeResumeRepository{config: config}, nil
}

func (repository NativeResumeRepository) ListResumeState(
	ctx context.Context,
) (PinnedInventory, error) {
	if err := contextErr(ctx); err != nil {
		return nil, err
	}
	ownership, state, err := repository.inspectOwnership()
	if err != nil {
		return nil, projectResumeError("inspect native resume ownership", err)
	}
	if err := contextErr(ctx); err != nil {
		return nil, err
	}
	switch state {
	case nativeResumeNamespaceAbsent:
		return newResumeInventory(checkpointstore.CertifiedConfig{}, nil), nil
	case nativeResumeOwnershipMissing:
		return repository.unavailableInventory(AttentionMissingOwnership), nil
	case nativeResumeOwnershipUnsafe:
		return repository.unavailableInventory(AttentionCorruptBinding), nil
	case nativeResumeOwnershipExact:
		store, err := NewResumeRepository(checkpointstore.CertifiedConfig{
			Root: repository.config.Root, Ownership: ownership,
		})
		if err != nil {
			return nil, err
		}
		return store.ListResumeState(ctx)
	default:
		return nil, projectResumeError("classify native resume ownership", outputcap.ErrUnsafeNamespace)
	}
}

func (repository NativeResumeRepository) unavailableInventory(
	reason AttentionReason,
) *resumeInventory {
	inventory := newResumeInventory(checkpointstore.CertifiedConfig{}, nil)
	attention := resumeAdapterAttention(reason, repository.attentionSource())
	state, _ := NewListedState(ListedStateSpec{
		Status: ListNeedsAttention, Backend: repository.config.BackendID,
		Attention: []Attention{attention},
	})
	inventory.items = []resumeInventoryItem{{state: state}}
	inventory.entries = []ListedState{state}
	return inventory
}

func (repository NativeResumeRepository) attentionSource() []byte {
	source := make([]byte, 0, len(repository.config.BackendID)+len(repository.config.Certification)+
		len(repository.config.RootIdentity)+2)
	source = append(source, repository.config.BackendID...)
	source = append(source, 0)
	source = append(source, repository.config.Certification...)
	source = append(source, 0)
	return append(source, repository.config.RootIdentity...)
}

type nativeResumeOwnershipState uint8

const (
	nativeResumeNamespaceAbsent nativeResumeOwnershipState = iota + 1
	nativeResumeOwnershipMissing
	nativeResumeOwnershipUnsafe
	nativeResumeOwnershipExact
)

func (repository NativeResumeRepository) inspectOwnership() (
	result checkpointmodel.Ownership,
	state nativeResumeOwnershipState,
	resultErr error,
) {
	root, err := repository.config.Root.Duplicate()
	if err != nil || root == nil {
		return checkpointmodel.Ownership{}, 0, errors.Join(err, outputcap.ErrUnsafeNamespace)
	}
	defer func() { resultErr = errors.Join(resultErr, closeDirectory(root)) }()
	controlPin, control, err := pinExistingDirectory(root, checkpointstore.ControlDirectory)
	if errors.Is(err, fs.ErrNotExist) {
		return checkpointmodel.Ownership{}, nativeResumeNamespaceAbsent, nil
	}
	if err != nil {
		if _, attentionState := resumeOpenAttention(err); attentionState {
			return checkpointmodel.Ownership{}, nativeResumeOwnershipUnsafe, nil
		}
		return checkpointmodel.Ownership{}, 0, err
	}
	defer func() {
		resultErr = errors.Join(resultErr, closeDirectory(control), closeEntryReference(controlPin))
	}()
	checkpointPin, checkpointRoot, err := pinExistingDirectory(control, checkpointstore.CheckpointDirectory)
	if errors.Is(err, fs.ErrNotExist) {
		return checkpointmodel.Ownership{}, nativeResumeNamespaceAbsent, nil
	}
	if err != nil {
		if _, attentionState := resumeOpenAttention(err); attentionState {
			return checkpointmodel.Ownership{}, nativeResumeOwnershipUnsafe, nil
		}
		return checkpointmodel.Ownership{}, 0, err
	}
	defer func() {
		resultErr = errors.Join(
			resultErr, closeDirectory(checkpointRoot), closeEntryReference(checkpointPin),
		)
	}()

	kind, exact, err := checkpointRoot.ClassifyExactEntry(checkpointstore.OwnershipFile)
	if err != nil {
		return checkpointmodel.Ownership{}, 0, err
	}
	if kind == outputcap.EntryAbsent {
		return checkpointmodel.Ownership{}, nativeResumeOwnershipMissing, nil
	}
	if !exact || kind != outputcap.EntryRegularFile {
		return checkpointmodel.Ownership{}, nativeResumeOwnershipUnsafe, nil
	}
	pin, err := checkpointRoot.OpenEntry(checkpointstore.OwnershipFile)
	if err != nil || pin == nil || pin.Kind() != outputcap.EntryRegularFile {
		return checkpointmodel.Ownership{}, nativeResumeOwnershipUnsafe,
			errors.Join(err, closeEntryReference(pin))
	}
	defer func() { resultErr = errors.Join(resultErr, closeEntryReference(pin)) }()
	evidence, err := pinnedEntryEvidence(checkpointRoot, checkpointstore.OwnershipFile, pin)
	if err != nil {
		return checkpointmodel.Ownership{}, 0, err
	}
	if evidence != EvidenceExact {
		return checkpointmodel.Ownership{}, nativeResumeOwnershipUnsafe, nil
	}
	encoded, err := readPinnedFileBytes(checkpointRoot, checkpointstore.OwnershipFile, pin)
	if err != nil {
		if _, attentionState := resumeObservationAttention(err); attentionState {
			return checkpointmodel.Ownership{}, nativeResumeOwnershipUnsafe, nil
		}
		return checkpointmodel.Ownership{}, 0, err
	}
	if encoded == nil {
		return checkpointmodel.Ownership{}, nativeResumeOwnershipUnsafe, nil
	}
	ownership, err := checkpointmodel.DecodeOwnership(encoded)
	if err != nil {
		return checkpointmodel.Ownership{}, nativeResumeOwnershipUnsafe, nil
	}
	if ownership.Backend() != repository.config.BackendID ||
		ownership.Certification() != repository.config.Certification ||
		!bytes.Equal(ownership.RootIdentity().Bytes(), repository.config.RootIdentity) {
		return checkpointmodel.Ownership{}, nativeResumeOwnershipUnsafe, nil
	}
	return ownership, nativeResumeOwnershipExact, nil
}

var _ Repository = NativeResumeRepository{}
