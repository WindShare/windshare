package receivecontract

import (
	"bytes"
	"encoding/binary"
	"strings"

	"github.com/windshare/windshare/core/catalog"
)

const maxCanonicalPathEncodingBytes = 8 + catalog.MaxPathDepth*8 + catalog.MaxPathBytes

// DecodeArtifactSpec reconstructs one frozen artifact through the same
// constructors used at initial intent creation. Persisted images are authority,
// so accepting a normalized or partially understood representation is unsafe.
func DecodeArtifactSpec(encoded []byte) (ArtifactSpec, error) {
	cursor, err := newContractDecoder(encoded, artifactSpecDomain)
	if err != nil {
		return ArtifactSpec{}, err
	}
	kind, err := cursor.rawByte()
	if err != nil {
		return ArtifactSpec{}, err
	}

	var artifact ArtifactSpec
	switch ArtifactKind(kind) {
	case ArtifactOriginalFile:
		fileRaw, fieldErr := cursor.fixedFrame(catalog.IdentityBytes)
		pathRaw, pathErr := cursor.frame(maxCanonicalPathEncodingBytes)
		nameRaw, nameErr := cursor.frame(MaxResultComponentBytes)
		file, fileErr := catalog.FileIDFromBytes(fileRaw)
		path, canonicalPathErr := decodeCanonicalPath(pathRaw)
		if firstDecodeError(fieldErr, pathErr, nameErr, fileErr, canonicalPathErr) != nil {
			return ArtifactSpec{}, ErrInvalidReceiveContract
		}
		artifact, err = NewOriginalFileArtifact(file, path, string(nameRaw))
	case ArtifactDirectoryTree:
		layoutRaw, fieldErr := cursor.frame(cursor.remaining())
		if fieldErr != nil {
			return ArtifactSpec{}, fieldErr
		}
		artifact, err = decodeDirectoryTreeArtifact(layoutRaw)
	case ArtifactZipArchive:
		layoutRaw, layoutFrameErr := cursor.frame(cursor.remaining())
		nameRaw, nameErr := cursor.frame(MaxResultComponentBytes)
		encoding, encodingErr := cursor.framedByte()
		completeness, completenessErr := cursor.framedByte()
		layout, layoutErr := decodeResultRootLayout(layoutRaw)
		if firstDecodeError(layoutFrameErr, nameErr, encodingErr, completenessErr, layoutErr) != nil ||
			ZipEncoding(encoding) != ZipEncodingStore ||
			ArtifactCompleteness(completeness) != ArtifactCompleteOnly {
			return ArtifactSpec{}, ErrInvalidReceiveContract
		}
		artifact, err = NewZipArchiveArtifact(layout)
		if err == nil {
			zip, ok := artifact.ZipArchive()
			if !ok || zip.SuggestedName != string(nameRaw) {
				err = ErrInvalidReceiveContract
			}
		}
	default:
		return ArtifactSpec{}, ErrInvalidReceiveContract
	}
	if err != nil || !cursor.done() || !bytes.Equal(artifact.CanonicalBytes(), encoded) {
		return ArtifactSpec{}, ErrInvalidReceiveContract
	}
	return artifact, nil
}

// DecodeMaterializationPlan validates the nested binding against artifact before
// constructing the plan. This keeps the artifact/binding digest check inside the
// single receive-contract codec authority.
func DecodeMaterializationPlan(encoded []byte, artifact ArtifactSpec) (MaterializationPlan, error) {
	if artifact.IsZero() {
		return MaterializationPlan{}, ErrInvalidReceiveContract
	}
	cursor, err := newContractDecoder(encoded, materializationPlanDomain)
	if err != nil {
		return MaterializationPlan{}, err
	}
	kind, err := cursor.rawByte()
	if err != nil {
		return MaterializationPlan{}, err
	}

	var plan MaterializationPlan
	switch MaterializationPlanKind(kind) {
	case PlanDirectTree, PlanDirectAtomic:
		bindingRaw, bindingErr := cursor.frame(cursor.remaining())
		preparation, preparationErr := cursor.framedByte()
		reservation, reservationErr := decodeDestinationReservation(bindingRaw, artifact)
		if firstDecodeError(bindingErr, preparationErr, reservationErr) != nil ||
			PreparationPolicy(preparation) != PreparationNone {
			return MaterializationPlan{}, ErrInvalidReceiveContract
		}
		if MaterializationPlanKind(kind) == PlanDirectTree {
			plan, err = NewDirectTreePlan(artifact, reservation)
		} else {
			plan, err = NewDirectAtomicPlan(artifact, reservation)
		}
	case PlanWorkspaceThenPublish:
		bindingRaw, bindingErr := cursor.frame(cursor.remaining())
		publication, publicationErr := cursor.framedByte()
		preparation, preparationErr := cursor.framedByte()
		workspace, workspaceErr := decodeWorkspaceBinding(bindingRaw, artifact)
		if firstDecodeError(bindingErr, publicationErr, preparationErr, workspaceErr) != nil {
			return MaterializationPlan{}, ErrInvalidReceiveContract
		}
		plan, err = NewWorkspaceThenPublishPlan(artifact, workspace, GuaranteeProfile(publication))
		if err == nil && plan.Preparation() != PreparationPolicy(preparation) {
			err = ErrInvalidReceiveContract
		}
	case PlanPortableHandoff:
		bindingRaw, bindingErr := cursor.frame(cursor.remaining())
		route, routeErr := cursor.framedByte()
		preparation, preparationErr := cursor.framedByte()
		portable, portableErr := decodePortableBinding(bindingRaw, artifact)
		if firstDecodeError(bindingErr, routeErr, preparationErr, portableErr) != nil ||
			GuaranteeProfile(route) != GuaranteeBrowserHandoff ||
			PreparationPolicy(preparation) != PreparationExactArtifact {
			return MaterializationPlan{}, ErrInvalidReceiveContract
		}
		plan, err = NewPortableHandoffPlan(artifact, portable)
	case PlanDirectResumableZIP:
		bindingRaw, bindingErr := cursor.frame(cursor.remaining())
		preparation, preparationErr := cursor.framedByte()
		binding, bindingErr2 := decodeFSAOwnedFileBinding(bindingRaw, artifact)
		if firstDecodeError(bindingErr, preparationErr, bindingErr2) != nil ||
			PreparationPolicy(preparation) != PreparationNone {
			return MaterializationPlan{}, ErrInvalidReceiveContract
		}
		plan, err = NewDirectResumableZIPPlan(artifact, binding)
	default:
		return MaterializationPlan{}, ErrInvalidReceiveContract
	}
	if err != nil || !cursor.done() || !bytes.Equal(plan.CanonicalBytes(), encoded) {
		return MaterializationPlan{}, ErrInvalidReceiveContract
	}
	return plan, nil
}

func decodeDirectoryTreeArtifact(encoded []byte) (ArtifactSpec, error) {
	cursor := contractDecoder{encoded: encoded}
	kind, err := cursor.rawByte()
	if err != nil {
		return ArtifactSpec{}, err
	}
	var artifact ArtifactSpec
	switch DirectoryTreeLayoutKind(kind) {
	case DirectoryTreeSingleFile:
		fileRaw, fieldErr := cursor.fixedFrame(catalog.IdentityBytes)
		pathRaw, pathErr := cursor.frame(maxCanonicalPathEncodingBytes)
		nameRaw, nameErr := cursor.frame(MaxResultComponentBytes)
		file, fileErr := catalog.FileIDFromBytes(fileRaw)
		path, canonicalPathErr := decodeCanonicalPath(pathRaw)
		if firstDecodeError(fieldErr, pathErr, nameErr, fileErr, canonicalPathErr) != nil {
			return ArtifactSpec{}, ErrInvalidReceiveContract
		}
		artifact, err = NewSingleFileDirectoryTree(file, path, string(nameRaw))
	case DirectoryTreeResultRoot:
		layoutRaw, frameErr := cursor.frame(cursor.remaining())
		layout, layoutErr := decodeResultRootLayout(layoutRaw)
		if firstDecodeError(frameErr, layoutErr) != nil {
			return ArtifactSpec{}, ErrInvalidReceiveContract
		}
		artifact, err = NewResultRootDirectoryTree(layout)
	case DirectoryTreeCatalogRoot:
		artifact = NewCatalogRootDirectoryTree()
	default:
		return ArtifactSpec{}, ErrInvalidReceiveContract
	}
	if err != nil || !cursor.done() {
		return ArtifactSpec{}, ErrInvalidReceiveContract
	}
	return artifact, nil
}

func decodeResultRootLayout(encoded []byte) (ResultRootLayout, error) {
	cursor, err := newContractDecoder(encoded, resultRootLayoutDomain)
	if err != nil {
		return ResultRootLayout{}, err
	}
	class, classErr := cursor.framedByte()
	anchorRaw, anchorErr := cursor.frame(cursor.remaining())
	nameRaw, nameErr := cursor.frame(MaxResultComponentBytes)
	if firstDecodeError(classErr, anchorErr, nameErr) != nil || !cursor.done() {
		return ResultRootLayout{}, ErrInvalidReceiveContract
	}

	anchor := contractDecoder{encoded: anchorRaw}
	anchorKind, err := anchor.rawByte()
	if err != nil {
		return ResultRootLayout{}, err
	}
	var layout ResultRootLayout
	switch ResultRootAnchorKind(anchorKind) {
	case ResultRootDirectoryAnchor:
		directoryRaw, directoryFrameErr := anchor.fixedFrame(catalog.IdentityBytes)
		pathRaw, pathFrameErr := anchor.frame(maxCanonicalPathEncodingBytes)
		directory, directoryErr := catalog.DirectoryIDFromBytes(directoryRaw)
		path, pathErr := decodeCanonicalPath(pathRaw)
		if firstDecodeError(directoryFrameErr, pathFrameErr, directoryErr, pathErr) != nil || !anchor.done() {
			return ResultRootLayout{}, ErrInvalidReceiveContract
		}
		switch ResultRootClass(class) {
		case ResultRootCompleteDirectory:
			layout, err = NewCompleteDirectoryResultRoot(directory, path)
		case ResultRootDirectorySelection:
			layout, err = NewDirectorySelectionResultRoot(directory, path)
		default:
			return ResultRootLayout{}, ErrInvalidReceiveContract
		}
	case ResultRootSyntheticAnchor:
		if ResultRootClass(class) != ResultRootSyntheticSelection || !anchor.done() {
			return ResultRootLayout{}, ErrInvalidReceiveContract
		}
		layout = NewSyntheticSelectionResultRoot()
	default:
		return ResultRootLayout{}, ErrInvalidReceiveContract
	}
	if err != nil || layout.Name() != string(nameRaw) || !bytes.Equal(layout.CanonicalBytes(), encoded) {
		return ResultRootLayout{}, ErrInvalidReceiveContract
	}
	return layout, nil
}

func decodeDestinationReservation(encoded []byte, artifact ArtifactSpec) (DestinationReservation, error) {
	cursor, err := newContractDecoder(encoded, destinationReservationDomain)
	if err != nil {
		return DestinationReservation{}, err
	}
	kind, kindErr := cursor.rawByte()
	operationRaw, operationFrameErr := cursor.fixedFrame(StableIdentityBytes)
	idRaw, idFrameErr := cursor.fixedFrame(StableIdentityBytes)
	artifactRaw, artifactFrameErr := cursor.fixedFrame(AuthorityRefBytes)
	authorityKind, authorityKindErr := cursor.framedByte()
	authorityRaw, authorityFrameErr := cursor.fixedFrame(AuthorityRefBytes)
	guaranteeRaw, guaranteeFrameErr := cursor.frame(cursor.remaining())
	operation, operationErr := OperationIDFromBytes(operationRaw)
	id, idErr := DestinationReservationIDFromBytes(idRaw)
	artifactDigest, artifactErr := ArtifactDigestFromBytes(artifactRaw)
	authority, authorityErr := AuthorityRefFromBytes(authorityRaw)
	guarantees, guaranteeErr := decodeGuaranteeSet(guaranteeRaw)
	if firstDecodeError(
		kindErr, operationFrameErr, idFrameErr, artifactFrameErr, authorityKindErr,
		authorityFrameErr, guaranteeFrameErr, operationErr, idErr, artifactErr,
		authorityErr, guaranteeErr,
	) != nil || artifactDigest != artifact.Digest() {
		return DestinationReservation{}, ErrInvalidReceiveContract
	}

	var reservation DestinationReservation
	switch DestinationReservationKind(kind) {
	case ReservationContainerRoot:
		if AuthorityKind(authorityKind) != AuthorityNativeContainer || guarantees != NativeTreeGuarantees() {
			return DestinationReservation{}, ErrInvalidReceiveContract
		}
		reservation, err = NewNativeContainerRootReservation(operation, id, artifact, authority)
	case ReservationNamedContainerEntry:
		entryKind, entryErr := cursor.framedByte()
		requestedRaw, requestedErr := cursor.frame(MaxResultComponentBytes)
		logicalReservedRaw, logicalReservedErr := cursor.frame(MaxResultComponentBytes)
		physicalRaw, physicalErr := cursor.frame(MaxResultComponentBytes)
		collisionIndex, collisionErr := cursor.framedUint32()
		if firstDecodeError(entryErr, requestedErr, logicalReservedErr, physicalErr, collisionErr) != nil {
			return DestinationReservation{}, ErrInvalidReceiveContract
		}
		switch AuthorityKind(authorityKind) {
		case AuthorityNativeContainer:
			if guarantees != NativeTreeGuarantees() {
				return DestinationReservation{}, ErrInvalidReceiveContract
			}
			reservation, err = NewNativeNamedEntryReservation(
				operation, id, artifact, authority, string(logicalReservedRaw), collisionIndex,
			)
			if err == nil && reservation.PhysicalName() != string(physicalRaw) {
				err = ErrInvalidReceiveContract
			}
		case AuthorityFSAContainer:
			if guarantees != FSATreeGuarantees() {
				return DestinationReservation{}, ErrInvalidReceiveContract
			}
			reservation, err = NewFSANamedEntryReservation(
				operation, id, artifact, authority,
				string(logicalReservedRaw), string(physicalRaw), collisionIndex,
			)
		default:
			return DestinationReservation{}, ErrInvalidReceiveContract
		}
		if err == nil && (reservation.EntryKind() != ContainerEntryKind(entryKind) ||
			reservation.RequestedName() != string(requestedRaw)) {
			err = ErrInvalidReceiveContract
		}
	case ReservationAtomicTarget:
		requestedRaw, requestedErr := cursor.frame(MaxResultComponentBytes)
		reservedRaw, reservedErr := cursor.frame(MaxResultComponentBytes)
		collisionIndex, collisionErr := cursor.framedUint32()
		if firstDecodeError(requestedErr, reservedErr, collisionErr) != nil ||
			AuthorityKind(authorityKind) != AuthorityManagedAtomicTarget ||
			guarantees.Profile() != GuaranteeManagedAtomic {
			return DestinationReservation{}, ErrInvalidReceiveContract
		}
		reservation, err = NewManagedAtomicReservation(
			operation, id, artifact, authority, guarantees.NameAuthority(),
			string(requestedRaw), string(reservedRaw), collisionIndex,
		)
	default:
		return DestinationReservation{}, ErrInvalidReceiveContract
	}
	if err != nil || !cursor.done() || !bytes.Equal(reservation.CanonicalBytes(), encoded) {
		return DestinationReservation{}, ErrInvalidReceiveContract
	}
	return reservation, nil
}

func decodeWorkspaceBinding(encoded []byte, artifact ArtifactSpec) (WorkspaceBinding, error) {
	cursor, err := newContractDecoder(encoded, workspaceBindingDomain)
	if err != nil {
		return WorkspaceBinding{}, err
	}
	operationRaw, operationFrameErr := cursor.fixedFrame(StableIdentityBytes)
	idRaw, idFrameErr := cursor.fixedFrame(StableIdentityBytes)
	artifactRaw, artifactFrameErr := cursor.fixedFrame(AuthorityRefBytes)
	repositoryRaw, repositoryFrameErr := cursor.fixedFrame(AuthorityRefBytes)
	workspaceKind, workspaceKindErr := cursor.framedByte()
	budgetPolicy, budgetErr := cursor.framedByte()
	retentionPolicy, retentionErr := cursor.framedByte()
	operation, operationErr := OperationIDFromBytes(operationRaw)
	id, idErr := WorkspaceIDFromBytes(idRaw)
	artifactDigest, artifactErr := ArtifactDigestFromBytes(artifactRaw)
	repository, repositoryErr := RepositoryRefFromBytes(repositoryRaw)
	if firstDecodeError(
		operationFrameErr, idFrameErr, artifactFrameErr, repositoryFrameErr,
		workspaceKindErr, budgetErr, retentionErr, operationErr, idErr,
		artifactErr, repositoryErr,
	) != nil || artifactDigest != artifact.Digest() ||
		WorkspaceKind(workspaceKind) != WorkspaceOriginPrivate ||
		WorkspaceBudgetPolicy(budgetPolicy) != WorkspaceBudgetV1 ||
		WorkspaceRetentionPolicy(retentionPolicy) != WorkspaceStable24HourV1 {
		return WorkspaceBinding{}, ErrInvalidReceiveContract
	}
	binding, err := NewWorkspaceBinding(operation, id, artifact, repository)
	if err != nil || !cursor.done() || !bytes.Equal(binding.CanonicalBytes(), encoded) {
		return WorkspaceBinding{}, ErrInvalidReceiveContract
	}
	return binding, nil
}

func decodePortableBinding(encoded []byte, artifact ArtifactSpec) (PortableBinding, error) {
	cursor, err := newContractDecoder(encoded, portableBindingDomain)
	if err != nil {
		return PortableBinding{}, err
	}
	operationRaw, operationFrameErr := cursor.fixedFrame(StableIdentityBytes)
	idRaw, idFrameErr := cursor.fixedFrame(StableIdentityBytes)
	artifactRaw, artifactFrameErr := cursor.fixedFrame(AuthorityRefBytes)
	maximumArtifactBytes, maximumErr := cursor.framedUint64()
	assemblyPartBytes, assemblyErr := cursor.framedUint64()
	maximumParts, partsErr := cursor.framedUint64()
	leaseMilliseconds, leaseErr := cursor.framedUint64()
	preparation, preparationErr := cursor.framedByte()
	operation, operationErr := OperationIDFromBytes(operationRaw)
	id, idErr := PortablePlanIDFromBytes(idRaw)
	artifactDigest, artifactErr := ArtifactDigestFromBytes(artifactRaw)
	if firstDecodeError(
		operationFrameErr, idFrameErr, artifactFrameErr, maximumErr, assemblyErr,
		partsErr, leaseErr, preparationErr, operationErr, idErr, artifactErr,
	) != nil || artifactDigest != artifact.Digest() ||
		maximumArtifactBytes != DefaultPortableArtifactLimit ||
		assemblyPartBytes != DefaultPortableAssemblyPartBytes ||
		maximumParts != DefaultPortableMaximumParts ||
		leaseMilliseconds != BrowserHandoffObjectURLLeaseMillis ||
		PreparationPolicy(preparation) != PreparationExactArtifact {
		return PortableBinding{}, ErrInvalidReceiveContract
	}
	binding, err := NewPortableBinding(operation, id, artifact)
	if err != nil || !cursor.done() || !bytes.Equal(binding.CanonicalBytes(), encoded) {
		return PortableBinding{}, ErrInvalidReceiveContract
	}
	return binding, nil
}

func decodeFSAOwnedFileBinding(encoded []byte, artifact ArtifactSpec) (FSAOwnedFileBinding, error) {
	cursor, err := newContractDecoder(encoded, fsaOwnedFileBindingDomain)
	if err != nil {
		return FSAOwnedFileBinding{}, err
	}
	operationRaw, operationFrameErr := cursor.fixedFrame(StableIdentityBytes)
	artifactRaw, artifactFrameErr := cursor.fixedFrame(AuthorityRefBytes)
	nameRaw, nameErr := cursor.frame(MaxResultComponentBytes)
	targetRaw, targetFrameErr := cursor.fixedFrame(AuthorityRefBytes)
	guaranteeRaw, guaranteeFrameErr := cursor.frame(cursor.remaining())
	encodingRaw, encodingErr := cursor.fixedFrame(AuthorityRefBytes)
	layoutRaw, layoutErr := cursor.fixedFrame(AuthorityRefBytes)
	checkpointRaw, checkpointErr := cursor.fixedFrame(AuthorityRefBytes)
	journalRaw, journalErr := cursor.fixedFrame(AuthorityRefBytes)
	epochRaw, epochErr := cursor.fixedFrame(AuthorityRefBytes)
	operation, operationErr := OperationIDFromBytes(operationRaw)
	artifactDigest, artifactErr := ArtifactDigestFromBytes(artifactRaw)
	target, targetErr := FSAOwnedTargetRefFromBytes(targetRaw)
	guarantees, guaranteesErr := decodeGuaranteeSet(guaranteeRaw)
	encoding, encodingDigestErr := PolicyDigestFromBytes(encodingRaw)
	layout, layoutDigestErr := PolicyDigestFromBytes(layoutRaw)
	checkpoint, checkpointDigestErr := PolicyDigestFromBytes(checkpointRaw)
	journal, journalDigestErr := PolicyDigestFromBytes(journalRaw)
	epoch, epochDigestErr := PolicyDigestFromBytes(epochRaw)
	if firstDecodeError(
		operationFrameErr, artifactFrameErr, nameErr, targetFrameErr, guaranteeFrameErr,
		encodingErr, layoutErr, checkpointErr, journalErr, epochErr,
		operationErr, artifactErr, targetErr, guaranteesErr, encodingDigestErr,
		layoutDigestErr, checkpointDigestErr, journalDigestErr, epochDigestErr,
	) != nil || artifactDigest != artifact.Digest() || guarantees != FSAOwnedFileGuarantees() {
		return FSAOwnedFileBinding{}, ErrInvalidReceiveContract
	}
	binding, err := NewFSAOwnedFileBinding(operation, artifact, string(nameRaw), target, DirectZipPolicyDigests{
		ZipEncoding: encoding, Layout: layout, Checkpoint: checkpoint, JournalBudget: journal, Epoch: epoch,
	})
	if err != nil || !cursor.done() || !bytes.Equal(binding.CanonicalBytes(), encoded) {
		return FSAOwnedFileBinding{}, ErrInvalidReceiveContract
	}
	return binding, nil
}

func decodeGuaranteeSet(encoded []byte) (GuaranteeSet, error) {
	candidates := []GuaranteeSet{
		NativeTreeGuarantees(), FSATreeGuarantees(), BrowserHandoffGuarantees(), FSAOwnedFileGuarantees(),
	}
	for _, name := range []NameAuthority{NameApplicationChosen, NameUserChosen} {
		managed, err := ManagedAtomicGuarantees(name)
		if err != nil {
			return GuaranteeSet{}, ErrInvalidReceiveContract
		}
		candidates = append(candidates, managed)
	}
	for _, candidate := range candidates {
		if bytes.Equal(candidate.canonicalBytes(), encoded) {
			return candidate, nil
		}
	}
	return GuaranteeSet{}, ErrInvalidReceiveContract
}

func decodeCanonicalPath(encoded []byte) (string, error) {
	cursor := contractDecoder{encoded: encoded}
	count, err := cursor.rawUint64()
	if err != nil || count == 0 || count > catalog.MaxPathDepth {
		return "", ErrInvalidReceiveContract
	}
	segments := make([]string, 0, int(count))
	totalBytes := uint64(0)
	for range count {
		segmentRaw, frameErr := cursor.frame(catalog.MaxNameBytes)
		if frameErr != nil {
			return "", frameErr
		}
		if len(segments) != 0 {
			totalBytes++
		}
		totalBytes += uint64(len(segmentRaw))
		if totalBytes > catalog.MaxPathBytes {
			return "", ErrInvalidReceiveContract
		}
		segments = append(segments, string(segmentRaw))
	}
	path := strings.Join(segments, "/")
	canonical, canonicalErr := catalog.CanonicalPath(path)
	if canonicalErr != nil || canonical != path || !cursor.done() ||
		!bytes.Equal(canonicalPathBytes(path), encoded) {
		return "", ErrInvalidReceiveContract
	}
	return path, nil
}

type contractDecoder struct {
	encoded []byte
	offset  int
}

func newContractDecoder(encoded []byte, domain string) (contractDecoder, error) {
	prefix := append(append([]byte(nil), domain...), 0, schemaVersion)
	if !bytes.HasPrefix(encoded, prefix) {
		return contractDecoder{}, ErrInvalidReceiveContract
	}
	return contractDecoder{encoded: encoded, offset: len(prefix)}, nil
}

func (cursor *contractDecoder) remaining() int { return len(cursor.encoded) - cursor.offset }
func (cursor *contractDecoder) done() bool     { return cursor.offset == len(cursor.encoded) }

func (cursor *contractDecoder) rawByte() (byte, error) {
	if cursor.remaining() < 1 {
		return 0, ErrInvalidReceiveContract
	}
	value := cursor.encoded[cursor.offset]
	cursor.offset++
	return value, nil
}

func (cursor *contractDecoder) rawUint64() (uint64, error) {
	if cursor.remaining() < 8 {
		return 0, ErrInvalidReceiveContract
	}
	value := binary.BigEndian.Uint64(cursor.encoded[cursor.offset : cursor.offset+8])
	cursor.offset += 8
	return value, nil
}

func (cursor *contractDecoder) frame(maximum int) ([]byte, error) {
	length, err := cursor.rawUint64()
	if err != nil || maximum < 0 || length > uint64(maximum) || length > uint64(cursor.remaining()) {
		return nil, ErrInvalidReceiveContract
	}
	value := cursor.encoded[cursor.offset : cursor.offset+int(length)]
	cursor.offset += int(length)
	return value, nil
}

func (cursor *contractDecoder) fixedFrame(size int) ([]byte, error) {
	value, err := cursor.frame(size)
	if err != nil || len(value) != size {
		return nil, ErrInvalidReceiveContract
	}
	return value, nil
}

func (cursor *contractDecoder) framedByte() (byte, error) {
	value, err := cursor.fixedFrame(1)
	if err != nil {
		return 0, err
	}
	return value[0], nil
}

func (cursor *contractDecoder) framedUint32() (uint32, error) {
	value, err := cursor.fixedFrame(4)
	if err != nil {
		return 0, err
	}
	return binary.BigEndian.Uint32(value), nil
}

func (cursor *contractDecoder) framedUint64() (uint64, error) {
	value, err := cursor.fixedFrame(8)
	if err != nil {
		return 0, err
	}
	return binary.BigEndian.Uint64(value), nil
}

func firstDecodeError(errors ...error) error {
	for _, err := range errors {
		if err != nil {
			return err
		}
	}
	return nil
}
