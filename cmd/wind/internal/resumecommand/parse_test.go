package resumecommand

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/windshare/windshare/cmd/wind/internal/command"
)

func TestResumePositionalsCannotBecomeHelpRequests(t *testing.T) {
	root := t.TempDir()
	for action, options := range map[string][]string{
		"list":    {"-o", root},
		"discard": {"-o", root, "--item", "1"},
	} {
		for name, suffix := range map[string][]string{
			"help immediately after terminator": {"--", "-h"},
			"short help after terminator":       {"--", "literal", "-h"},
			"long help after terminator":        {"--", "literal", "--help"},
			"short help after positional":       {"literal", "-h"},
			"long help after positional":        {"literal", "--help"},
		} {
			t.Run(action+"/"+name, func(t *testing.T) {
				opener := &fakeResumeStateInventoryOpener{}
				app, _, stderr := newResumeTestApp()
				app.resumeInventories = opener
				args := append([]string{"resume", action}, options...)
				args = append(args, suffix...)
				if result := app.Run(context.Background(), args); result != command.ExitUsage {
					t.Fatalf("result=%d stderr=%q", result, stderr.String())
				}
				if opener.calls != 0 {
					t.Fatalf("invalid request opened destination state %d times", opener.calls)
				}
			})
		}
	}
}

func TestResumeParsersPreserveOptionValuesBeforeTerminator(t *testing.T) {
	for _, value := range []string{"--", "-h", "--help"} {
		t.Run(value, func(t *testing.T) {
			root, err := filepath.Abs(value)
			if err != nil {
				t.Fatal(err)
			}
			app, _, _ := newResumeTestApp()
			request, err := app.parser().ParseRoot("resume list", []string{"-o", value, "--"})
			if err != nil || request.rootPath != root {
				t.Fatalf("list request=%+v err=%v", request, err)
			}
			discard, err := app.parser().ParseDiscard([]string{"--item", "1", "-o", value, "--"})
			if err != nil || discard.rootPath != root || discard.itemNumber != 1 {
				t.Fatalf("discard request=%+v err=%v", discard, err)
			}
		})
	}
}
