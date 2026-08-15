package outputruntime

import (
	"bytes"
	"crypto/sha256"
	"sync"

	"github.com/windshare/windshare/core/osfs/internal/destinationauthority"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/receivecontract"
)

type volatileClaimKey struct {
	authority receivecontract.AuthorityRef
	name      string
}

var processVolatileClaims = struct {
	sync.Mutex
	claims map[volatileClaimKey]destinationauthority.ReservationClaimToken
}{claims: make(map[volatileClaimKey]destinationauthority.ReservationClaimToken)}

type volatileReservationClaimer struct {
	authority receivecontract.AuthorityRef
	claims    map[volatileClaimKey]destinationauthority.ReservationClaimToken
	closed    bool
}

func newVolatileReservationClaimer(
	authority receivecontract.AuthorityRef,
) (*volatileReservationClaimer, error) {
	if authority.IsZero() {
		return nil, transfer.ErrInvalidOutputBinding
	}
	return &volatileReservationClaimer{
		authority: authority,
		claims:    make(map[volatileClaimKey]destinationauthority.ReservationClaimToken),
	}, nil
}

func (claimer *volatileReservationClaimer) BeginReservation(
	spec destinationauthority.ReservationClaimSpec,
) (destinationauthority.ReservationClaimHandle, destinationauthority.ReservationMetadataClaimOutcome, error) {
	if claimer == nil || claimer.closed || !spec.Valid() {
		return nil, 0, transfer.ErrInvalidOutputBinding
	}
	key := volatileClaimKey{authority: claimer.authority, name: spec.CanonicalNameKey}
	token := volatileReservationClaimToken(claimer.authority, spec)
	processVolatileClaims.Lock()
	defer processVolatileClaims.Unlock()
	if _, exists := processVolatileClaims.claims[key]; exists {
		return nil, destinationauthority.ReservationMetadataClaimCollision, nil
	}
	processVolatileClaims.claims[key] = token
	claimer.claims[key] = token
	return &volatileReservationClaimHandle{
		owner: claimer, key: key, spec: spec,
		claim: destinationauthority.ReservationClaim{Token: token, Generation: 1},
	}, destinationauthority.ReservationMetadataClaimCommitted, nil
}

func (claimer *volatileReservationClaimer) Close() error {
	if claimer == nil || claimer.closed {
		return nil
	}
	processVolatileClaims.Lock()
	for key, token := range claimer.claims {
		if processVolatileClaims.claims[key] == token {
			delete(processVolatileClaims.claims, key)
		}
	}
	processVolatileClaims.Unlock()
	claimer.claims = nil
	claimer.closed = true
	return nil
}

func volatileReservationClaimToken(
	authority receivecontract.AuthorityRef,
	spec destinationauthority.ReservationClaimSpec,
) destinationauthority.ReservationClaimToken {
	hash := sha256.New()
	_, _ = hash.Write([]byte("windshare/volatile-reservation-claim/v1"))
	_, _ = hash.Write([]byte{0})
	_, _ = hash.Write(authority.Bytes())
	_, _ = hash.Write(spec.OperationID.Bytes())
	_, _ = hash.Write(spec.ReservationID.Bytes())
	_, _ = hash.Write([]byte(spec.CanonicalNameKey))
	var token destinationauthority.ReservationClaimToken
	copy(token[:], hash.Sum(nil))
	return token
}

type volatileReservationClaimHandle struct {
	owner       *volatileReservationClaimer
	key         volatileClaimKey
	spec        destinationauthority.ReservationClaimSpec
	claim       destinationauthority.ReservationClaim
	reservation receivecontract.DestinationReservation
	identity    []byte
	closed      bool
}

func (handle *volatileReservationClaimHandle) Claim() destinationauthority.ReservationClaim {
	if handle == nil || handle.closed {
		return destinationauthority.ReservationClaim{}
	}
	return handle.claim
}

func (handle *volatileReservationClaimHandle) BindReservation(
	reservation receivecontract.DestinationReservation,
) (destinationauthority.ReservationMetadataClaimOutcome, error) {
	if handle == nil || handle.closed || !handle.claim.Valid() || !handle.reservation.IsZero() ||
		reservation.IsZero() || reservation.OperationID() != handle.spec.OperationID ||
		reservation.ID() != handle.spec.ReservationID ||
		reservation.RequestedName() != handle.spec.RequestedName ||
		reservation.ReservedName() != handle.spec.ReservedName ||
		reservation.CollisionIndex() != handle.spec.CollisionIndex ||
		reservation.EntryKind() != handle.spec.EntryKind {
		return 0, transfer.ErrInvalidOutputBinding
	}
	handle.reservation = reservation
	handle.claim.Generation++
	return destinationauthority.ReservationMetadataClaimCommitted, nil
}

func (handle *volatileReservationClaimHandle) BindDirectoryIdentity(
	identity []byte,
) (destinationauthority.ReservationMetadataClaimOutcome, error) {
	if handle == nil || handle.closed || handle.reservation.IsZero() ||
		handle.spec.EntryKind != receivecontract.ContainerEntryResultRoot || len(identity) == 0 {
		return 0, transfer.ErrInvalidOutputBinding
	}
	handle.identity = bytes.Clone(identity)
	handle.claim.Generation++
	return destinationauthority.ReservationMetadataClaimCommitted, nil
}

func (handle *volatileReservationClaimHandle) Rollback() (
	destinationauthority.ReservationMetadataClaimOutcome,
	error,
) {
	if handle == nil || handle.closed || handle.owner == nil {
		return destinationauthority.ReservationMetadataClaimIndeterminate, transfer.ErrInvalidOutputBinding
	}
	processVolatileClaims.Lock()
	if processVolatileClaims.claims[handle.key] == handle.claim.Token {
		delete(processVolatileClaims.claims, handle.key)
		delete(handle.owner.claims, handle.key)
	}
	processVolatileClaims.Unlock()
	handle.claim = destinationauthority.ReservationClaim{}
	return destinationauthority.ReservationMetadataClaimCommitted, nil
}

func (handle *volatileReservationClaimHandle) Close() error {
	if handle == nil || handle.closed {
		return nil
	}
	handle.closed = true
	return nil
}

// Operation owns the exact frozen ordinary output coordinates. It intentionally
// exposes only the intent and selected mode; raw destination capabilities stay
// inside outputruntime for the W3 composition cut.
