package serviceauthority

type AdmissionState uint8

const (
	AdmissionReady AdmissionState = iota + 1
	AdmissionPendingSetup
	AdmissionExpired
	AdmissionTemporaryFailure
	AdmissionDenied
	AdmissionUserActionRequired
	AdmissionPendingAttribution
)

func (state AdmissionState) Name() string {
	switch state {
	case AdmissionReady:
		return "ready"
	case AdmissionPendingSetup:
		return "pending_setup"
	case AdmissionPendingAttribution:
		return "pending_attribution"
	case AdmissionExpired:
		return "expired"
	case AdmissionTemporaryFailure:
		return "temporarily_unavailable"
	case AdmissionDenied:
		return "denied"
	case AdmissionUserActionRequired:
		return "user_action_required"
	default:
		return ""
	}
}

// A waiting RegistrationAuthorization and its Changed signal describe the
// same revision so progress cannot be missed while starting a wait.
type RegistrationAuthorization struct {
	State       AdmissionState
	Bearer      string                  `json:"-"`
	Changed     <-chan struct{}         `json:"-"`
	Cause       error                   `json:"-"`
	Reservation RegistrationReservation `json:"-"`
}

type RegistrationDelivery uint8

const (
	RegistrationMayHaveBeenSent RegistrationDelivery = iota
	RegistrationNotSent
)

// A RegistrationReservation pins attribution for one ready attempt. Settle it
// with the dial result before applying any local cancellation or admission policy.
type RegistrationReservation func(RegistrationDelivery)

func (reservation RegistrationReservation) Finish(delivery RegistrationDelivery) {
	if reservation != nil {
		reservation(delivery)
	}
}
