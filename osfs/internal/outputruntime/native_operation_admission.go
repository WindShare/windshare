package outputruntime

import (
	"errors"

	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/receivecontract"
	"github.com/windshare/windshare/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/osfs/internal/checkpointstore"
	"github.com/windshare/windshare/osfs/internal/destinationauthority"
)

type LookupKind uint8

const (
	LookupCreateReady LookupKind = iota + 1
	LookupNotFound
	LookupReopened
	LookupAlreadyRunning
	LookupNeedsAttention
	LookupAmbiguous
)

// ActiveLookup is an opaque continuation. Only LookupCreateReady retains
// admission authority; LookupNotFound cannot create a replacement for an
// explicitly selected ID. A reopened result retains the exact operation lease
// and top-level capability.
type ActiveLookup struct {
	authority *Authority
	kind      LookupKind
	admission *heldAdmission
	operation *Operation
	attention AttentionReason
}

func (lookup ActiveLookup) Kind() LookupKind { return lookup.kind }

func (lookup ActiveLookup) Attention() AttentionReason {
	if lookup.kind != LookupNeedsAttention {
		return AttentionNone
	}
	return lookup.attention
}

func (lookup ActiveLookup) Operation() *Operation {
	if lookup.kind != LookupReopened {
		return nil
	}
	return lookup.operation
}

type heldAdmission struct {
	operationID receivecontract.OperationID
	key         checkpointmodel.ActiveOperationKey
	selection   transfer.SelectionSpec
	durable     *checkpointstore.ActiveAdmission
	operation   receivecontract.OperationID
}

func (admission *heldAdmission) prepare(operation receivecontract.OperationID) error {
	if admission == nil || admission.durable == nil || operation.IsZero() || !admission.operation.IsZero() {
		return transfer.ErrInvalidOutputBinding
	}
	if err := admission.durable.PrepareCandidate(operation); err != nil {
		return err
	}
	admission.operation = operation
	return nil
}

func (admission *heldAdmission) BeginReservation(
	spec destinationauthority.ReservationClaimSpec,
) (destinationauthority.ReservationClaimHandle, destinationauthority.ReservationMetadataClaimOutcome, error) {
	if admission == nil || admission.durable == nil || admission.operation.IsZero() || spec.OperationID != admission.operation {
		return nil, 0, transfer.ErrInvalidOutputBinding
	}
	// A public collision rolls the exact candidate back and prepares it again,
	// allowing the held admission itself to remain the sole name claimer.
	handle, outcome, err := admission.durable.BeginReservation(spec)
	if err != nil || outcome != destinationauthority.ReservationMetadataClaimCommitted || handle == nil {
		return handle, outcome, err
	}
	return &admissionReservationClaimHandle{admission: admission, handle: handle},
		destinationauthority.ReservationMetadataClaimCommitted, nil
}

func (admission *heldAdmission) close() error {
	if admission == nil {
		return nil
	}
	var err error
	if admission.durable != nil {
		err = errors.Join(err, admission.durable.Close())
	}
	admission.durable = nil
	admission.selection = transfer.SelectionSpec{}
	admission.key = checkpointmodel.ActiveOperationKey{}
	admission.operation = receivecontract.OperationID{}
	return err
}

type admissionReservationClaimHandle struct {
	admission *heldAdmission
	handle    destinationauthority.ReservationClaimHandle
}

func (handle *admissionReservationClaimHandle) Claim() destinationauthority.ReservationClaim {
	if handle == nil || handle.handle == nil {
		return destinationauthority.ReservationClaim{}
	}
	return handle.handle.Claim()
}

func (handle *admissionReservationClaimHandle) BindReservation(
	reservation receivecontract.DestinationReservation,
) (destinationauthority.ReservationMetadataClaimOutcome, error) {
	if handle == nil || handle.handle == nil {
		return 0, transfer.ErrInvalidOutputBinding
	}
	return handle.handle.BindReservation(reservation)
}

func (handle *admissionReservationClaimHandle) BindDirectoryIdentity(
	identity []byte,
) (destinationauthority.ReservationMetadataClaimOutcome, error) {
	if handle == nil || handle.handle == nil {
		return 0, transfer.ErrInvalidOutputBinding
	}
	return handle.handle.BindDirectoryIdentity(identity)
}

func (handle *admissionReservationClaimHandle) Rollback() (
	destinationauthority.ReservationMetadataClaimOutcome,
	error,
) {
	if handle == nil || handle.handle == nil || handle.admission == nil ||
		handle.admission.durable == nil || handle.admission.operation.IsZero() {
		return destinationauthority.ReservationMetadataClaimIndeterminate, transfer.ErrInvalidOutputBinding
	}
	outcome, err := handle.handle.Rollback()
	if outcome != destinationauthority.ReservationMetadataClaimCommitted || err != nil {
		return destinationauthority.ReservationMetadataClaimIndeterminate, err
	}
	operation := handle.admission.operation
	if err := handle.admission.durable.RollbackCandidate(); err != nil {
		return destinationauthority.ReservationMetadataClaimIndeterminate, err
	}
	if err := handle.admission.durable.PrepareCandidate(operation); err != nil {
		return destinationauthority.ReservationMetadataClaimIndeterminate, err
	}
	return destinationauthority.ReservationMetadataClaimCommitted, nil
}

func (handle *admissionReservationClaimHandle) Close() error {
	if handle == nil || handle.handle == nil {
		return nil
	}
	err := handle.handle.Close()
	handle.handle = nil
	return err
}
