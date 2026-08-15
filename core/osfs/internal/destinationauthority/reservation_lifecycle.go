package destinationauthority

import (
	"errors"
	"slices"

	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/transfer/ordinaryoutput"
	"github.com/windshare/windshare/core/transfer/receivecontract"
)

func (authority *BoundDestination) ReserveTopLevel(
	request ReservationRequest,
) (*TopLevelReservation, error) {
	if !request.valid() {
		return nil, ErrInvalidReservation
	}
	var result *TopLevelReservation
	err := authority.withGuardedRoot(func(root outputcap.Directory) error {
		reserved, err := reserveTopLevelOnRoot(
			root, authority.binding, request, authority.platform.CanonicalComponentKey,
		)
		result = reserved
		return err
	})
	if err != nil {
		if result != nil {
			err = errors.Join(err, result.Close())
		}
		return nil, err
	}
	result.authority = authority
	return result, nil
}

type topLevelReservationCandidate struct {
	canonical    receivecontract.DestinationReservation
	claim        ReservationClaim
	handle       ReservationClaimHandle
	reservedName string
}

func reserveTopLevelOnRoot(
	root outputcap.Directory,
	binding Binding,
	request ReservationRequest,
	canonicalComponentKey func(string) (string, error),
) (*TopLevelReservation, error) {
	preferredName, entryKind, ok := artifactReservationShape(request.Artifact)
	if !ok || !binding.Valid() || canonicalComponentKey == nil {
		return nil, ErrInvalidReservation
	}
	for collisionIndex := range ordinaryoutput.MaximumResultNameReservationAttemptsV1 {
		candidate, collision, err := beginTopLevelReservationCandidate(
			binding, request, preferredName, entryKind, collisionIndex, canonicalComponentKey,
		)
		if err != nil {
			return nil, err
		}
		if collision {
			continue
		}
		var reservation *TopLevelReservation
		switch entryKind {
		case receivecontract.ContainerEntrySingleFile:
			reservation, collision, err = reserveSingleFileCandidate(root, candidate)
		case receivecontract.ContainerEntryResultRoot:
			reservation, collision, err = reserveResultRootCandidate(root, candidate)
		default:
			err = ErrInvalidReservation
		}
		if err != nil {
			return nil, err
		}
		if collision {
			continue
		}
		return reservation, nil
	}
	return nil, ErrReservationExhausted
}

func beginTopLevelReservationCandidate(
	binding Binding,
	request ReservationRequest,
	preferredName string,
	entryKind receivecontract.ContainerEntryKind,
	collisionIndex uint32,
	canonicalComponentKey func(string) (string, error),
) (topLevelReservationCandidate, bool, error) {
	reservedName, err := receivecontract.CollisionName(
		request.OperationID, preferredName, collisionIndex,
		entryKind == receivecontract.ContainerEntrySingleFile,
	)
	if err != nil {
		return topLevelReservationCandidate{}, false, errors.Join(ErrInvalidReservation, err)
	}
	canonicalNameKey, err := canonicalComponentKey(reservedName)
	if err != nil || canonicalNameKey == "" {
		return topLevelReservationCandidate{}, false, errors.Join(ErrInvalidReservation, err)
	}
	handle, outcome, claimErr := request.Metadata.BeginReservation(ReservationClaimSpec{
		CanonicalNameKey: canonicalNameKey,
		OperationID:      request.OperationID, ReservationID: request.ReservationID,
		EntryKind: entryKind, RequestedName: preferredName, ReservedName: reservedName,
		CollisionIndex: collisionIndex,
	})
	if outcome == ReservationMetadataClaimCollision {
		if claimErr != nil || handle != nil {
			return topLevelReservationCandidate{}, false, errors.Join(
				ErrReservationIndeterminate, claimErr, closeReservationClaimHandle(handle),
			)
		}
		return topLevelReservationCandidate{}, true, nil
	}
	if outcome != ReservationMetadataClaimCommitted || claimErr != nil || handle == nil ||
		!handle.Claim().Valid() {
		return topLevelReservationCandidate{}, false, errors.Join(
			ErrReservationIndeterminate, claimErr, closeReservationClaimHandle(handle),
		)
	}
	canonical, err := canonicalReservation(binding, request, reservedName, collisionIndex)
	if err != nil {
		return topLevelReservationCandidate{}, false, errors.Join(err, rollbackReservationClaim(handle))
	}
	bindOutcome, bindErr := handle.BindReservation(canonical)
	if bindOutcome != ReservationMetadataClaimCommitted || bindErr != nil {
		return topLevelReservationCandidate{}, false, errors.Join(
			ErrReservationIndeterminate, bindErr, closeReservationClaimHandle(handle),
		)
	}
	claim := handle.Claim()
	if !claim.Valid() {
		return topLevelReservationCandidate{}, false, errors.Join(
			ErrReservationIndeterminate, closeReservationClaimHandle(handle),
		)
	}
	return topLevelReservationCandidate{
		canonical: canonical, claim: claim, handle: handle, reservedName: reservedName,
	}, false, nil
}

func reserveSingleFileCandidate(
	root outputcap.Directory,
	candidate topLevelReservationCandidate,
) (*TopLevelReservation, bool, error) {
	kind, exact, err := root.ClassifyExactEntry(candidate.reservedName)
	if err != nil || !exact {
		return nil, false, errors.Join(
			ErrReservationIndeterminate, err, closeReservationClaimHandle(candidate.handle),
		)
	}
	if kind != outputcap.EntryAbsent {
		if err := rollbackReservationClaim(candidate.handle); err != nil {
			return nil, false, errors.Join(ErrReservationIndeterminate, err)
		}
		return nil, true, nil
	}
	if err := candidate.handle.Close(); err != nil {
		return nil, false, errors.Join(ErrReservationIndeterminate, err)
	}
	reservation, err := newTopLevelReservation(candidate.canonical, candidate.claim, nil, nil)
	return reservation, false, err
}

func reserveResultRootCandidate(
	root outputcap.Directory,
	candidate topLevelReservationCandidate,
) (*TopLevelReservation, bool, error) {
	reserver, ok := root.(publicDirectoryReserver)
	if !ok {
		return nil, false, errors.Join(
			ErrInvalidConfiguration, errors.New("public directory reservation is unavailable"),
			rollbackReservationClaim(candidate.handle),
		)
	}
	directory, outcome, reserveErr := reserver.ReservePublicDirectoryNoReplace(candidate.reservedName)
	switch outcome {
	case outputcap.PublishNoReplaceCollision:
		if reserveErr != nil || directory != nil {
			return nil, false, errors.Join(
				ErrReservationIndeterminate, reserveErr, closeDirectory(directory),
				closeReservationClaimHandle(candidate.handle),
			)
		}
		if err := rollbackReservationClaim(candidate.handle); err != nil {
			return nil, false, errors.Join(ErrReservationIndeterminate, err)
		}
		return nil, true, nil
	case outputcap.PublishNoReplaceCommitted:
		reservation, err := commitResultRootCandidate(candidate, directory, reserveErr)
		return reservation, false, err
	case outputcap.PublishNoReplaceIndeterminate:
		// The public name may be visible. Preserve it and surface the retained
		// handle only through reconciliation, never as a committed reservation.
		return nil, false, errors.Join(
			ErrReservationIndeterminate, reserveErr, closeDirectory(directory),
			closeReservationClaimHandle(candidate.handle),
		)
	default:
		if directory != nil {
			return nil, false, errors.Join(
				ErrReservationIndeterminate, reserveErr, closeDirectory(directory),
				closeReservationClaimHandle(candidate.handle),
			)
		}
		rollbackErr := rollbackReservationClaim(candidate.handle)
		if rollbackErr != nil {
			return nil, false, errors.Join(ErrReservationIndeterminate, reserveErr, rollbackErr)
		}
		return nil, false, reserveErr
	}
}

func commitResultRootCandidate(
	candidate topLevelReservationCandidate,
	directory outputcap.Directory,
	reserveErr error,
) (*TopLevelReservation, error) {
	if reserveErr != nil || directory == nil {
		return nil, errors.Join(
			ErrReservationIndeterminate, reserveErr, closeDirectory(directory),
			closeReservationClaimHandle(candidate.handle),
		)
	}
	identityClaim, err := preparePersistentIdentityClaim(directory)
	if err != nil {
		return nil, errors.Join(
			ErrReservationIndeterminate, err, directory.Close(),
			closeReservationClaimHandle(candidate.handle),
		)
	}
	identityOutcome, identityErr := candidate.handle.BindDirectoryIdentity(slices.Clone(identityClaim))
	if identityOutcome != ReservationMetadataClaimCommitted || identityErr != nil {
		return nil, errors.Join(
			ErrReservationIndeterminate, identityErr, directory.Close(),
			closeReservationClaimHandle(candidate.handle),
		)
	}
	boundClaim := candidate.handle.Claim()
	if !boundClaim.Valid() {
		return nil, errors.Join(
			ErrReservationIndeterminate, directory.Close(),
			closeReservationClaimHandle(candidate.handle),
		)
	}
	if err := candidate.handle.Close(); err != nil {
		return nil, errors.Join(ErrReservationIndeterminate, err, directory.Close())
	}
	return newTopLevelReservation(candidate.canonical, boundClaim, identityClaim, directory)
}

func (authority *BoundDestination) ReopenTopLevel(
	expected ExpectedReservation,
) (*TopLevelReservation, error) {
	if !expected.valid() {
		return nil, ErrInvalidReservation
	}
	var result *TopLevelReservation
	err := authority.withGuardedRoot(func(root outputcap.Directory) error {
		entry, err := NewReservedEntry(expected.Reservation)
		if err != nil || expected.Reservation.AuthorityRef() != authority.binding.AuthorityRef() {
			return ErrInvalidReservation
		}
		kind, exact, err := root.ClassifyExactEntry(entry.ReservedName())
		if err != nil || !exact {
			return errors.Join(ErrReservationIndeterminate, err)
		}
		if entry.EntryKind() == receivecontract.ContainerEntrySingleFile {
			if kind != outputcap.EntryAbsent && kind != outputcap.EntryRegularFile {
				return ErrReservationCollision
			}
			// A publishing crash may leave the final present. Reopening retains
			// only the name claim; the file transaction must still prove exact
			// anchor identity before treating that entry as its publication.
			result = &TopLevelReservation{
				entry: entry, canonical: expected.Reservation, metadataClaim: expected.MetadataClaim,
			}
			return nil
		}
		if kind != outputcap.EntryDirectory {
			return ErrReservationCollision
		}
		directory, openErr := openExactPublicDirectory(root, entry.ReservedName())
		if openErr != nil {
			return openErr
		}
		claim, claimErr := readPersistentIdentityClaim(directory)
		if claimErr != nil || !slices.Equal(claim, expected.PersistentIdentityClaim) {
			return errors.Join(ErrReservationIndeterminate, claimErr, directory.Close())
		}
		result = &TopLevelReservation{
			entry: entry, canonical: expected.Reservation,
			persistentIdentityClaim: claim, directory: directory,
			metadataClaim: expected.MetadataClaim,
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	result.authority = authority
	return result, nil
}

func newTopLevelReservation(
	canonical receivecontract.DestinationReservation,
	metadataClaim ReservationClaim,
	identityClaim []byte,
	directory outputcap.Directory,
) (*TopLevelReservation, error) {
	entry, err := NewReservedEntry(canonical)
	if err != nil {
		return nil, errors.Join(err, closeDirectory(directory))
	}
	return &TopLevelReservation{
		entry: entry, canonical: canonical,
		persistentIdentityClaim: slices.Clone(identityClaim), directory: directory,
		metadataClaim: metadataClaim,
	}, nil
}

func canonicalReservation(
	binding Binding,
	request ReservationRequest,
	reservedName string,
	collisionIndex uint32,
) (receivecontract.DestinationReservation, error) {
	canonical, err := receivecontract.NewNativeNamedEntryReservation(
		request.OperationID, request.ReservationID, request.Artifact,
		binding.AuthorityRef(), reservedName, collisionIndex,
	)
	if err != nil {
		return receivecontract.DestinationReservation{}, errors.Join(ErrInvalidReservation, err)
	}
	return canonical, nil
}

func rollbackReservationClaim(handle ReservationClaimHandle) error {
	if handle == nil {
		return nil
	}
	outcome, err := handle.Rollback()
	closeErr := handle.Close()
	if outcome != ReservationMetadataClaimCommitted || err != nil || closeErr != nil {
		return errors.Join(ErrReservationIndeterminate, err, closeErr)
	}
	return nil
}

func closeReservationClaimHandle(handle ReservationClaimHandle) error {
	if handle == nil {
		return nil
	}
	return handle.Close()
}

func artifactReservationShape(
	artifact receivecontract.ArtifactSpec,
) (string, receivecontract.ContainerEntryKind, bool) {
	layout, ok := artifact.DirectoryTree()
	if !ok {
		return "", 0, false
	}
	switch layout.Kind() {
	case receivecontract.DirectoryTreeSingleFile:
		file, ok := layout.SingleFile()
		return file.SuggestedName, receivecontract.ContainerEntrySingleFile, ok
	case receivecontract.DirectoryTreeResultRoot:
		root, ok := layout.ResultRoot()
		return root.Name(), receivecontract.ContainerEntryResultRoot, ok
	default:
		return "", 0, false
	}
}

func preparePersistentIdentityClaim(directory outputcap.Directory) ([]byte, error) {
	preparer, ok := directory.(outputcap.PersistentDirectoryIdentityPreparer)
	if !ok {
		return nil, errors.Join(ErrInvalidConfiguration, errors.New("persistent directory identity enrollment is unavailable"))
	}
	claim, err := preparer.PreparePersistentDirectoryIdentityClaim()
	if err != nil || !validPersistentIdentityClaim(claim) {
		return nil, errors.Join(ErrReservationIndeterminate, err)
	}
	if err := directory.Sync(); err != nil {
		return nil, errors.Join(ErrReservationIndeterminate, err)
	}
	return slices.Clone(claim), nil
}

func readPersistentIdentityClaim(directory outputcap.Directory) ([]byte, error) {
	preparer, ok := directory.(outputcap.PersistentDirectoryIdentityPreparer)
	if !ok {
		return nil, errors.Join(
			ErrInvalidConfiguration,
			errors.New("persistent directory identity recovery is unavailable"),
		)
	}
	// A fresh native handle has no trusted in-memory identity cache. The
	// idempotent native prepare operation rehydrates the durable Object ID; the
	// stored claim comparison below remains the authority decision.
	claim, err := preparer.PreparePersistentDirectoryIdentityClaim()
	if err != nil || !validPersistentIdentityClaim(claim) {
		return nil, errors.Join(ErrReservationIndeterminate, err)
	}
	return slices.Clone(claim), nil
}

func validPersistentIdentityClaim(claim []byte) bool {
	return len(claim) > 0 && len(claim) <= outputcap.MaxRootIdentityClaimBytes
}

func openExactPublicDirectory(parent outputcap.Directory, name string) (outputcap.Directory, error) {
	reference, err := parent.OpenEntry(name)
	if err != nil || reference == nil || reference.Kind() != outputcap.EntryDirectory {
		return nil, errors.Join(ErrReservationIndeterminate, err, closeEntry(reference))
	}
	directory, openErr := parent.OpenPinnedDirectory(reference, false)
	closeErr := reference.Close()
	if openErr != nil || closeErr != nil || directory == nil {
		return nil, errors.Join(ErrReservationIndeterminate, openErr, closeErr, closeDirectory(directory))
	}
	return directory, nil
}
