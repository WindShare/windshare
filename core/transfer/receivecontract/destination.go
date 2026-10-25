package receivecontract

import (
	"crypto/sha256"
	"encoding/hex"
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
	ReservationAtomicTarget
)

type AuthorityKind uint8

const (
	AuthorityNativeContainer AuthorityKind = iota + 1
	AuthorityManagedAtomicTarget
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
	authorityKind       AuthorityKind
	authority           AuthorityRef
	guarantees          GuaranteeSet
	entryKind           ContainerEntryKind
	requestedName       string
	logicalReservedName string
	physicalName        string
	reservedName        string
	collisionIndex      uint32
	encoded             []byte
	digest              BindingDigest
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
		ReservationContainerRoot, operation, id, artifact, AuthorityNativeContainer,
		authority, NativeTreeGuarantees(), 0, "", "", "", "", 0,
	)
}

func NewNativeNamedEntryReservation(
	operation OperationID,
	id DestinationReservationID,
	artifact ArtifactSpec,
	authority AuthorityRef,
	logicalReservedName string,
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
	if err != nil || logicalReservedName != expected {
		return DestinationReservation{}, ErrInvalidReceiveContract
	}
	return newDestinationReservation(
		ReservationNamedContainerEntry, operation, id, artifact, AuthorityNativeContainer,
		authority, NativeTreeGuarantees(), entryKind, requestedName, logicalReservedName, logicalReservedName, "", collisionIndex,
	)
}

func NewManagedAtomicReservation(
	operation OperationID,
	id DestinationReservationID,
	artifact ArtifactSpec,
	authority AuthorityRef,
	nameAuthority NameAuthority,
	requestedName, reservedName string,
	collisionIndex uint32,
) (DestinationReservation, error) {
	artifactName, ok := completeArtifactName(artifact)
	if !ok {
		return DestinationReservation{}, ErrInvalidReceiveContract
	}
	if nameAuthority == NameApplicationChosen && requestedName != artifactName {
		return DestinationReservation{}, ErrInvalidReceiveContract
	}
	expected, err := CollisionName(operation, requestedName, collisionIndex, true)
	guarantees, guaranteeErr := ManagedAtomicGuarantees(nameAuthority)
	if err != nil || guaranteeErr != nil || reservedName != expected {
		return DestinationReservation{}, ErrInvalidReceiveContract
	}
	return newDestinationReservation(
		ReservationAtomicTarget, operation, id, artifact, AuthorityManagedAtomicTarget,
		authority, guarantees, 0, requestedName, "", "", reservedName, collisionIndex,
	)
}

func newDestinationReservation(
	kind DestinationReservationKind,
	operation OperationID,
	id DestinationReservationID,
	artifact ArtifactSpec,
	authorityKind AuthorityKind,
	authority AuthorityRef,
	guarantees GuaranteeSet,
	entryKind ContainerEntryKind,
	requestedName, logicalReservedName, physicalName, reservedName string,
	collisionIndex uint32,
) (DestinationReservation, error) {
	if operation.IsZero() || id.IsZero() || artifact.IsZero() || authority.IsZero() || !guarantees.valid() {
		return DestinationReservation{}, ErrInvalidReceiveContract
	}
	encoded := canonicalRecord(destinationReservationDomain,
		[]byte{byte(kind)},
		frame(operation.Bytes()), frame(id.Bytes()), frame(artifact.Digest().Bytes()),
		frame([]byte{byte(authorityKind)}), frame(authority.Bytes()), frame(guarantees.canonicalBytes()),
	)
	switch kind {
	case ReservationContainerRoot:
		if authorityKind != AuthorityNativeContainer || guarantees.profile != GuaranteeNativeTree ||
			entryKind != 0 || requestedName != "" || logicalReservedName != "" ||
			physicalName != "" || reservedName != "" || collisionIndex != 0 {
			return DestinationReservation{}, ErrInvalidReceiveContract
		}
	case ReservationNamedContainerEntry:
		if authorityKind != AuthorityNativeContainer ||
			(entryKind != ContainerEntrySingleFile && entryKind != ContainerEntryResultRoot) {
			return DestinationReservation{}, ErrInvalidReceiveContract
		}
		encoded = append(encoded, frame([]byte{byte(entryKind)})...)
		encoded = append(encoded, frame([]byte(requestedName))...)
		encoded = append(encoded, frame([]byte(logicalReservedName))...)
		encoded = append(encoded, frame([]byte(physicalName))...)
		encoded = append(encoded, frame(uint32Bytes(collisionIndex))...)
	case ReservationAtomicTarget:
		if authorityKind != AuthorityManagedAtomicTarget || guarantees.profile != GuaranteeManagedAtomic || entryKind != 0 ||
			logicalReservedName != "" || physicalName != "" {
			return DestinationReservation{}, ErrInvalidReceiveContract
		}
		encoded = append(encoded, frame([]byte(requestedName))...)
		encoded = append(encoded, frame([]byte(reservedName))...)
		encoded = append(encoded, frame(uint32Bytes(collisionIndex))...)
	default:
		return DestinationReservation{}, ErrInvalidReceiveContract
	}
	sum := digest(encoded)
	return DestinationReservation{
		kind: kind, operation: operation, id: id, artifact: artifact.Digest(),
		authorityKind: authorityKind, authority: authority, guarantees: guarantees,
		entryKind: entryKind, requestedName: requestedName,
		logicalReservedName: logicalReservedName, physicalName: physicalName, reservedName: reservedName,
		collisionIndex: collisionIndex,
		encoded:        encoded, digest: BindingDigest(sum),
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

func completeArtifactName(artifact ArtifactSpec) (string, bool) {
	if original, ok := artifact.OriginalFile(); ok {
		return original.SuggestedName, true
	}
	if zip, ok := artifact.ZipArchive(); ok {
		return zip.SuggestedName, true
	}
	return "", false
}

func (reservation DestinationReservation) valid() bool {
	return !reservation.operation.IsZero() && !reservation.id.IsZero() &&
		!reservation.artifact.IsZero() && !reservation.authority.IsZero() &&
		reservation.guarantees.valid() && !reservation.digest.IsZero() &&
		BindingDigest(digest(reservation.encoded)) == reservation.digest
}

func (reservation DestinationReservation) Kind() DestinationReservationKind { return reservation.kind }
func (reservation DestinationReservation) OperationID() OperationID         { return reservation.operation }
func (reservation DestinationReservation) ID() DestinationReservationID     { return reservation.id }
func (reservation DestinationReservation) ArtifactDigest() ArtifactDigest {
	return reservation.artifact
}
func (reservation DestinationReservation) AuthorityKind() AuthorityKind {
	return reservation.authorityKind
}
func (reservation DestinationReservation) AuthorityRef() AuthorityRef { return reservation.authority }
func (reservation DestinationReservation) Guarantees() GuaranteeSet   { return reservation.guarantees }
func (reservation DestinationReservation) EntryKind() ContainerEntryKind {
	return reservation.entryKind
}
func (reservation DestinationReservation) RequestedName() string { return reservation.requestedName }
func (reservation DestinationReservation) LogicalReservedName() string {
	return reservation.logicalReservedName
}
func (reservation DestinationReservation) PhysicalName() string   { return reservation.physicalName }
func (reservation DestinationReservation) ReservedName() string   { return reservation.reservedName }
func (reservation DestinationReservation) CollisionIndex() uint32 { return reservation.collisionIndex }
func (reservation DestinationReservation) CanonicalBytes() []byte { return clone(reservation.encoded) }
func (reservation DestinationReservation) Digest() BindingDigest  { return reservation.digest }
func (reservation DestinationReservation) IsZero() bool           { return !reservation.valid() }
