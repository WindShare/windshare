package receivecontract

const materializationPlanDomain = "windshare/materialization-plan/v3"

type MaterializationPlanKind uint8

const (
	PlanDirectTree MaterializationPlanKind = iota + 1
	PlanDirectAtomic
	PlanWorkspaceThenPublish
	PlanPortableHandoff
	PlanDirectResumableZIP
)

type PreparationPolicy uint8

const (
	PreparationNone          PreparationPolicy = 0
	PreparationExactZip      PreparationPolicy = 1
	PreparationExactArtifact PreparationPolicy = 2
)

type MaterializationPlan struct {
	kind        MaterializationPlanKind
	reservation DestinationReservation
	workspace   WorkspaceBinding
	portable    PortableBinding
	ownedFile   FSAOwnedFileBinding
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

// NewWorkspaceThenPublishPlan records the publication promise in the frozen plan.
// Absence maps only to browser handoff because workspace ownership alone cannot
// confer the stronger managed-atomic publication guarantee.
func NewWorkspaceThenPublishPlan(
	artifact ArtifactSpec,
	workspace WorkspaceBinding,
	publication ...GuaranteeProfile,
) (MaterializationPlan, error) {
	if artifact.IsZero() || artifact.Kind() == ArtifactDirectoryTree || !workspace.valid() ||
		workspace.ArtifactDigest() != artifact.Digest() || len(publication) > 1 {
		return MaterializationPlan{}, ErrInvalidReceiveContract
	}
	profile := GuaranteeBrowserHandoff
	if len(publication) == 1 {
		profile = publication[0]
	}
	if profile != GuaranteeManagedAtomic && profile != GuaranteeBrowserHandoff {
		return MaterializationPlan{}, ErrInvalidReceiveContract
	}
	preparation := PreparationNone
	if artifact.Kind() == ArtifactZipArchive {
		preparation = PreparationExactZip
	}
	encoded := canonicalRecord(materializationPlanDomain,
		[]byte{byte(PlanWorkspaceThenPublish)}, frame(workspace.CanonicalBytes()),
		frame([]byte{byte(profile)}), frame([]byte{byte(preparation)}),
	)
	return MaterializationPlan{
		kind: PlanWorkspaceThenPublish, workspace: workspace, publication: profile,
		preparation: preparation, encoded: encoded,
	}, nil
}

func NewPortableHandoffPlan(artifact ArtifactSpec, portable PortableBinding) (MaterializationPlan, error) {
	if artifact.IsZero() || artifact.Kind() == ArtifactDirectoryTree || !portable.valid() ||
		portable.ArtifactDigest() != artifact.Digest() {
		return MaterializationPlan{}, ErrInvalidReceiveContract
	}
	encoded := canonicalRecord(materializationPlanDomain,
		[]byte{byte(PlanPortableHandoff)}, frame(portable.CanonicalBytes()),
		frame([]byte{byte(GuaranteeBrowserHandoff)}), frame([]byte{byte(PreparationExactArtifact)}),
	)
	return MaterializationPlan{
		kind: PlanPortableHandoff, portable: portable, publication: GuaranteeBrowserHandoff,
		preparation: PreparationExactArtifact, encoded: encoded,
	}, nil
}

func NewDirectResumableZIPPlan(artifact ArtifactSpec, binding FSAOwnedFileBinding) (MaterializationPlan, error) {
	if artifact.IsZero() || artifact.Kind() != ArtifactZipArchive || !binding.valid() ||
		binding.ArtifactDigest() != artifact.Digest() || binding.Guarantees().Profile() != GuaranteeFSAOwnedFile {
		return MaterializationPlan{}, ErrInvalidReceiveContract
	}
	encoded := canonicalRecord(materializationPlanDomain,
		[]byte{byte(PlanDirectResumableZIP)}, frame(binding.CanonicalBytes()), frame([]byte{byte(PreparationNone)}),
	)
	return MaterializationPlan{
		kind: PlanDirectResumableZIP, ownedFile: binding, publication: GuaranteeFSAOwnedFile,
		preparation: PreparationNone, encoded: encoded,
	}, nil
}

func (plan MaterializationPlan) valid() bool {
	if len(plan.encoded) == 0 {
		return false
	}
	switch plan.kind {
	case PlanDirectTree, PlanDirectAtomic:
		return plan.reservation.valid() && plan.workspace.IsZero() && plan.portable.IsZero() &&
			plan.ownedFile.IsZero() && plan.publication == plan.reservation.Guarantees().Profile() &&
			plan.preparation == PreparationNone
	case PlanWorkspaceThenPublish:
		return plan.reservation.IsZero() && plan.workspace.valid() && plan.portable.IsZero() &&
			plan.ownedFile.IsZero() &&
			(plan.publication == GuaranteeManagedAtomic || plan.publication == GuaranteeBrowserHandoff) &&
			(plan.preparation == PreparationNone || plan.preparation == PreparationExactZip)
	case PlanPortableHandoff:
		return plan.reservation.IsZero() && plan.workspace.IsZero() && plan.portable.valid() &&
			plan.ownedFile.IsZero() && plan.publication == GuaranteeBrowserHandoff &&
			plan.preparation == PreparationExactArtifact
	case PlanDirectResumableZIP:
		return plan.reservation.IsZero() && plan.workspace.IsZero() && plan.portable.IsZero() &&
			plan.ownedFile.valid() && plan.publication == GuaranteeFSAOwnedFile && plan.preparation == PreparationNone
	default:
		return false
	}
}

func (plan MaterializationPlan) Kind() MaterializationPlanKind      { return plan.kind }
func (plan MaterializationPlan) Preparation() PreparationPolicy     { return plan.preparation }
func (plan MaterializationPlan) GuaranteeProfile() GuaranteeProfile { return plan.publication }
func (plan MaterializationPlan) CanonicalBytes() []byte             { return clone(plan.encoded) }
func (plan MaterializationPlan) IsZero() bool                       { return !plan.valid() }

func (plan MaterializationPlan) OperationID() OperationID {
	switch plan.kind {
	case PlanDirectTree, PlanDirectAtomic:
		return plan.reservation.OperationID()
	case PlanWorkspaceThenPublish:
		return plan.workspace.OperationID()
	case PlanPortableHandoff:
		return plan.portable.OperationID()
	case PlanDirectResumableZIP:
		return plan.ownedFile.OperationID()
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
	case PlanDirectResumableZIP:
		return plan.ownedFile.ArtifactDigest()
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
	case PlanDirectResumableZIP:
		return plan.ownedFile.Digest()
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

func (plan MaterializationPlan) FSAOwnedFileBinding() (FSAOwnedFileBinding, bool) {
	return plan.ownedFile, plan.valid() && plan.kind == PlanDirectResumableZIP
}
