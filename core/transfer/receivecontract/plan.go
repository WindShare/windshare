package receivecontract

const materializationPlanDomain = "windshare/materialization-plan/v1"

type MaterializationPlanKind uint8

const (
	PlanDirectTree MaterializationPlanKind = iota + 1
	PlanDirectAtomic
	PlanWorkspaceThenPublish
	PlanPortableHandoff
)

type PreparationPolicy uint8

const (
	PreparationNone          PreparationPolicy = 0
	PreparationExactZip      PreparationPolicy = 1
	PreparationExactArtifact PreparationPolicy = 2
)

type PublicationRoute uint8

const (
	PublicationManagedAtomic PublicationRoute = iota + 1
	PublicationBrowserHandoff
)

type MaterializationPlan struct {
	kind        MaterializationPlanKind
	reservation DestinationReservation
	workspace   WorkspaceBinding
	portable    PortableBinding
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
	return directPlan(PlanDirectTree, reservation)
}

func NewDirectAtomicPlan(artifact ArtifactSpec, reservation DestinationReservation) (MaterializationPlan, error) {
	if artifact.IsZero() || artifact.Kind() == ArtifactDirectoryTree || !reservation.valid() ||
		reservation.Kind() != ReservationAtomicTarget || reservation.ArtifactDigest() != artifact.Digest() ||
		reservation.Guarantees().Profile() != GuaranteeManagedAtomic {
		return MaterializationPlan{}, ErrInvalidReceiveContract
	}
	return directPlan(PlanDirectAtomic, reservation)
}

func directPlan(kind MaterializationPlanKind, reservation DestinationReservation) (MaterializationPlan, error) {
	encoded := canonicalRecord(materializationPlanDomain,
		[]byte{byte(kind)}, frame(reservation.CanonicalBytes()), frame([]byte{byte(PreparationNone)}),
	)
	return MaterializationPlan{
		kind: kind, reservation: reservation, preparation: PreparationNone, encoded: encoded,
	}, nil
}

func NewWorkspaceThenPublishPlan(artifact ArtifactSpec, workspace WorkspaceBinding) (MaterializationPlan, error) {
	if artifact.IsZero() || artifact.Kind() == ArtifactDirectoryTree || !workspace.valid() ||
		workspace.ArtifactDigest() != artifact.Digest() {
		return MaterializationPlan{}, ErrInvalidReceiveContract
	}
	preparation := PreparationNone
	if artifact.Kind() == ArtifactZipArchive {
		preparation = PreparationExactZip
	}
	encoded := canonicalRecord(materializationPlanDomain,
		[]byte{byte(PlanWorkspaceThenPublish)},
		frame(workspace.CanonicalBytes()), frame([]byte{byte(preparation)}),
	)
	return MaterializationPlan{
		kind: PlanWorkspaceThenPublish, workspace: workspace,
		preparation: preparation, encoded: encoded,
	}, nil
}

func NewPortableHandoffPlan(artifact ArtifactSpec, portable PortableBinding) (MaterializationPlan, error) {
	if artifact.IsZero() || artifact.Kind() == ArtifactDirectoryTree || !portable.valid() ||
		portable.ArtifactDigest() != artifact.Digest() {
		return MaterializationPlan{}, ErrInvalidReceiveContract
	}
	encoded := canonicalRecord(materializationPlanDomain,
		[]byte{byte(PlanPortableHandoff)},
		frame(portable.CanonicalBytes()),
		frame([]byte{byte(PublicationBrowserHandoff)}),
		frame([]byte{byte(PreparationExactArtifact)}),
	)
	return MaterializationPlan{
		kind: PlanPortableHandoff, portable: portable,
		preparation: PreparationExactArtifact, encoded: encoded,
	}, nil
}

func (plan MaterializationPlan) valid() bool {
	switch plan.kind {
	case PlanDirectTree, PlanDirectAtomic:
		return plan.reservation.valid() && plan.workspace.IsZero() && plan.portable.IsZero() &&
			plan.preparation == PreparationNone && len(plan.encoded) != 0
	case PlanWorkspaceThenPublish:
		return plan.reservation.IsZero() && plan.workspace.valid() && plan.portable.IsZero() &&
			(plan.preparation == PreparationNone || plan.preparation == PreparationExactZip) && len(plan.encoded) != 0
	case PlanPortableHandoff:
		return plan.reservation.IsZero() && plan.workspace.IsZero() && plan.portable.valid() &&
			plan.preparation == PreparationExactArtifact && len(plan.encoded) != 0
	default:
		return false
	}
}

func (plan MaterializationPlan) Kind() MaterializationPlanKind  { return plan.kind }
func (plan MaterializationPlan) Preparation() PreparationPolicy { return plan.preparation }
func (plan MaterializationPlan) CanonicalBytes() []byte         { return clone(plan.encoded) }
func (plan MaterializationPlan) IsZero() bool                   { return !plan.valid() }

func (plan MaterializationPlan) OperationID() OperationID {
	switch plan.kind {
	case PlanDirectTree, PlanDirectAtomic:
		return plan.reservation.OperationID()
	case PlanWorkspaceThenPublish:
		return plan.workspace.OperationID()
	case PlanPortableHandoff:
		return plan.portable.OperationID()
	default:
		return OperationID{}
	}
}

func (plan MaterializationPlan) ArtifactDigest() ArtifactDigest {
	switch plan.kind {
	case PlanDirectTree, PlanDirectAtomic:
		return plan.reservation.ArtifactDigest()
	case PlanWorkspaceThenPublish:
		return plan.workspace.ArtifactDigest()
	case PlanPortableHandoff:
		return plan.portable.ArtifactDigest()
	default:
		return ArtifactDigest{}
	}
}

func (plan MaterializationPlan) BindingDigest() BindingDigest {
	switch plan.kind {
	case PlanDirectTree, PlanDirectAtomic:
		return plan.reservation.Digest()
	case PlanWorkspaceThenPublish:
		return plan.workspace.Digest()
	case PlanPortableHandoff:
		return plan.portable.Digest()
	default:
		return BindingDigest{}
	}
}

func (plan MaterializationPlan) DestinationReservation() (DestinationReservation, bool) {
	return plan.reservation, plan.valid() && (plan.kind == PlanDirectTree || plan.kind == PlanDirectAtomic)
}

func (plan MaterializationPlan) WorkspaceBinding() (WorkspaceBinding, bool) {
	return plan.workspace, plan.valid() && plan.kind == PlanWorkspaceThenPublish
}

func (plan MaterializationPlan) PortableBinding() (PortableBinding, bool) {
	return plan.portable, plan.valid() && plan.kind == PlanPortableHandoff
}
