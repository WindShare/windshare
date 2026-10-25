package receivecontract

const materializationPlanDomain = "windshare/materialization-plan/v4"

type MaterializationPlanKind uint8

const (
	PlanDirectTree MaterializationPlanKind = iota + 1
	PlanDirectAtomic
)

type PreparationPolicy uint8

const (
	PreparationNone PreparationPolicy = 0
)

type MaterializationPlan struct {
	kind        MaterializationPlanKind
	reservation DestinationReservation
	publication GuaranteeProfile
	preparation PreparationPolicy
	encoded     []byte
}

func NewDirectTreePlan(artifact ArtifactSpec, reservation DestinationReservation) (MaterializationPlan, error) {
	layout, ok := artifact.DirectoryTree()
	if !ok || !reservation.valid() || reservation.ArtifactDigest() != artifact.Digest() {
		return MaterializationPlan{}, ErrInvalidReceiveContract
	}
	switch layout.Kind() {
	case DirectoryTreeCatalogRoot:
		if reservation.Kind() != ReservationContainerRoot || reservation.Guarantees().Profile() != GuaranteeNativeTree {
			return MaterializationPlan{}, ErrInvalidReceiveContract
		}
	case DirectoryTreeSingleFile:
		if reservation.Kind() != ReservationNamedContainerEntry || reservation.EntryKind() != ContainerEntrySingleFile {
			return MaterializationPlan{}, ErrInvalidReceiveContract
		}
	case DirectoryTreeResultRoot:
		if reservation.Kind() != ReservationNamedContainerEntry || reservation.EntryKind() != ContainerEntryResultRoot {
			return MaterializationPlan{}, ErrInvalidReceiveContract
		}
	default:
		return MaterializationPlan{}, ErrInvalidReceiveContract
	}
	return directReservationPlan(PlanDirectTree, reservation)
}

func NewDirectAtomicPlan(artifact ArtifactSpec, reservation DestinationReservation) (MaterializationPlan, error) {
	if artifact.IsZero() || artifact.Kind() != ArtifactOriginalFile || !reservation.valid() ||
		reservation.Kind() != ReservationAtomicTarget || reservation.ArtifactDigest() != artifact.Digest() ||
		reservation.Guarantees().Profile() != GuaranteeManagedAtomic {
		return MaterializationPlan{}, ErrInvalidReceiveContract
	}
	return directReservationPlan(PlanDirectAtomic, reservation)
}

func directReservationPlan(kind MaterializationPlanKind, reservation DestinationReservation) (MaterializationPlan, error) {
	encoded := canonicalRecord(materializationPlanDomain,
		[]byte{byte(kind)}, frame(reservation.CanonicalBytes()), frame([]byte{byte(PreparationNone)}),
	)
	return MaterializationPlan{
		kind: kind, reservation: reservation, publication: reservation.Guarantees().Profile(),
		preparation: PreparationNone, encoded: encoded,
	}, nil
}

func (plan MaterializationPlan) Kind() MaterializationPlanKind      { return plan.kind }
func (plan MaterializationPlan) Preparation() PreparationPolicy     { return plan.preparation }
func (plan MaterializationPlan) GuaranteeProfile() GuaranteeProfile { return plan.publication }
func (plan MaterializationPlan) CanonicalBytes() []byte             { return clone(plan.encoded) }
func (plan MaterializationPlan) IsZero() bool                       { return !plan.valid() }

func (plan MaterializationPlan) DestinationReservation() (DestinationReservation, bool) {
	return plan.reservation, plan.valid() && (plan.kind == PlanDirectTree || plan.kind == PlanDirectAtomic)
}

// valid checks only contracts the native receiver can execute. Browser bindings
// and their persistence codecs belong to the browser runtime.
func (plan MaterializationPlan) valid() bool {
	return len(plan.encoded) != 0 &&
		(plan.kind == PlanDirectTree || plan.kind == PlanDirectAtomic) &&
		plan.reservation.valid() &&
		plan.publication == plan.reservation.Guarantees().Profile() &&
		plan.preparation == PreparationNone
}

func (plan MaterializationPlan) OperationID() OperationID { return plan.reservation.OperationID() }

func (plan MaterializationPlan) ArtifactDigest() ArtifactDigest {
	return plan.reservation.ArtifactDigest()
}

func (plan MaterializationPlan) BindingDigest() BindingDigest { return plan.reservation.Digest() }
