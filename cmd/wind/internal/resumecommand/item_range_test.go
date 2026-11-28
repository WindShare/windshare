package resumecommand

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"github.com/windshare/windshare/cmd/wind/internal/command"
)

// Item ordinals beyond the platform int range must be rejected while parsing.
// Converting them would wrap to non-positive numbers that later reach the
// inventory bounds check and status output as a different item.
func TestResumeDiscardRejectsItemsBeyondIntRange(t *testing.T) {
	root := t.TempDir()
	maxInt := strconv.FormatUint(uint64(^uint(0)>>1), 10)
	app, _, _ := newResumeTestApp()
	request, err := app.parser().ParseDiscard([]string{"-o", root, "--item", maxInt})
	if err != nil || strconv.Itoa(request.itemNumber) != maxInt {
		t.Fatalf("largest int item request=%+v err=%v", request, err)
	}

	for _, item := range []string{
		strconv.FormatUint(uint64(^uint(0)>>1)+1, 10),
		strconv.FormatUint(uint64(^uint(0)), 10),
	} {
		t.Run(item, func(t *testing.T) {
			request, err := app.parser().ParseDiscard([]string{"-o", root, "--item", item})
			if command.ParseExitCode(err) != command.ExitUsage {
				t.Fatalf("out-of-range item was accepted as %d (err=%v)", request.itemNumber, err)
			}

			snapshot, err := newResumeInventorySnapshot([]resumeOperation{
				testResumeOperation("1", resumeOperationResumable),
			}, false)
			if err != nil {
				t.Fatal(err)
			}
			opener := &fakeResumeStateInventoryOpener{inventory: &fakeResumeStateInventory{snapshot: snapshot}}
			runApp, stdout, stderr := newResumeTestApp()
			runApp.resumeInventories = opener
			result := runApp.Run(context.Background(), []string{"resume", "discard", "-o", root, "--item", item})
			if result != command.ExitUsage || opener.calls != 0 || stdout.Len() != 0 {
				t.Fatalf("result=%d calls=%d stdout=%q stderr=%q", result, opener.calls, stdout.String(), stderr.String())
			}
			if strings.Contains(stderr.String(), "--item -") {
				t.Fatalf("stderr reported a wrapped item number: %q", stderr.String())
			}
		})
	}
}
