package diagnostics

import "github.com/windshare/windshare/core/diagnosticerror"

func Failure(snapshot diagnosticerror.Snapshot) Value {
	if !snapshot.Present() {
		return Value{}
	}
	nodes := make([]Value, 0)
	for _, node := range snapshot.Nodes() {
		fields := Fields{"type": Text(node.Type), "message": Text(node.Message),
			"truncated": Bool(node.Truncated), "inspection_failed": Bool(node.InspectionFailed)}
		if node.Parent >= 0 {
			fields["parent_index"] = Int(int64(node.Parent))
		}
		nodes = append(nodes, Object(fields))
	}
	frames := make([]Value, 0)
	for _, frame := range snapshot.CaptureStack() {
		frames = append(frames, Object(Fields{"function": Text(frame.Function), "file": Text(frame.File), "line": Int(int64(frame.Line))}))
	}
	return Object(Fields{"source": Text(snapshot.Source()), "error_available": Bool(len(nodes) != 0),
		"nodes": Array(nodes...), "capture_stack": Array(frames...), "truncated": Bool(snapshot.Truncated())})
}
