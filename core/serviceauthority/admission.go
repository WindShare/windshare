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

// RegistrationAuthorization and Changed describe one immutable source revision.
// No caller needs to acquire credentials inside a transport handshake.
type RegistrationAuthorization struct {
	State   AdmissionState
	Bearer  string          `json:"-"`
	Changed <-chan struct{} `json:"-"`
	Cause   error           `json:"-"`
	// Retain fixes attribution after a registration may have reached the relay.
	// Its returned release belongs to the publication, including uncertain sends.
	Retain func() func() `json:"-"`
}
