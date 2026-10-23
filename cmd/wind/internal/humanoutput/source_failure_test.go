package humanoutput

import (
	"strings"
	"testing"

	"github.com/windshare/windshare/cmd/wind/internal/clievent"
	"github.com/windshare/windshare/cmd/wind/internal/terminalcanvas"
	appobs "github.com/windshare/windshare/engine/observation"
)

func TestSourceFailuresRenderTheirSelectedPathWithoutTerminalControls(t *testing.T) {
	const path = "selected/文件\x1b[2J\nfile.bin"
	unexpected, _ := appobs.NewFailure(appobs.FailureUnexpected)
	for _, code := range []appobs.FailureCode{appobs.FailureSourceMissing,
		appobs.FailureSourceAccessDenied, appobs.FailureSourceUnsupported,
		appobs.FailureSourceUnavailable, appobs.FailureSourceRevisionChanged} {
		failure, err := appobs.NewSourcePathFailure(code, appobs.NewDisplayPath(path))
		if err != nil {
			t.Fatal(err)
		}
		event, err := clievent.NewCommandFailed(clievent.CommandShare, clievent.ExitUsage, failure)
		if err != nil {
			t.Fatal(err)
		}
		for _, interactive := range []bool{false, true} {
			harness := newRenderHarness(t, terminalcanvas.Capabilities{Interactive: interactive}, false)
			if err := harness.renderer.RenderTerminal(event); err != nil {
				t.Fatal(err)
			}
			text := harness.buffer.String()
			if !strings.Contains(text, terminalcanvas.EscapeText(path)) || strings.Contains(text, path) ||
				strings.Contains(text, failureMessage(unexpected)) {
				t.Fatalf("selected source did not render safely: %q", text)
			}
		}
	}
}
