package receivecontract

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
)

const (
	destinationReservationDomain = "windshare/destination-reservation/v4"
	nameCollisionDomain          = "windshare/name-collision/v1"
)

type DestinationReservationKind uint8

const (
	ReservationContainerRoot DestinationReservationKind = iota + 1
	ReservationNamedContainerEntry
)

type ContainerEntryKind uint8

const (
	ContainerEntrySingleFile ContainerEntryKind = iota + 1
	ContainerEntryResultRoot
)

type DestinationReservation struct {
	kind                DestinationReservationKind
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

func NewNativeContainerRootReservation(
	operation OperationID,
	id DestinationReservationID,
	artifact ArtifactSpec,
	authority AuthorityRef,
) (DestinationReservation, error) {
	layout, ok := artifact.DirectoryTree()
	if !ok || layout.Kind() != DirectoryTreeCatalogRoot {
		return DestinationReservation{}, ErrInvalidReceiveContract
	}
	return newDestinationReservation(
		ReservationContainerRoot, operation, id, artifact,
		authority, 0, "", "", "", 0,
	)
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
		ReservationNamedContainerEntry, operation, id, artifact,
		authority, entryKind, requestedName, logicalReservedName, physicalName, collisionIndex,
	)
}

func newDestinationReservation(
	kind DestinationReservationKind,
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
	material := canonicalRecord(destinationReservationDomain,
		[]byte{byte(kind)},
		frame(operation.Bytes()), frame(id.Bytes()),
		frame(artifact.Digest().Bytes()), frame(authority.Bytes()),
	)
	switch kind {
	case ReservationContainerRoot:
		if entryKind != 0 || requestedName != "" || logicalReservedName != "" ||
			physicalName != "" || collisionIndex != 0 {
			return DestinationReservation{}, ErrInvalidReceiveContract
		}
	case ReservationNamedContainerEntry:
		if entryKind != ContainerEntrySingleFile && entryKind != ContainerEntryResultRoot {
			return DestinationReservation{}, ErrInvalidReceiveContract
		}
		material = append(material, frame([]byte{byte(entryKind)})...)
		material = append(material, frame([]byte(requestedName))...)
		material = append(material, frame([]byte(logicalReservedName))...)
		material = append(material, frame([]byte(physicalName))...)
		material = append(material, frame(uint32Bytes(collisionIndex))...)
	default:
		return DestinationReservation{}, ErrInvalidReceiveContract
	}
	sum := digest(material)
	return DestinationReservation{
		kind: kind, operation: operation, id: id, artifact: artifact.Digest(),
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

func (reservation DestinationReservation) Kind() DestinationReservationKind { return reservation.kind }
func (reservation DestinationReservation) OperationID() OperationID         { return reservation.operation }
func (reservation DestinationReservation) ID() DestinationReservationID     { return reservation.id }
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
	Kind                string                   `json:"kind"`
	OperationID         OperationID              `json:"operation_id"`
	ID                  DestinationReservationID `json:"id"`
	ArtifactDigest      ArtifactDigest           `json:"artifact_digest"`
	AuthorityRef        AuthorityRef             `json:"authority_ref"`
	EntryKind           string                   `json:"entry_kind,omitempty"`
	RequestedName       string                   `json:"requested_name,omitempty"`
	LogicalReservedName string                   `json:"logical_reserved_name,omitempty"`
	PhysicalName        string                   `json:"physical_name,omitempty"`
	CollisionIndex      uint32                   `json:"collision_index,omitempty"`
}

func (reservation DestinationReservation) MarshalJSON() ([]byte, error) {
	if !reservation.valid {
		return nil, ErrInvalidReceiveContract
	}
	payload := destinationReservationJSON{
		OperationID:    reservation.operation,
		ID:             reservation.id,
		ArtifactDigest: reservation.artifact,
		AuthorityRef:   reservation.authority,
	}
	switch reservation.kind {
	case ReservationContainerRoot:
		payload.Kind = "container_root"
	case ReservationNamedContainerEntry:
		payload.Kind = "named_entry"
		switch reservation.entryKind {
		case ContainerEntrySingleFile:
			payload.EntryKind = "single_file"
		case ContainerEntryResultRoot:
			payload.EntryKind = "result_root"
		default:
			return nil, ErrInvalidReceiveContract
		}
		payload.RequestedName = reservation.requestedName
		payload.LogicalReservedName = reservation.logicalReservedName
		payload.PhysicalName = reservation.physicalName
		payload.CollisionIndex = reservation.collisionIndex
	default:
		return nil, ErrInvalidReceiveContract
	}
	return json.Marshal(payload)
}

func decodeReservationJSON(payload destinationReservationJSON, artifact ArtifactSpec) (DestinationReservation, error) {
	if artifact.IsZero() || payload.ArtifactDigest != artifact.Digest() {
		return DestinationReservation{}, ErrInvalidReceiveContract
	}
	switch payload.Kind {
	case "container_root":
		if payload.EntryKind != "" || payload.RequestedName != "" ||
			payload.LogicalReservedName != "" || payload.PhysicalName != "" ||
			payload.CollisionIndex != 0 {
			return DestinationReservation{}, ErrInvalidReceiveContract
		}
		return NewNativeContainerRootReservation(payload.OperationID, payload.ID, artifact, payload.AuthorityRef)
	case "named_entry":
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
	default:
		return DestinationReservation{}, ErrInvalidReceiveContract
	}
}
