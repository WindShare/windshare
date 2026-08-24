package receivecontract

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"strings"
	"unicode/utf8"
)

const (
	destinationReservationDomain = "windshare/destination-reservation/v4"
	fsaOwnedFileBindingDomain    = "windshare/fsa-owned-file-binding/v1"
	workspaceBindingDomain       = "windshare/workspace-binding/v1"
	portableBindingDomain        = "windshare/portable-binding/v1"
	nameCollisionDomain          = "windshare/name-collision/v1"

	DefaultPortableArtifactLimit             = uint64(67_108_864)
	DefaultPortableAssemblyPartBytes         = uint64(1_048_576)
	DefaultPortableMaximumParts              = uint64(64)
	BrowserHandoffObjectURLLeaseMillis       = uint64(60_000)
	DirectZipCandidateTokenLength            = 22
	DirectZipStableNameInfix                 = ".windshare-"
	FSAReservedRootLayoutV1            uint8 = 1
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
	AuthorityFSAContainer
	AuthorityManagedAtomicTarget
)

type ContainerEntryKind uint8

const (
	ContainerEntrySingleFile ContainerEntryKind = iota + 1
	ContainerEntryResultRoot
)

type WorkspaceKind uint8
type WorkspaceBudgetPolicy uint8
type WorkspaceRetentionPolicy uint8

const (
	WorkspaceOriginPrivate  WorkspaceKind            = 1
	WorkspaceBudgetV1       WorkspaceBudgetPolicy    = 1
	WorkspaceStable24HourV1 WorkspaceRetentionPolicy = 1
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
	fsaLayoutVersion    uint8
	encoded             []byte
	digest              BindingDigest
}

type WorkspaceBinding struct {
	operation       OperationID
	id              WorkspaceID
	artifact        ArtifactDigest
	repository      RepositoryRef
	workspaceKind   WorkspaceKind
	budgetPolicy    WorkspaceBudgetPolicy
	retentionPolicy WorkspaceRetentionPolicy
	encoded         []byte
	digest          BindingDigest
}

type PortableBinding struct {
	operation                  OperationID
	id                         PortablePlanID
	artifact                   ArtifactDigest
	maximumArtifactBytes       uint64
	assemblyPartBytes          uint64
	maximumParts               uint64
	objectURLLeaseMilliseconds uint64
	preparation                PreparationPolicy
	encoded                    []byte
	digest                     BindingDigest
}

type DirectZipPolicyDigests struct {
	ZipEncoding   PolicyDigest
	Layout        PolicyDigest
	Checkpoint    PolicyDigest
	JournalBudget PolicyDigest
	Epoch         PolicyDigest
}

type FSAOwnedFileBinding struct {
	operation  OperationID
	artifact   ArtifactDigest
	stableName string
	target     FSAOwnedTargetRef
	guarantees GuaranteeSet
	policies   DirectZipPolicyDigests
	encoded    []byte
	digest     BindingDigest
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
	return newNamedEntryReservation(
		operation, id, artifact, AuthorityNativeContainer, authority,
		NativeTreeGuarantees(), logicalReservedName, logicalReservedName, collisionIndex,
	)
}

func NewFSANamedEntryReservation(
	operation OperationID,
	id DestinationReservationID,
	artifact ArtifactSpec,
	authority AuthorityRef,
	logicalReservedName string,
	physicalName string,
	collisionIndex uint32,
) (DestinationReservation, error) {
	return newNamedEntryReservation(
		operation, id, artifact, AuthorityFSAContainer, authority,
		FSATreeGuarantees(), logicalReservedName, physicalName, collisionIndex,
	)
}

func newNamedEntryReservation(
	operation OperationID,
	id DestinationReservationID,
	artifact ArtifactSpec,
	authorityKind AuthorityKind,
	authority AuthorityRef,
	guarantees GuaranteeSet,
	logicalReservedName string,
	physicalName string,
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
	if err != nil || logicalReservedName != expected || canonicalComponent(physicalName) != nil ||
		(authorityKind == AuthorityNativeContainer && physicalName != logicalReservedName) {
		return DestinationReservation{}, ErrInvalidReceiveContract
	}
	return newDestinationReservation(
		ReservationNamedContainerEntry, operation, id, artifact, authorityKind,
		authority, guarantees, entryKind, requestedName, logicalReservedName, physicalName, "", collisionIndex,
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
	var fsaLayoutVersion uint8
	switch kind {
	case ReservationContainerRoot:
		if authorityKind != AuthorityNativeContainer || guarantees.profile != GuaranteeNativeTree ||
			entryKind != 0 || requestedName != "" || logicalReservedName != "" ||
			physicalName != "" || reservedName != "" || collisionIndex != 0 {
			return DestinationReservation{}, ErrInvalidReceiveContract
		}
	case ReservationNamedContainerEntry:
		if (authorityKind != AuthorityNativeContainer && authorityKind != AuthorityFSAContainer) ||
			(entryKind != ContainerEntrySingleFile && entryKind != ContainerEntryResultRoot) {
			return DestinationReservation{}, ErrInvalidReceiveContract
		}
		encoded = append(encoded, frame([]byte{byte(entryKind)})...)
		encoded = append(encoded, frame([]byte(requestedName))...)
		encoded = append(encoded, frame([]byte(logicalReservedName))...)
		encoded = append(encoded, frame([]byte(physicalName))...)
		encoded = append(encoded, frame(uint32Bytes(collisionIndex))...)
		if authorityKind == AuthorityFSAContainer {
			fsaLayoutVersion = FSAReservedRootLayoutV1
			encoded = append(encoded, frame([]byte{fsaLayoutVersion})...)
		}
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
		collisionIndex: collisionIndex, fsaLayoutVersion: fsaLayoutVersion,
		encoded: encoded, digest: BindingDigest(sum),
	}, nil
}

func NewWorkspaceBinding(
	operation OperationID,
	id WorkspaceID,
	artifact ArtifactSpec,
	repository RepositoryRef,
) (WorkspaceBinding, error) {
	if operation.IsZero() || id.IsZero() || artifact.IsZero() || repository.IsZero() ||
		artifact.Kind() == ArtifactDirectoryTree {
		return WorkspaceBinding{}, ErrInvalidReceiveContract
	}
	encoded := canonicalRecord(workspaceBindingDomain,
		frame(operation.Bytes()), frame(id.Bytes()), frame(artifact.Digest().Bytes()),
		frame(repository.Bytes()), frame([]byte{byte(WorkspaceOriginPrivate)}),
		frame([]byte{byte(WorkspaceBudgetV1)}), frame([]byte{byte(WorkspaceStable24HourV1)}),
	)
	sum := digest(encoded)
	return WorkspaceBinding{
		operation: operation, id: id, artifact: artifact.Digest(), repository: repository,
		workspaceKind: WorkspaceOriginPrivate, budgetPolicy: WorkspaceBudgetV1,
		retentionPolicy: WorkspaceStable24HourV1,
		encoded:         encoded, digest: BindingDigest(sum),
	}, nil
}

func NewPortableBinding(operation OperationID, id PortablePlanID, artifact ArtifactSpec) (PortableBinding, error) {
	if operation.IsZero() || id.IsZero() || artifact.IsZero() || artifact.Kind() == ArtifactDirectoryTree {
		return PortableBinding{}, ErrInvalidReceiveContract
	}
	encoded := canonicalRecord(portableBindingDomain,
		frame(operation.Bytes()), frame(id.Bytes()), frame(artifact.Digest().Bytes()),
		frame(uint64Bytes(DefaultPortableArtifactLimit)),
		frame(uint64Bytes(DefaultPortableAssemblyPartBytes)),
		frame(uint64Bytes(DefaultPortableMaximumParts)),
		frame(uint64Bytes(BrowserHandoffObjectURLLeaseMillis)),
		frame([]byte{byte(PreparationExactArtifact)}),
	)
	sum := digest(encoded)
	return PortableBinding{
		operation: operation, id: id, artifact: artifact.Digest(),
		maximumArtifactBytes:       DefaultPortableArtifactLimit,
		assemblyPartBytes:          DefaultPortableAssemblyPartBytes,
		maximumParts:               DefaultPortableMaximumParts,
		objectURLLeaseMilliseconds: BrowserHandoffObjectURLLeaseMillis,
		preparation:                PreparationExactArtifact,
		encoded:                    encoded, digest: BindingDigest(sum),
	}, nil
}

func NewFSAOwnedFileBinding(
	operation OperationID,
	artifact ArtifactSpec,
	stableName string,
	target FSAOwnedTargetRef,
	policies DirectZipPolicyDigests,
) (FSAOwnedFileBinding, error) {
	if operation.IsZero() || artifact.IsZero() || artifact.Kind() != ArtifactZipArchive ||
		target.IsZero() || !policies.Available() || !validDirectZipStableName(stableName) {
		return FSAOwnedFileBinding{}, ErrInvalidReceiveContract
	}
	guarantees := FSAOwnedFileGuarantees()
	encoded := canonicalRecord(fsaOwnedFileBindingDomain,
		frame(operation.Bytes()),
		frame(artifact.Digest().Bytes()),
		frame([]byte(stableName)),
		frame(target.Bytes()),
		frame(guarantees.canonicalBytes()),
		frame(policies.ZipEncoding.Bytes()),
		frame(policies.Layout.Bytes()),
		frame(policies.Checkpoint.Bytes()),
		frame(policies.JournalBudget.Bytes()),
		frame(policies.Epoch.Bytes()),
	)
	sum := digest(encoded)
	return FSAOwnedFileBinding{
		operation: operation, artifact: artifact.Digest(), stableName: stableName,
		target: target, guarantees: guarantees, policies: policies,
		encoded: encoded, digest: BindingDigest(sum),
	}, nil
}

// Available is deliberately false when any measured policy identity is absent.
// A zero digest is absence, never a placeholder that can authorize a direct route.
func (policies DirectZipPolicyDigests) Available() bool {
	return !policies.ZipEncoding.IsZero() && !policies.Layout.IsZero() &&
		!policies.Checkpoint.IsZero() && !policies.JournalBudget.IsZero() &&
		!policies.Epoch.IsZero()
}

func validDirectZipStableName(value string) bool {
	if canonicalComponent(value) != nil || !strings.HasSuffix(value, ArchiveExtension) {
		return false
	}
	withoutExtension := strings.TrimSuffix(value, ArchiveExtension)
	separator := strings.LastIndex(withoutExtension, DirectZipStableNameInfix)
	if separator <= 0 {
		return false
	}
	token := withoutExtension[separator+len(DirectZipStableNameInfix):]
	if len(token) != DirectZipCandidateTokenLength || strings.Contains(token, "=") {
		return false
	}
	raw, err := base64.RawURLEncoding.DecodeString(token)
	return err == nil && len(raw) == StableIdentityBytes && nonZero(raw)
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
	maximumStemBytes := MaxResultComponentBytes - len([]byte(suffix)) - len([]byte(extension))
	for len([]byte(stem)) > maximumStemBytes {
		_, width := utf8.DecodeLastRuneInString(stem)
		if width == 0 {
			return "", ErrInvalidReceiveContract
		}
		stem = stem[:len(stem)-width]
	}
	if stem == "" {
		return "", ErrInvalidReceiveContract
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
	expectedFSALayoutVersion := uint8(0)
	if reservation.kind == ReservationNamedContainerEntry &&
		reservation.authorityKind == AuthorityFSAContainer {
		expectedFSALayoutVersion = FSAReservedRootLayoutV1
	}
	return reservation.fsaLayoutVersion == expectedFSALayoutVersion &&
		!reservation.operation.IsZero() && !reservation.id.IsZero() &&
		!reservation.artifact.IsZero() && !reservation.authority.IsZero() &&
		reservation.guarantees.valid() && !reservation.digest.IsZero() &&
		BindingDigest(digest(reservation.encoded)) == reservation.digest
}

func (binding WorkspaceBinding) valid() bool {
	return !binding.operation.IsZero() && !binding.id.IsZero() && !binding.artifact.IsZero() &&
		!binding.repository.IsZero() && binding.workspaceKind == WorkspaceOriginPrivate &&
		binding.budgetPolicy == WorkspaceBudgetV1 && binding.retentionPolicy == WorkspaceStable24HourV1 &&
		!binding.digest.IsZero() &&
		BindingDigest(digest(binding.encoded)) == binding.digest
}

func (binding PortableBinding) valid() bool {
	return !binding.operation.IsZero() && !binding.id.IsZero() && !binding.artifact.IsZero() &&
		binding.maximumArtifactBytes == DefaultPortableArtifactLimit &&
		binding.assemblyPartBytes == DefaultPortableAssemblyPartBytes &&
		binding.maximumParts == DefaultPortableMaximumParts &&
		binding.objectURLLeaseMilliseconds == BrowserHandoffObjectURLLeaseMillis &&
		binding.preparation == PreparationExactArtifact &&
		!binding.digest.IsZero() && BindingDigest(digest(binding.encoded)) == binding.digest
}

func (binding FSAOwnedFileBinding) valid() bool {
	return !binding.operation.IsZero() && !binding.artifact.IsZero() &&
		validDirectZipStableName(binding.stableName) && !binding.target.IsZero() &&
		binding.guarantees == FSAOwnedFileGuarantees() && binding.policies.Available() &&
		!binding.digest.IsZero() && BindingDigest(digest(binding.encoded)) == binding.digest
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
func (reservation DestinationReservation) FSALayoutVersion() (uint8, bool) {
	return reservation.fsaLayoutVersion,
		reservation.valid() && reservation.authorityKind == AuthorityFSAContainer
}
func (reservation DestinationReservation) CanonicalBytes() []byte    { return clone(reservation.encoded) }
func (reservation DestinationReservation) Digest() BindingDigest     { return reservation.digest }
func (reservation DestinationReservation) IsZero() bool              { return !reservation.valid() }
func (binding WorkspaceBinding) OperationID() OperationID            { return binding.operation }
func (binding WorkspaceBinding) WorkspaceID() WorkspaceID            { return binding.id }
func (binding WorkspaceBinding) ArtifactDigest() ArtifactDigest      { return binding.artifact }
func (binding WorkspaceBinding) RepositoryRef() RepositoryRef        { return binding.repository }
func (binding WorkspaceBinding) WorkspaceKind() WorkspaceKind        { return binding.workspaceKind }
func (binding WorkspaceBinding) BudgetPolicy() WorkspaceBudgetPolicy { return binding.budgetPolicy }
func (binding WorkspaceBinding) RetentionPolicy() WorkspaceRetentionPolicy {
	return binding.retentionPolicy
}
func (binding WorkspaceBinding) CanonicalBytes() []byte        { return clone(binding.encoded) }
func (binding WorkspaceBinding) Digest() BindingDigest         { return binding.digest }
func (binding WorkspaceBinding) IsZero() bool                  { return !binding.valid() }
func (binding PortableBinding) OperationID() OperationID       { return binding.operation }
func (binding PortableBinding) PortablePlanID() PortablePlanID { return binding.id }
func (binding PortableBinding) ArtifactDigest() ArtifactDigest { return binding.artifact }
func (binding PortableBinding) MaximumArtifactBytes() uint64   { return binding.maximumArtifactBytes }
func (binding PortableBinding) AssemblyPartBytes() uint64      { return binding.assemblyPartBytes }
func (binding PortableBinding) MaximumParts() uint64           { return binding.maximumParts }
func (binding PortableBinding) ObjectURLLeaseMilliseconds() uint64 {
	return binding.objectURLLeaseMilliseconds
}
func (binding PortableBinding) Preparation() PreparationPolicy            { return binding.preparation }
func (binding PortableBinding) CanonicalBytes() []byte                    { return clone(binding.encoded) }
func (binding PortableBinding) Digest() BindingDigest                     { return binding.digest }
func (binding PortableBinding) IsZero() bool                              { return !binding.valid() }
func (binding FSAOwnedFileBinding) OperationID() OperationID              { return binding.operation }
func (binding FSAOwnedFileBinding) ArtifactDigest() ArtifactDigest        { return binding.artifact }
func (binding FSAOwnedFileBinding) StableName() string                    { return binding.stableName }
func (binding FSAOwnedFileBinding) TargetRef() FSAOwnedTargetRef          { return binding.target }
func (binding FSAOwnedFileBinding) Guarantees() GuaranteeSet              { return binding.guarantees }
func (binding FSAOwnedFileBinding) PolicyDigests() DirectZipPolicyDigests { return binding.policies }
func (binding FSAOwnedFileBinding) CanonicalBytes() []byte                { return clone(binding.encoded) }
func (binding FSAOwnedFileBinding) Digest() BindingDigest                 { return binding.digest }
func (binding FSAOwnedFileBinding) IsZero() bool                          { return !binding.valid() }
