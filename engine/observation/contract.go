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
