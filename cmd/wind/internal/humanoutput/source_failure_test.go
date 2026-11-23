package humanoutput

import (
	"strings"
	"testing"

	"github.com/windshare/windshare/cmd/wind/internal/terminalcanvas"
	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/engine"
)

func TestSourceFailuresRenderTheirSelectedPathWithoutTerminalControls(t *testing.T) {
	const path = "selected/文件\x1b[2J\nfile.bin"
	unexpected := engine.Failure{Code: engine.FailureUnexpected}
	for _, code := range []engine.FailureCode{
		engine.FailureSourceMissing,
		engine.FailureSourceAccessDenied,
		engine.FailureSourceUnsupported,
		engine.FailureSourceCloudPlaceholder,
		engine.FailureSourceUnavailable,
		engine.FailureSourceChanged,
	} {
		failure := engine.Failure{Code: code, SourcePath: path}
		for _, interactive := range []bool{false, true} {
			harness := newRenderHarness(t, terminalcanvas.Capabilities{Interactive: interactive}, false)
			harness.renderer.RenderCommandFailed(true, failure)
			text := harness.buffer.String()
			if !strings.Contains(text, terminalcanvas.EscapeText(path)) || strings.Contains(text, path) ||
				strings.Contains(text, failureMessage(unexpected)) {
				t.Fatalf("selected source did not render safely: %q", text)
			}
		}
	}
}

func TestCloudPlaceholderFailureAndOmissionGuidance(t *testing.T) {
	msg := failureMessage(engine.Failure{
		Code:       engine.FailureSourceCloudPlaceholder,
		SourcePath: `C:\OneDrive\slides.pptx`,
	})
	if !strings.Contains(msg, "Make this item available on this device, then share it again.") ||
		!strings.Contains(msg, `Path: C:\OneDrive\slides.pptx`) {
		t.Fatalf("unexpected cloud placeholder failure message: %q", msg)
	}
	reason := sourceOmissionReason(catalog.SourceOmission{
		Name:   "slides.pptx",
		Reason: catalog.SourceOmissionCloudPlaceholder,
	})
	if reason != "item not fully available on this device" {
		t.Fatalf("unexpected cloud omission reason: %q", reason)
	}
}
