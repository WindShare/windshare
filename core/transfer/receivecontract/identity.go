package receivecontract

import (
	"bytes"
	"crypto/sha256"
	"errors"
)

const (
	StableIdentityBytes   = 16
	AuthorityRefBytes     = sha256.Size
	ArtifactChoiceIDBytes = sha256.Size
)

var ErrInvalidReceiveContract = errors.New("receive contract is invalid")

type OperationID [StableIdentityBytes]byte
type DestinationReservationID [StableIdentityBytes]byte
type WorkspaceID [StableIdentityBytes]byte
type PortablePlanID [StableIdentityBytes]byte
type AuthorityRef [AuthorityRefBytes]byte
type RepositoryRef [AuthorityRefBytes]byte
type FSAOwnedTargetRef [AuthorityRefBytes]byte
type ArtifactDigest [sha256.Size]byte
type BindingDigest [sha256.Size]byte
type PolicyDigest [sha256.Size]byte
type ArtifactChoiceID [ArtifactChoiceIDBytes]byte

const artifactChoiceDomain = "windshare/artifact-choice/v1"

type ArtifactChoiceIdentity struct {
	artifactKind        ArtifactKind
	materializationKind MaterializationPlanKind
	guaranteeProfile    GuaranteeProfile
	preparation         PreparationPolicy
	encoded             []byte
	id                  ArtifactChoiceID
}

func NewArtifactChoiceIdentity(
	artifactKind ArtifactKind,
	materializationKind MaterializationPlanKind,
	guaranteeProfile GuaranteeProfile,
	preparation PreparationPolicy,
) (ArtifactChoiceIdentity, error) {
	if !legalArtifactChoiceTuple(artifactKind, materializationKind, guaranteeProfile, preparation) {
		return ArtifactChoiceIdentity{}, ErrInvalidReceiveContract
	}
	encoded := canonicalRecord(artifactChoiceDomain,
		frame([]byte{byte(artifactKind)}),
		frame([]byte{byte(materializationKind)}),
		frame([]byte{byte(guaranteeProfile)}),
		frame([]byte{byte(preparation)}),
	)
	sum := digest(encoded)
	return ArtifactChoiceIdentity{
		artifactKind: artifactKind, materializationKind: materializationKind,
		guaranteeProfile: guaranteeProfile, preparation: preparation,
		encoded: encoded, id: ArtifactChoiceID(sum),
	}, nil
}

func DeriveArtifactChoiceIdentity(artifact ArtifactSpec, plan MaterializationPlan) (ArtifactChoiceIdentity, error) {
	if artifact.IsZero() || plan.IsZero() || plan.ArtifactDigest() != artifact.Digest() {
		return ArtifactChoiceIdentity{}, ErrInvalidReceiveContract
	}
	return NewArtifactChoiceIdentity(artifact.Kind(), plan.Kind(), plan.GuaranteeProfile(), plan.Preparation())
}

func DecodeArtifactChoiceIdentity(encoded []byte) (ArtifactChoiceIdentity, error) {
	cursor, err := newContractDecoder(encoded, artifactChoiceDomain)
	if err != nil {
		return ArtifactChoiceIdentity{}, err
	}
	artifactKind, artifactErr := cursor.framedByte()
	materializationKind, materializationErr := cursor.framedByte()
	guaranteeProfile, guaranteeErr := cursor.framedByte()
	preparation, preparationErr := cursor.framedByte()
	if firstDecodeError(artifactErr, materializationErr, guaranteeErr, preparationErr) != nil || !cursor.done() {
		return ArtifactChoiceIdentity{}, ErrInvalidReceiveContract
	}
	identity, err := NewArtifactChoiceIdentity(
		ArtifactKind(artifactKind), MaterializationPlanKind(materializationKind),
		GuaranteeProfile(guaranteeProfile), PreparationPolicy(preparation),
	)
	if err != nil || !bytes.Equal(identity.CanonicalBytes(), encoded) {
		return ArtifactChoiceIdentity{}, ErrInvalidReceiveContract
	}
	return identity, nil
}

func legalArtifactChoiceTuple(
	artifactKind ArtifactKind,
	materializationKind MaterializationPlanKind,
	guaranteeProfile GuaranteeProfile,
	preparation PreparationPolicy,
) bool {
	switch materializationKind {
	case PlanDirectTree:
		return artifactKind == ArtifactDirectoryTree && preparation == PreparationNone &&
			(guaranteeProfile == GuaranteeNativeTree || guaranteeProfile == GuaranteeFSATree)
	case PlanDirectAtomic:
		return artifactKind == ArtifactOriginalFile && guaranteeProfile == GuaranteeManagedAtomic &&
			preparation == PreparationNone
	case PlanWorkspaceThenPublish:
		expected := PreparationNone
		if artifactKind == ArtifactZipArchive {
			expected = PreparationExactZip
		} else if artifactKind != ArtifactOriginalFile {
			return false
		}
		return preparation == expected &&
			(guaranteeProfile == GuaranteeManagedAtomic || guaranteeProfile == GuaranteeBrowserHandoff)
	case PlanPortableHandoff:
		return (artifactKind == ArtifactOriginalFile || artifactKind == ArtifactZipArchive) &&
			guaranteeProfile == GuaranteeBrowserHandoff && preparation == PreparationExactArtifact
	case PlanDirectResumableZIP:
		return artifactKind == ArtifactZipArchive && guaranteeProfile == GuaranteeFSAOwnedFile &&
			preparation == PreparationNone
	default:
		return false
	}
}

func (identity ArtifactChoiceIdentity) ArtifactKind() ArtifactKind { return identity.artifactKind }
func (identity ArtifactChoiceIdentity) MaterializationKind() MaterializationPlanKind {
	return identity.materializationKind
}
func (identity ArtifactChoiceIdentity) GuaranteeProfile() GuaranteeProfile {
	return identity.guaranteeProfile
}
func (identity ArtifactChoiceIdentity) Preparation() PreparationPolicy { return identity.preparation }
func (identity ArtifactChoiceIdentity) CanonicalBytes() []byte         { return clone(identity.encoded) }
func (identity ArtifactChoiceIdentity) ID() ArtifactChoiceID           { return identity.id }
func (identity ArtifactChoiceIdentity) IsZero() bool {
	return identity.id.IsZero() || !legalArtifactChoiceTuple(
		identity.artifactKind, identity.materializationKind,
		identity.guaranteeProfile, identity.preparation,
	) || ArtifactChoiceID(digest(identity.encoded)) != identity.id
}

func OperationIDFromBytes(raw []byte) (OperationID, error) {
	var value OperationID
	if !copyIdentity(value[:], raw) {
		return OperationID{}, ErrInvalidReceiveContract
	}
	return value, nil
}

func DestinationReservationIDFromBytes(raw []byte) (DestinationReservationID, error) {
	var value DestinationReservationID
	if !copyIdentity(value[:], raw) {
		return DestinationReservationID{}, ErrInvalidReceiveContract
	}
	return value, nil
}

func WorkspaceIDFromBytes(raw []byte) (WorkspaceID, error) {
	var value WorkspaceID
	if !copyIdentity(value[:], raw) {
		return WorkspaceID{}, ErrInvalidReceiveContract
	}
	return value, nil
}

func PortablePlanIDFromBytes(raw []byte) (PortablePlanID, error) {
	var value PortablePlanID
	if !copyIdentity(value[:], raw) {
		return PortablePlanID{}, ErrInvalidReceiveContract
	}
	return value, nil
}

func AuthorityRefFromBytes(raw []byte) (AuthorityRef, error) {
	var value AuthorityRef
	if !copyIdentity(value[:], raw) {
		return AuthorityRef{}, ErrInvalidReceiveContract
	}
	return value, nil
}

func RepositoryRefFromBytes(raw []byte) (RepositoryRef, error) {
	var value RepositoryRef
	if !copyIdentity(value[:], raw) {
		return RepositoryRef{}, ErrInvalidReceiveContract
	}
	return value, nil
}

func FSAOwnedTargetRefFromBytes(raw []byte) (FSAOwnedTargetRef, error) {
	var value FSAOwnedTargetRef
	if !copyIdentity(value[:], raw) {
		return FSAOwnedTargetRef{}, ErrInvalidReceiveContract
	}
	return value, nil
}

func ArtifactDigestFromBytes(raw []byte) (ArtifactDigest, error) {
	var value ArtifactDigest
	if !copyIdentity(value[:], raw) {
		return ArtifactDigest{}, ErrInvalidReceiveContract
	}
	return value, nil
}

func BindingDigestFromBytes(raw []byte) (BindingDigest, error) {
	var value BindingDigest
	if !copyIdentity(value[:], raw) {
		return BindingDigest{}, ErrInvalidReceiveContract
	}
	return value, nil
}

func PolicyDigestFromBytes(raw []byte) (PolicyDigest, error) {
	var value PolicyDigest
	if !copyIdentity(value[:], raw) {
		return PolicyDigest{}, ErrInvalidReceiveContract
	}
	return value, nil
}

func ArtifactChoiceIDFromBytes(raw []byte) (ArtifactChoiceID, error) {
	var value ArtifactChoiceID
	if !copyIdentity(value[:], raw) {
		return ArtifactChoiceID{}, ErrInvalidReceiveContract
	}
	return value, nil
}

func copyIdentity(destination, raw []byte) bool {
	if len(destination) != len(raw) || !nonZero(raw) {
		return false
	}
	copy(destination, raw)
	return true
}

func (id OperationID) Bytes() []byte              { return clone(id[:]) }
func (id DestinationReservationID) Bytes() []byte { return clone(id[:]) }
func (id WorkspaceID) Bytes() []byte              { return clone(id[:]) }
func (id PortablePlanID) Bytes() []byte           { return clone(id[:]) }
func (id AuthorityRef) Bytes() []byte             { return clone(id[:]) }
func (id RepositoryRef) Bytes() []byte            { return clone(id[:]) }
func (id FSAOwnedTargetRef) Bytes() []byte        { return clone(id[:]) }
func (id ArtifactDigest) Bytes() []byte           { return clone(id[:]) }
func (id BindingDigest) Bytes() []byte            { return clone(id[:]) }
func (id PolicyDigest) Bytes() []byte             { return clone(id[:]) }
func (id ArtifactChoiceID) Bytes() []byte         { return clone(id[:]) }

func (id OperationID) IsZero() bool              { return id == OperationID{} }
func (id DestinationReservationID) IsZero() bool { return id == DestinationReservationID{} }
func (id WorkspaceID) IsZero() bool              { return id == WorkspaceID{} }
func (id PortablePlanID) IsZero() bool           { return id == PortablePlanID{} }
func (id AuthorityRef) IsZero() bool             { return id == AuthorityRef{} }
func (id RepositoryRef) IsZero() bool            { return id == RepositoryRef{} }
func (id FSAOwnedTargetRef) IsZero() bool        { return id == FSAOwnedTargetRef{} }
func (id ArtifactDigest) IsZero() bool           { return id == ArtifactDigest{} }
func (id BindingDigest) IsZero() bool            { return id == BindingDigest{} }
func (id PolicyDigest) IsZero() bool             { return id == PolicyDigest{} }
func (id ArtifactChoiceID) IsZero() bool         { return id == ArtifactChoiceID{} }
