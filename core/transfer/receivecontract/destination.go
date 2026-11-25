package receivecontract

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
)

const (
	destinationReservationDomain = "windshare/destination-reservation/v5"
	nameCollisionDomain          = "windshare/name-collision/v1"
)

type ContainerEntryKind uint8

const (
	ContainerEntrySingleFile ContainerEntryKind = iota + 1
	ContainerEntryResultRoot
)

type DestinationReservation struct {
	operation           OperationID
	id                  DestinationReservationID
	artifact            ArtifactDigest
	authority           AuthorityRef
	entryKind           ContainerEntryKind
	requestedName       string
	logicalReservedName string
	physicalName        string
	collisionIndex      uint32
	digest              BindingDigest
	valid               bool
}

func NewNativeNamedEntryReservation(
	operation OperationID,
	id DestinationReservationID,
	artifact ArtifactSpec,
	authority AuthorityRef,
	logicalReservedName, physicalName string,
	collisionIndex uint32,
) (DestinationReservation, error) {
	layout, ok := artifact.DirectoryTree()
	if !ok {
		return DestinationReservation{}, ErrInvalidReceiveContract
	}
	var entryKind ContainerEntryKind
	var requestedName string
	switch layout.Kind() {
	case DirectoryTreeSingleFile:
		single, _ := layout.SingleFile()
		entryKind, requestedName = ContainerEntrySingleFile, single.SuggestedName
	case DirectoryTreeResultRoot:
		root, _ := layout.ResultRoot()
		entryKind, requestedName = ContainerEntryResultRoot, root.Name()
	default:
		return DestinationReservation{}, ErrInvalidReceiveContract
	}
	expected, err := CollisionName(operation, requestedName, collisionIndex, entryKind == ContainerEntrySingleFile)
	if err != nil || logicalReservedName != expected || canonicalComponent(physicalName) != nil {
		return DestinationReservation{}, ErrInvalidReceiveContract
	}
	return newDestinationReservation(
		operation, id, artifact,
		authority, entryKind, requestedName, logicalReservedName, physicalName, collisionIndex,
	)
}

func newDestinationReservation(
	operation OperationID,
	id DestinationReservationID,
	artifact ArtifactSpec,
	authority AuthorityRef,
	entryKind ContainerEntryKind,
	requestedName, logicalReservedName, physicalName string,
	collisionIndex uint32,
) (DestinationReservation, error) {
	if operation.IsZero() || id.IsZero() || artifact.IsZero() || authority.IsZero() {
		return DestinationReservation{}, ErrInvalidReceiveContract
	}
	if entryKind != ContainerEntrySingleFile && entryKind != ContainerEntryResultRoot {
		return DestinationReservation{}, ErrInvalidReceiveContract
	}
	material := canonicalRecord(destinationReservationDomain,
		frame(operation.Bytes()), frame(id.Bytes()),
		frame(artifact.Digest().Bytes()), frame(authority.Bytes()),
		frame([]byte{byte(entryKind)}),
		frame([]byte(requestedName)),
		frame([]byte(logicalReservedName)),
		frame([]byte(physicalName)),
		frame(uint32Bytes(collisionIndex)),
	)
	sum := digest(material)
	return DestinationReservation{
		operation: operation, id: id, artifact: artifact.Digest(),
		authority: authority, entryKind: entryKind, requestedName: requestedName,
		logicalReservedName: logicalReservedName, physicalName: physicalName,
		collisionIndex: collisionIndex, digest: BindingDigest(sum), valid: true,
	}, nil
}

func CollisionName(operation OperationID, requestedName string, index uint32, fileLike bool) (string, error) {
	if operation.IsZero() || canonicalComponent(requestedName) != nil {
		return "", ErrInvalidReceiveContract
	}
	if index == 0 {
		return requestedName, nil
	}
	material := canonicalRecord(nameCollisionDomain,
		frame(operation.Bytes()), uint32Bytes(index), frame([]byte(requestedName)),
	)
	token := sha256.Sum256(material)
	suffix := "-" + hex.EncodeToString(token[:CollisionSuffixHexChars/2])
	stem, extension := requestedName, ""
	if fileLike {
		if dot := strings.LastIndexByte(requestedName, '.'); dot > 0 {
			stem, extension = requestedName[:dot], requestedName[dot:]
		}
	}
	maximumBaseBytes := MaxResultComponentBytes - len(suffix)
	stem = resultNamePrefix(stem, maximumBaseBytes-len(extension))
	if stem == "" {
		// Preserving the extension must not prevent a valid name from being
		// downloaded again. If no complete stem scalar fits, shorten the full name.
		stem, extension = resultNamePrefix(requestedName, maximumBaseBytes), ""
	}
	result := stem + suffix + extension
	if canonicalComponent(result) != nil {
		return "", ErrInvalidReceiveContract
	}
	return result, nil
}

func (reservation DestinationReservation) OperationID() OperationID     { return reservation.operation }
func (reservation DestinationReservation) ID() DestinationReservationID { return reservation.id }
func (reservation DestinationReservation) ArtifactDigest() ArtifactDigest {
	return reservation.artifact
}
func (reservation DestinationReservation) AuthorityRef() AuthorityRef { return reservation.authority }
func (reservation DestinationReservation) EntryKind() ContainerEntryKind {
	return reservation.entryKind
}
func (reservation DestinationReservation) RequestedName() string { return reservation.requestedName }
func (reservation DestinationReservation) LogicalReservedName() string {
	return reservation.logicalReservedName
}
func (reservation DestinationReservation) PhysicalName() string   { return reservation.physicalName }
func (reservation DestinationReservation) CollisionIndex() uint32 { return reservation.collisionIndex }
func (reservation DestinationReservation) Digest() BindingDigest  { return reservation.digest }
func (reservation DestinationReservation) IsZero() bool           { return !reservation.valid }

type destinationReservationJSON struct {
	OperationID         OperationID              `json:"operation_id"`
	ID                  DestinationReservationID `json:"id"`
	ArtifactDigest      ArtifactDigest           `json:"artifact_digest"`
	AuthorityRef        AuthorityRef             `json:"authority_ref"`
	EntryKind           string                   `json:"entry_kind"`
	RequestedName       string                   `json:"requested_name"`
	LogicalReservedName string                   `json:"logical_reserved_name"`
	PhysicalName        string                   `json:"physical_name"`
	CollisionIndex      uint32                   `json:"collision_index,omitempty"`
}

func (reservation DestinationReservation) MarshalJSON() ([]byte, error) {
	if !reservation.valid {
		return nil, ErrInvalidReceiveContract
	}
	var entryKind string
	switch reservation.entryKind {
	case ContainerEntrySingleFile:
		entryKind = "single_file"
	case ContainerEntryResultRoot:
		entryKind = "result_root"
	default:
		return nil, ErrInvalidReceiveContract
	}
	payload := destinationReservationJSON{
		OperationID:         reservation.operation,
		ID:                  reservation.id,
		ArtifactDigest:      reservation.artifact,
		AuthorityRef:        reservation.authority,
		EntryKind:           entryKind,
		RequestedName:       reservation.requestedName,
		LogicalReservedName: reservation.logicalReservedName,
		PhysicalName:        reservation.physicalName,
		CollisionIndex:      reservation.collisionIndex,
	}
	return json.Marshal(payload)
}

func decodeReservationJSON(payload destinationReservationJSON, artifact ArtifactSpec) (DestinationReservation, error) {
	if artifact.IsZero() || payload.ArtifactDigest != artifact.Digest() {
		return DestinationReservation{}, ErrInvalidReceiveContract
	}
	reservation, err := NewNativeNamedEntryReservation(
		payload.OperationID, payload.ID, artifact, payload.AuthorityRef,
		payload.LogicalReservedName, payload.PhysicalName, payload.CollisionIndex,
	)
	if err != nil {
		return DestinationReservation{}, err
	}
	var wantEntryKind ContainerEntryKind
	switch payload.EntryKind {
	case "single_file":
		wantEntryKind = ContainerEntrySingleFile
	case "result_root":
		wantEntryKind = ContainerEntryResultRoot
	default:
		return DestinationReservation{}, ErrInvalidReceiveContract
	}
	if reservation.EntryKind() != wantEntryKind || reservation.RequestedName() != payload.RequestedName {
		return DestinationReservation{}, ErrInvalidReceiveContract
	}
	return reservation, nil
}
