package humanoutput

import (
	"github.com/windshare/windshare/cmd/wind/internal/terminalcanvas"
	"github.com/windshare/windshare/core/catalog"
	appobs "github.com/windshare/windshare/engine/observation"
)

func (visitor eventVisitor) VisitSourceOmissions(event appobs.SourceOmissionsObserved) error {
	summary := event.Summary()
	lines := []terminalcanvas.Line{
		statusLine(visitor.symbols().Warning, "Sharing skipped "+formatCount(summary.Count)+" source items in "+escapedDisplay(summary.DirectoryPath), terminalcanvas.StyleWarning),
	}
	for _, sample := range summary.Samples {
		lines = append(lines, terminalcanvas.Plain("  "+escapedDisplay(sample.Name)+": "+sourceOmissionReason(sample)))
	}
	if unsampled := summary.Count - uint64(len(summary.Samples)); unsampled != 0 {
		lines = append(lines, terminalcanvas.Plain("  "+formatCount(unsampled)+" more skipped items; examples above are limited."))
	}
	lines = append(lines, terminalcanvas.Plain("  A skipped directory counts as one item; none of its contents are shared. Fix the source and share again to include it."))
	visitor.renderer.insert(lines...)
	return nil
}

func sourceOmissionReason(sample catalog.SourceOmission) string {
	switch sample.Reason {
	case catalog.SourceOmissionSymbolicLink:
		return "symbolic link; select the actual file or directory"
	case catalog.SourceOmissionReparsePoint:
		return "reparse point; select the actual file or directory"
	case catalog.SourceOmissionSpecialFile:
		return "special file; only regular files and directories can be shared"
	case catalog.SourceOmissionName:
		return escapedDisplay(sample.Detail) + "; rename this item"
	case catalog.SourceOmissionNameCollision:
		return escapedDisplay(sample.Detail) + " ignoring letter case or Unicode form; rename one of them"
	case catalog.SourceOmissionAccessDenied:
		return "access denied; grant this sender read access and directory traversal access, then share again"
	default:
		return "source item not included"
	}
}
