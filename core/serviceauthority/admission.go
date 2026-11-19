package serviceauthority

type AdmissionState uint8

const (
	AdmissionReady AdmissionState = iota + 1
	AdmissionPendingSetup
	AdmissionExpired
	AdmissionTemporaryFailure
	AdmissionDenied
)

func (state AdmissionState) Name() string {
	switch state {
	case AdmissionReady:
		return "ready"
	case AdmissionPendingSetup:
		return "pending_setup"
	case AdmissionExpired:
		return "expired"
	case AdmissionTemporaryFailure:
		return "temporarily_unavailable"
	case AdmissionDenied:
		return "denied"
	default:
		return ""
	}
}

// A waiting RegistrationAuthorization and its Changed signal describe the
// same revision so progress cannot be missed while starting a wait.
type RegistrationAuthorization struct {
	State   AdmissionState
	Bearer  string          `json:"-"`
	Changed <-chan struct{} `json:"-"`
	Cause   error           `json:"-"`
}
