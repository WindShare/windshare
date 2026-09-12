package runtrace

import (
	"strconv"

	"github.com/windshare/windshare/core/diagnosticerror"
)

type diagnosticFailureV4 struct {
	Source         string                  `json:"source"`
	ErrorAvailable bool                    `json:"error_available"`
	Nodes          []diagnosticErrorNodeV4 `json:"nodes"`
	CaptureStack   []diagnosticFrameV4     `json:"capture_stack"`
	Truncated      bool                    `json:"truncated"`
}

type diagnosticErrorNodeV4 struct {
	ParentIndex      *string `json:"parent_index,omitempty"`
	Type             string  `json:"type"`
	Message          string  `json:"message"`
	Truncated        bool    `json:"truncated,omitempty"`
	InspectionFailed bool    `json:"inspection_failed,omitempty"`
}

type diagnosticFrameV4 struct {
	Function string `json:"function"`
	File     string `json:"file"`
	Line     string `json:"line"`
}

func projectDiagnosticFailure(snapshot diagnosticerror.Snapshot) *diagnosticFailureV4 {
	if !snapshot.Present() {
		return nil
	}
	nodes := snapshot.Nodes()
	failure := &diagnosticFailureV4{
		Source: snapshot.Source(), ErrorAvailable: len(nodes) != 0,
		Nodes:        make([]diagnosticErrorNodeV4, 0, len(nodes)),
		CaptureStack: make([]diagnosticFrameV4, 0),
		Truncated:    snapshot.Truncated(),
	}
	for _, node := range nodes {
		var parent *string
		if node.Parent >= 0 {
			value := strconv.Itoa(node.Parent)
			parent = &value
		}
		failure.Nodes = append(failure.Nodes, diagnosticErrorNodeV4{
			ParentIndex: parent, Type: node.Type, Message: node.Message,
			Truncated: node.Truncated, InspectionFailed: node.InspectionFailed,
		})
	}
	for _, frame := range snapshot.CaptureStack() {
		failure.CaptureStack = append(failure.CaptureStack, diagnosticFrameV4{
			Function: frame.Function, File: frame.File, Line: strconv.Itoa(frame.Line),
		})
	}
	return failure
}
