package diagnosticerror

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"
)

type mutableError struct {
	message string
	cause   error
}

func (err *mutableError) Error() string { return err.message }
func (err *mutableError) Unwrap() error { return err.cause }

type cyclicError []byte

func (cyclicError) Error() string     { return "cycle" }
func (err cyclicError) Unwrap() error { return err }

type panicError struct{}

func (panicError) Error() string { panic("Error method failed") }
func (panicError) Unwrap() error { panic("Unwrap method failed") }

type manyCauses struct{ causes []error }

func (manyCauses) Error() string       { return "many causes" }
func (err manyCauses) Unwrap() []error { return err.causes }

func TestCaptureFreezesTypedJoinedCausesAndCaller(t *testing.T) {
	leaf := &mutableError{message: "native failure"}
	joined := errors.Join(fmt.Errorf("write: %w", leaf), errors.New("cleanup failure"))
	snapshot := Capture(joined, "peer")
	leaf.message = "later mutation"
	nodes := snapshot.Nodes()
	if !snapshot.Present() || snapshot.Source() != "peer" || snapshot.Truncated() {
		t.Fatalf("snapshot metadata = %#v", snapshot)
	}
	if len(nodes) != 4 || nodes[0].Type != "*errors.joinError" || nodes[0].Parent != -1 ||
		nodes[1].Parent != 0 || nodes[2].Parent != 1 || nodes[2].Type != "*diagnosticerror.mutableError" ||
		nodes[2].Message != "native failure" || nodes[3].Parent != 0 || nodes[3].Message != "cleanup failure" {
		t.Fatalf("cause graph = %#v", nodes)
	}
	stack := snapshot.CaptureStack()
	if len(stack) == 0 || !strings.HasSuffix(stack[0].Function, ".TestCaptureFreezesTypedJoinedCausesAndCaller") ||
		!strings.HasSuffix(stack[0].File, "snapshot_test.go") || stack[0].Line == 0 {
		t.Fatalf("capture location = %#v", stack)
	}
	nodes[0].Message = "observer mutation"
	stack[0].Function = "observer mutation"
	if snapshot.Nodes()[0].Message == "observer mutation" || snapshot.CaptureStack()[0].Function == "observer mutation" {
		t.Fatal("observer mutated retained evidence")
	}
}

func TestCaptureBoundsCyclesWideGraphsAndText(t *testing.T) {
	for _, test := range []struct {
		name   string
		err    error
		source string
	}{
		{"cycle with noncomparable error", cyclicError{1}, "runtime"},
		{"wide causes", manyCauses{causes: repeatedErrors(MaxNodes + 1)}, "lane_pump"},
		{"nil-heavy causes", manyCauses{causes: make([]error, MaxNodes+1)}, "peer"},
		{"long text", manyCauses{causes: repeatedErrors(MaxNodes)}, strings.Repeat("source", MaxSourceBytes)},
		{"invalid utf8", errors.New(strings.Repeat("\xff", MaxMessageBytes)), "runtime"},
	} {
		t.Run(test.name, func(t *testing.T) {
			snapshot := Capture(test.err, test.source)
			nodes := snapshot.Nodes()
			if len(nodes) > MaxNodes || !snapshot.Truncated() {
				t.Fatalf("unbounded or unmarked evidence: %#v", snapshot)
			}
			total := len(snapshot.Source())
			for index, node := range nodes {
				if !utf8.ValidString(node.Message) || len(node.Message) > MaxMessageBytes ||
					len(node.Type) > MaxTypeBytes || node.Parent >= index {
					t.Fatalf("invalid bounded node: %#v", node)
				}
				depth := 1
				for parent := node.Parent; parent >= 0; parent = nodes[parent].Parent {
					depth++
				}
				if depth > MaxDepth {
					t.Fatalf("depth = %d", depth)
				}
				total += len(node.Message) + len(node.Type)
			}
			if total > MaxErrorTextBytes {
				t.Fatalf("error bytes = %d", total)
			}
			total = 0
			for _, frame := range snapshot.CaptureStack() {
				total += len(frame.Function) + len(frame.File)
			}
			if total > MaxStackTextBytes {
				t.Fatalf("stack bytes = %d", total)
			}
		})
	}
}

func repeatedErrors(count int) []error {
	result := make([]error, count)
	for index := range result {
		result[index] = errors.New(strings.Repeat("native failure ", MaxMessageBytes))
	}
	return result
}

func TestCaptureSurvivesInspectionPanicsAndDistinguishesMissingError(t *testing.T) {
	snapshot := Capture(panicError{}, "runtime")
	nodes := snapshot.Nodes()
	if len(nodes) != 1 || !nodes[0].InspectionFailed || nodes[0].Type != "diagnosticerror.panicError" {
		t.Fatalf("inspection failure = %#v", nodes)
	}
	missing := Capture(nil, "peer")
	if !missing.Present() || len(missing.Nodes()) != 0 || len(missing.CaptureStack()) == 0 {
		t.Fatalf("missing error lost its capture site: %#v", missing)
	}
	empty := Snapshot{}
	if empty.Present() || empty.Truncated() || empty.Source() != "" || len(empty.Nodes()) != 0 || len(empty.CaptureStack()) != 0 {
		t.Fatal("zero snapshot is not absent")
	}
}

func TestCaptureBoundsStack(t *testing.T) {
	snapshot := deepCapture(MaxFrames + 2)
	if len(snapshot.CaptureStack()) != MaxFrames || !snapshot.Truncated() {
		t.Fatalf("unbounded stack: %#v", snapshot)
	}
}

func deepCapture(depth int) Snapshot {
	if depth == 0 {
		return Capture(errors.New("failure"), "runtime")
	}
	return deepCapture(depth - 1)
}
