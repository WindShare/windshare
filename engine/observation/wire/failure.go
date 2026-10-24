package wire

import (
	"strconv"

	"github.com/windshare/windshare/core/diagnosticerror"
)

type DiagnosticFailure struct {
	Source         string                `json:"source"`
	ErrorAvailable bool                  `json:"error_available"`
	Nodes          []DiagnosticErrorNode `json:"nodes"`
	CaptureStack   []DiagnosticFrame     `json:"capture_stack"`
	Truncated      bool                  `json:"truncated"`
}

type DiagnosticErrorNode struct {
	ParentIndex      *string `json:"parent_index,omitempty"`
	Type             string  `json:"type"`
	Message          string  `json:"message"`
	Truncated        bool    `json:"truncated,omitempty"`
	InspectionFailed bool    `json:"inspection_failed,omitempty"`
}

type DiagnosticFrame struct {
	Function string `json:"function"`
	File     string `json:"file"`
	Line     string `json:"line"`
}

func EncodeDiagnosticFailure(snapshot diagnosticerror.Snapshot) *DiagnosticFailure {
	if !snapshot.Present() {
		return nil
	}
	nodes := snapshot.Nodes()
	failure := &DiagnosticFailure{
		Source: snapshot.Source(), ErrorAvailable: len(nodes) != 0,
		Nodes:        make([]DiagnosticErrorNode, 0, len(nodes)),
		CaptureStack: make([]DiagnosticFrame, 0),
		Truncated:    snapshot.Truncated(),
	}
	for _, node := range nodes {
		var parent *string
		if node.Parent >= 0 {
			value := strconv.Itoa(node.Parent)
			parent = &value
		}
		failure.Nodes = append(failure.Nodes, DiagnosticErrorNode{
			ParentIndex: parent, Type: node.Type, Message: node.Message,
			Truncated: node.Truncated, InspectionFailed: node.InspectionFailed,
		})
	}
	for _, frame := range snapshot.CaptureStack() {
		failure.CaptureStack = append(failure.CaptureStack, DiagnosticFrame{
			Function: frame.Function, File: frame.File, Line: strconv.Itoa(frame.Line),
		})
	}
	return failure
}
