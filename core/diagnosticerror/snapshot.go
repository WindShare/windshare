// Package diagnosticerror freezes bounded error evidence without retaining errors.
package diagnosticerror

import (
	"reflect"
	"runtime"
	"slices"
	"strings"
)

const (
	MaxNodes           = 16
	MaxDepth           = 8
	MaxFrames          = 12
	MaxMessageBytes    = 1024
	MaxTypeBytes       = 256
	MaxSourceBytes     = 256
	MaxFrameFieldBytes = 256
	MaxErrorTextBytes  = 6144
	MaxStackTextBytes  = 2048
)

// Node uses a parent index to retain every branch of errors.Join without a
// recursive wire shape. The root's parent is -1.
type Node struct {
	Parent           int
	Type             string
	Message          string
	Truncated        bool
	InspectionFailed bool
}

// Frame is a capture location, not a claim about where an error was created.
type Frame struct {
	Function string
	File     string
	Line     int
}

// Snapshot exposes copies of its collections so observer queues never share
// mutable error objects or mutable backing arrays with their consumers.
type Snapshot struct {
	present   bool
	source    string
	nodes     []Node
	stack     []Frame
	truncated bool
}

func (snapshot Snapshot) Present() bool         { return snapshot.present }
func (snapshot Snapshot) Source() string        { return snapshot.source }
func (snapshot Snapshot) Nodes() []Node         { return slices.Clone(snapshot.nodes) }
func (snapshot Snapshot) CaptureStack() []Frame { return slices.Clone(snapshot.stack) }
func (snapshot Snapshot) Truncated() bool       { return snapshot.truncated }

// Capture records the call site and snapshots standard single- and multi-cause
// errors. A nil error still records the source and stack, with no error nodes.
func Capture(err error, source string) Snapshot {
	snapshot := Snapshot{present: true}
	errorBudget := textBudget{remaining: MaxErrorTextBytes}
	snapshot.source = errorBudget.take(source, MaxSourceBytes)
	snapshot.appendError(err, -1, 0, &errorBudget)

	pcs := make([]uintptr, MaxFrames+1)
	count := runtime.Callers(2, pcs)
	frames := runtime.CallersFrames(pcs[:count])
	stackBudget := textBudget{remaining: MaxStackTextBytes}
	for count > 0 {
		frame, more := frames.Next()
		if len(snapshot.stack) == MaxFrames {
			snapshot.truncated = true
			break
		}
		snapshot.stack = append(snapshot.stack, Frame{
			Function: stackBudget.take(frame.Function, MaxFrameFieldBytes),
			File:     stackBudget.take(frame.File, MaxFrameFieldBytes),
			Line:     frame.Line,
		})
		if !more {
			break
		}
	}
	snapshot.truncated = snapshot.truncated || errorBudget.truncated || stackBudget.truncated
	return snapshot
}

func (snapshot *Snapshot) appendError(err error, parent, depth int, budget *textBudget) {
	if err == nil {
		return
	}
	if len(snapshot.nodes) >= MaxNodes || depth >= MaxDepth {
		snapshot.truncated = true
		if parent >= 0 {
			snapshot.nodes[parent].Truncated = true
		}
		return
	}
	index := len(snapshot.nodes)
	message, messageFailed := errorMessage(err)
	node := Node{
		Parent:           parent,
		Type:             budget.take(reflect.TypeOf(err).String(), MaxTypeBytes),
		Message:          budget.take(message, MaxMessageBytes),
		InspectionFailed: messageFailed,
	}
	node.Truncated = node.Message != message
	snapshot.nodes = append(snapshot.nodes, node)
	causes, unwrapFailed := errorCauses(err)
	snapshot.nodes[index].InspectionFailed = messageFailed || unwrapFailed
	for causeIndex, cause := range causes {
		if causeIndex >= MaxNodes || len(snapshot.nodes) >= MaxNodes {
			// Bound inspection as well as retained nodes, including nil-heavy
			// child lists and cyclic unwrap graphs.
			snapshot.nodes[index].Truncated = true
			snapshot.truncated = true
			break
		}
		snapshot.appendError(cause, index, depth+1, budget)
	}
}

func errorMessage(err error) (message string, failed bool) {
	defer func() {
		if recover() != nil {
			message = ""
			failed = true
		}
	}()
	return err.Error(), false
}

//nolint:errorlint // errors.As would traverse the graph outside our depth and node limits.
func errorCauses(err error) (causes []error, failed bool) {
	defer func() {
		if recover() != nil {
			causes = nil
			failed = true
		}
	}()
	switch value := err.(type) {
	case interface{ Unwrap() []error }:
		return value.Unwrap(), false
	case interface{ Unwrap() error }:
		return []error{value.Unwrap()}, false
	default:
		return nil, false
	}
}

type textBudget struct {
	remaining int
	truncated bool
}

func (budget *textBudget) take(text string, limit int) string {
	count := min(len(text), limit, budget.remaining)
	if count < len(text) {
		budget.truncated = true
	}
	budget.remaining -= count
	// Clone only the bounded prefix: a substring must not pin a large original
	// message. ASCII replacement also keeps invalid UTF-8 within the byte budget.
	value := strings.ToValidUTF8(text[:count], "?")
	if value != text[:count] {
		budget.truncated = true
	}
	return strings.Clone(value)
}
