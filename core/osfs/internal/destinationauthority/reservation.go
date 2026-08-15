package destinationauthority

import (
	"crypto/sha256"
	"errors"
	"slices"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/transfer/ordinaryoutput"
	"github.com/windshare/windshare/core/transfer/receivecontract"
)

var (
	ErrInvalidReservation       = errors.New("destination authority reservation is invalid")
	ErrReservationCollision     = errors.New("destination authority reservation collides with an existing entry")
	ErrReservationExhausted     = errors.New("destination authority reservation attempts are exhausted")
	ErrReservationIndeterminate = errors.New("destination authority reservation outcome is indeterminate")
)

type ReservationMetadataClaimOutcome uint8

const (
	ReservationMetadataClaimCommitted ReservationMetadataClaimOutcome = iota + 1
	ReservationMetadataClaimCollision
	ReservationMetadataClaimIndeterminate
)

func (outcome ReservationMetadataClaimOutcome) valid() bool {
	return outcome >= ReservationMetadataClaimCommitted && outcome <= ReservationMetadataClaimIndeterminate
}

// ReservationMetadataClaim is operation-scoped root-owned metadata. The native
// directory identity is supplied only after direct public creation; it never
// enters the canonical receive intent or becomes path authority.
type ReservationClaimSpec struct {
	CanonicalNameKey string
	OperationID      receivecontract.OperationID
	ReservationID    receivecontract.DestinationReservationID
	EntryKind        receivecontract.ContainerEntryKind
	RequestedName    string
	ReservedName     string
	CollisionIndex   uint32
}

func (spec ReservationClaimSpec) Valid() bool {
	if spec.CanonicalNameKey == "" || spec.OperationID.IsZero() || spec.ReservationID.IsZero() ||
		spec.RequestedName == "" || spec.ReservedName == "" ||
		spec.CollisionIndex >= ordinaryoutput.MaximumResultNameReservationAttemptsV1 ||
		(spec.EntryKind != receivecontract.ContainerEntrySingleFile &&
			spec.EntryKind != receivecontract.ContainerEntryResultRoot) {
		return false
	}
	expected, err := receivecontract.CollisionName(
		spec.OperationID, spec.RequestedName, spec.CollisionIndex,
		spec.EntryKind == receivecontract.ContainerEntrySingleFile,
	)
	return err == nil && expected == spec.ReservedName
}

type ReservationMetadataClaimer interface {
	BeginReservation(ReservationClaimSpec) (ReservationClaimHandle, ReservationMetadataClaimOutcome, error)
}

type ReservationClaimToken [sha256.Size]byte

func (token ReservationClaimToken) IsZero() bool { return token == ReservationClaimToken{} }

type ReservationClaim struct {
	Token      ReservationClaimToken
	Generation uint64
}

func (claim ReservationClaim) Valid() bool { return !claim.Token.IsZero() && claim.Generation > 0 }

type ReservationClaimHandle interface {
	Claim() ReservationClaim
	BindReservation(receivecontract.DestinationReservation) (ReservationMetadataClaimOutcome, error)
	BindDirectoryIdentity([]byte) (ReservationMetadataClaimOutcome, error)
	Rollback() (ReservationMetadataClaimOutcome, error)
	Close() error
}

// ReservedEntry is a physical alias for the first logical artifact component.
// Descendants remain logical artifact paths; this value never reinterprets a
// source path or creates a second naming authority.
type ReservedEntry struct {
	preferredName  string
	reservedName   string
	collisionIndex uint32
	kind           receivecontract.ContainerEntryKind
}

func NewReservedEntry(reservation receivecontract.DestinationReservation) (ReservedEntry, error) {
	if reservation.IsZero() || reservation.Kind() != receivecontract.ReservationNamedContainerEntry ||
		reservation.AuthorityKind() != receivecontract.AuthorityNativeContainer ||
		(reservation.EntryKind() != receivecontract.ContainerEntrySingleFile &&
			reservation.EntryKind() != receivecontract.ContainerEntryResultRoot) ||
		reservation.RequestedName() == "" || reservation.ReservedName() == "" {
		return ReservedEntry{}, ErrInvalidReservation
	}
	expected, err := receivecontract.CollisionName(
		reservation.OperationID(), reservation.RequestedName(), reservation.CollisionIndex(),
		reservation.EntryKind() == receivecontract.ContainerEntrySingleFile,
	)
	if err != nil || expected != reservation.ReservedName() ||
		reservation.CollisionIndex() >= ordinaryoutput.MaximumResultNameReservationAttemptsV1 {
		return ReservedEntry{}, ErrInvalidReservation
	}
	return ReservedEntry{
		preferredName: reservation.RequestedName(), reservedName: reservation.ReservedName(),
		collisionIndex: reservation.CollisionIndex(), kind: reservation.EntryKind(),
	}, nil
}

func (entry ReservedEntry) PreferredName() string                         { return entry.preferredName }
func (entry ReservedEntry) ReservedName() string                          { return entry.reservedName }
func (entry ReservedEntry) CollisionIndex() uint32                        { return entry.collisionIndex }
func (entry ReservedEntry) EntryKind() receivecontract.ContainerEntryKind { return entry.kind }
func (entry ReservedEntry) Valid() bool {
	return entry.preferredName != "" && entry.reservedName != "" &&
		(entry.kind == receivecontract.ContainerEntrySingleFile ||
			entry.kind == receivecontract.ContainerEntryResultRoot)
}

// ReservationRequest carries canonical receive construction inputs. The
// authority derives every candidate through CollisionName; callers cannot feed
// it a physical leaf or a path-shaped authority token.
type ReservationRequest struct {
	OperationID   receivecontract.OperationID
	ReservationID receivecontract.DestinationReservationID
	Artifact      receivecontract.ArtifactSpec
	Metadata      ReservationMetadataClaimer
}

func (request ReservationRequest) valid() bool {
	if request.OperationID.IsZero() || request.ReservationID.IsZero() || request.Artifact.IsZero() || request.Metadata == nil {
		return false
	}
	_, _, ok := artifactReservationShape(request.Artifact)
	return ok
}

// ExpectedReservation is the exact recovery proof for one already-frozen
// reservation. Directory identity is mandatory only for an installed result
// root; single-file recovery still requires the public name to be absent.
type ExpectedReservation struct {
	Reservation             receivecontract.DestinationReservation
	PersistentIdentityClaim []byte
	MetadataClaim           ReservationClaim
}

func (expected ExpectedReservation) valid() bool {
	entry, err := NewReservedEntry(expected.Reservation)
	if err != nil {
		return false
	}
	if !expected.MetadataClaim.Valid() ||
		entry.CollisionIndex() >= ordinaryoutput.MaximumResultNameReservationAttemptsV1 {
		return false
	}
	if entry.EntryKind() == receivecontract.ContainerEntrySingleFile {
		return len(expected.PersistentIdentityClaim) == 0
	}
	return validPersistentIdentityClaim(expected.PersistentIdentityClaim)
}

// TopLevelReservation owns the retained public result-root handle when a
// directory was installed. A single-file reservation deliberately owns none:
// its final name stays absent until the one no-replace publication.
type TopLevelReservation struct {
	entry                   ReservedEntry
	canonical               receivecontract.DestinationReservation
	persistentIdentityClaim []byte
	directory               outputcap.Directory
	metadataClaim           ReservationClaim
	authority               *BoundDestination
}

func (reservation *TopLevelReservation) ReservedEntry() ReservedEntry {
	if reservation == nil {
		return ReservedEntry{}
	}
	return reservation.entry
}

func (reservation *TopLevelReservation) CanonicalReservation() receivecontract.DestinationReservation {
	if reservation == nil {
		return receivecontract.DestinationReservation{}
	}
	return reservation.canonical
}

func (reservation *TopLevelReservation) PersistentIdentityClaim() []byte {
	if reservation == nil {
		return nil
	}
	return slices.Clone(reservation.persistentIdentityClaim)
}

func (reservation *TopLevelReservation) MetadataClaim() ReservationClaim {
	if reservation == nil {
		return ReservationClaim{}
	}
	return reservation.metadataClaim
}

// BorrowResultRoot gives one composition callback the retained exact result
// root. The handle remains owned by the reservation and cannot escape the call.
func (reservation *TopLevelReservation) BorrowResultRoot(
	visit func(outputcap.Directory) error,
) error {
	if reservation == nil || reservation.directory == nil ||
		reservation.entry.EntryKind() != receivecontract.ContainerEntryResultRoot || visit == nil {
		return ErrInvalidReservation
	}
	return visit(reservation.directory)
}

// AcquirePublicOperationGuard composes the frozen top-level reservation as one
// execution root. A result root yields only a duplicate of its retained exact
// handle; a single file yields the freshly revalidated bound container guard.
func (reservation *TopLevelReservation) AcquirePublicOperationGuard() (
	outputcap.PublicOperationGuard,
	error,
) {
	if reservation == nil || reservation.authority == nil || !reservation.entry.Valid() {
		return nil, ErrInvalidReservation
	}
	authority := reservation.authority
	authority.mu.RLock()
	if authority.closed || authority.platform == nil || authority.rootWitness == nil {
		authority.mu.RUnlock()
		return nil, ErrAuthorityClosed
	}
	switch reservation.entry.EntryKind() {
	case receivecontract.ContainerEntryResultRoot:
		return reservation.acquireResultRootGuardLocked(authority)
	case receivecontract.ContainerEntrySingleFile:
		return reservation.acquireSingleFileGuardLocked(authority)
	default:
		authority.mu.RUnlock()
		return nil, ErrInvalidReservation
	}
}

func (reservation *TopLevelReservation) acquireResultRootGuardLocked(
	authority *BoundDestination,
) (outputcap.PublicOperationGuard, error) {
	if reservation.directory == nil {
		authority.mu.RUnlock()
		return nil, ErrInvalidReservation
	}
	containerGuard, container, err := acquireBoundContainerGuardLocked(authority)
	if err != nil {
		return nil, err
	}
	root, openErr := openExactPublicDirectory(container, reservation.entry.ReservedName())
	if openErr == nil && root != nil {
		var same bool
		same, openErr = root.SameDirectory(reservation.directory)
		if openErr == nil && !same {
			openErr = ErrRetainedRootChanged
		}
	}
	if openErr != nil || root == nil {
		closeErr := errors.Join(closeDirectory(root), containerGuard.Close())
		authority.mu.RUnlock()
		return nil, errors.Join(ErrRetainedRootChanged, openErr, closeErr)
	}
	return &reservationExecutionGuard{
		root: root,
		close: func() error {
			defer authority.mu.RUnlock()
			return errors.Join(root.Close(), containerGuard.Close())
		},
	}, nil
}

func (reservation *TopLevelReservation) acquireSingleFileGuardLocked(
	authority *BoundDestination,
) (outputcap.PublicOperationGuard, error) {
	guard, root, err := acquireBoundContainerGuardLocked(authority)
	if err != nil {
		return nil, err
	}
	return &reservationExecutionGuard{
		root: root,
		close: func() error {
			defer authority.mu.RUnlock()
			return guard.Close()
		},
	}, nil
}

func acquireBoundContainerGuardLocked(
	authority *BoundDestination,
) (outputcap.PublicOperationGuard, outputcap.Directory, error) {
	guard, err := authority.platform.AcquirePublicOperationGuard()
	if err != nil || guard == nil {
		authority.mu.RUnlock()
		return nil, nil, errors.Join(ErrRetainedRootChanged, err)
	}
	root := guard.Root()
	same := false
	if root != nil {
		same, err = root.SameDirectory(authority.rootWitness)
	}
	if err != nil || !same {
		closeErr := guard.Close()
		authority.mu.RUnlock()
		return nil, nil, errors.Join(ErrRetainedRootChanged, err, closeErr)
	}
	return guard, root, nil
}

func (reservation *TopLevelReservation) RootOpenDisposition() outputcap.RootOpenDisposition {
	if reservation != nil && reservation.entry.EntryKind() == receivecontract.ContainerEntryResultRoot {
		return outputcap.AuthorityCreatedRoot
	}
	if reservation != nil && reservation.entry.EntryKind() == receivecontract.ContainerEntrySingleFile {
		return outputcap.CallerProvidedContainer
	}
	return ""
}

func (reservation *TopLevelReservation) ValidateModifiedTime(modified catalog.ModifiedTime) error {
	authority, err := reservation.executionAuthority()
	if err != nil {
		return err
	}
	defer authority.mu.RUnlock()
	return authority.platform.ValidateModifiedTime(modified)
}

func (reservation *TopLevelReservation) CanonicalLocatorKey(path string) (string, error) {
	authority, err := reservation.executionAuthority()
	if err != nil {
		return "", err
	}
	defer authority.mu.RUnlock()
	return authority.platform.CanonicalLocatorKey(path)
}

func (reservation *TopLevelReservation) CanonicalComponentKey(component string) (string, error) {
	authority, err := reservation.executionAuthority()
	if err != nil {
		return "", err
	}
	defer authority.mu.RUnlock()
	return authority.platform.CanonicalComponentKey(component)
}

func (reservation *TopLevelReservation) executionAuthority() (*BoundDestination, error) {
	if reservation == nil || reservation.authority == nil || !reservation.entry.Valid() {
		return nil, ErrInvalidReservation
	}
	authority := reservation.authority
	authority.mu.RLock()
	if authority.closed || authority.platform == nil {
		authority.mu.RUnlock()
		return nil, ErrAuthorityClosed
	}
	return authority, nil
}

type reservationExecutionGuard struct {
	root  outputcap.Directory
	close func() error
}

func (guard *reservationExecutionGuard) Root() outputcap.Directory {
	if guard == nil {
		return nil
	}
	return guard.root
}

func (guard *reservationExecutionGuard) Close() error {
	if guard == nil || guard.close == nil {
		return nil
	}
	closeOperation := guard.close
	guard.close = nil
	guard.root = nil
	return closeOperation()
}

func (reservation *TopLevelReservation) Close() error {
	if reservation == nil {
		return nil
	}
	var err error
	if reservation.directory != nil {
		err = reservation.directory.Close()
		reservation.directory = nil
	}
	reservation.authority = nil
	return err
}
