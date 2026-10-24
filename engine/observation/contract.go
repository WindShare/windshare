// Package observation owns immutable application facts and bounded diagnostic evidence.
// Adapters present these facts without interpreting network implementation types.
// Events retain structured diagnostic detail; rendering and exit status belong to clients.
package observation

type Event interface {
	EngineEvent()
	Workflow() Workflow
	Level() Level
	Validate() error
}
type Workflow uint8

const (
	WorkflowShare Workflow = iota + 1
	WorkflowReceive
)

func (w Workflow) Name() (string, bool) {
	switch w {
	case WorkflowShare:
		return "share", true
	case WorkflowReceive:
		return "receive", true
	default:
		return "", false
	}
}
func (w Workflow) Valid() bool { _, ok := w.Name(); return ok }

// OutputDiagnostic is immutable, provider-owned debugging evidence. It carries
// no task authority: output modes and failures travel through separate contracts.
// Hosts may render supported provider schemas; shared workflows only validate
// and forward them through the bounded receive observation stream.
type OutputDiagnostic interface {
	Event
	OutputDiagnostic()
}

// OutputDiagnosticError retains why provider evidence was rejected without
// granting that evidence authority over the receive task. Only the pointer form
// implements error so providers and consumers agree on errors.AsType matching.
type OutputDiagnosticError struct {
	Reason  ObserverLossReason
	Context ObservationRejection
}

func (*OutputDiagnosticError) Error() string { return ErrInvalidEvent.Error() }
func (*OutputDiagnosticError) Unwrap() error { return ErrInvalidEvent }
