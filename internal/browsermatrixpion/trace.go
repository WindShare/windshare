package browsermatrixpion

const (
	traceAuthorityProbe    = "authority-probe"
	traceSTUNProbe         = "stun-probe"
	traceTURNCredential    = "turn-credential"
	traceAttemptReserved   = "attempt-reserved"
	traceAttemptStarting   = "attempt-starting"
	traceAttemptActive     = "attempt-active"
	traceOfferTerminal     = "offer-terminal"
	traceTerminalReceipt   = "terminal-receipt"
	tracePairObserved      = "selected-pair-observed"
	traceChallengeObserved = "challenge-observed"
	traceAttemptRetiring   = "attempt-retiring"
	traceAttemptReaped     = "attempt-reaped"
	traceContainmentFailed = "containment-failed"
)

// TraceEvent intentionally exposes only stable authority identifiers and
// decisions. SDP, bearer credentials, TLS keys, and proof challenges never
// enter operational logs.
type TraceEvent struct {
	Milestone         string `json:"milestone"`
	InstanceID        string `json:"instanceId,omitempty"`
	RunID             string `json:"runId,omitempty"`
	AttestationSHA256 string `json:"attestationSha256,omitempty"`
	RequestID         string `json:"requestId,omitempty"`
	AttemptID         string `json:"attemptId,omitempty"`
	Outcome           string `json:"outcome,omitempty"`
}

// TraceSink must return promptly. A panic is isolated because observability
// cannot acquire lifecycle authority; a blocking sink still keeps the process
// owner alive and is contained by the outer process supervisor.
type TraceSink func(TraceEvent)

func emitTrace(sink TraceSink, event TraceEvent) {
	if sink == nil {
		return
	}
	defer func() { _ = recover() }()
	sink(event)
}
