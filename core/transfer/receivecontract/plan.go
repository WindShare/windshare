package receivecontract

import "encoding/json"

type MaterializationPlanKind uint8

const (
	PlanDirectTree MaterializationPlanKind = 1
)

type MaterializationPlan struct {
	kind        MaterializationPlanKind
	reservation DestinationReservation
	valid       bool
}

func NewDirectTreePlan(artifact ArtifactSpec, reservation DestinationReservation) (MaterializationPlan, error) {
	layout, ok := artifact.DirectoryTree()
	if !ok || reservation.IsZero() || reservation.ArtifactDigest() != artifact.Digest() {
		return MaterializationPlan{}, ErrInvalidReceiveContract
	}
	switch layout.Kind() {
	case DirectoryTreeCatalogRoot:
		if reservation.Kind() != ReservationContainerRoot {
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
	return MaterializationPlan{
		kind:        PlanDirectTree,
		reservation: reservation,
		valid:       true,
	}, nil
}

func (plan MaterializationPlan) Kind() MaterializationPlanKind { return plan.kind }
func (plan MaterializationPlan) IsZero() bool                  { return !plan.valid }

func (plan MaterializationPlan) DestinationReservation() (DestinationReservation, bool) {
	return plan.reservation, plan.valid && plan.kind == PlanDirectTree
}

func (plan MaterializationPlan) OperationID() OperationID { return plan.reservation.OperationID() }
func (plan MaterializationPlan) ArtifactDigest() ArtifactDigest {
	return plan.reservation.ArtifactDigest()
}
func (plan MaterializationPlan) BindingDigest() BindingDigest { return plan.reservation.Digest() }

type materializationPlanJSON struct {
	Kind        string                     `json:"kind"`
	Reservation destinationReservationJSON `json:"reservation"`
}

func (plan MaterializationPlan) MarshalJSON() ([]byte, error) {
	if !plan.valid || plan.kind != PlanDirectTree {
		return nil, ErrInvalidReceiveContract
	}
	reservationBytes, err := json.Marshal(plan.reservation)
	if err != nil {
		return nil, err
	}
	return json.Marshal(struct {
		Kind        string          `json:"kind"`
		Reservation json.RawMessage `json:"reservation"`
	}{
		Kind:        "direct_tree",
		Reservation: reservationBytes,
	})
}

// UnmarshalMaterializationPlanJSON decodes a persisted MaterializationPlan and
// validates its nested reservation against artifact through NewDirectTreePlan.
func UnmarshalMaterializationPlanJSON(data []byte, artifact ArtifactSpec) (MaterializationPlan, error) {
	var payload materializationPlanJSON
	if err := decodeStrictJSON(data, &payload); err != nil {
		return MaterializationPlan{}, ErrInvalidReceiveContract
	}
	if payload.Kind != "direct_tree" {
		return MaterializationPlan{}, ErrInvalidReceiveContract
	}
	reservation, err := decodeReservationJSON(payload.Reservation, artifact)
	if err != nil {
		return MaterializationPlan{}, err
	}
	return NewDirectTreePlan(artifact, reservation)
}
