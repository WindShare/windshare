package humanoutput

import (
	"strings"
	"testing"

	"github.com/windshare/windshare/cmd/wind/internal/terminalcanvas"
	"github.com/windshare/windshare/core/catalog"
)

func TestSourceOmissionsCountNounsAgreeWithCounts(t *testing.T) {
	symlink := catalog.SourceOmission{Name: "link", Reason: catalog.SourceOmissionSymbolicLink}
	for _, test := range []struct {
		name    string
		summary catalog.SourceOmissions
		want    []string
		reject  []string
	}{
		{
			name:    "one skipped item",
			summary: catalog.SourceOmissions{DirectoryPath: "/src", Count: 1, Samples: []catalog.SourceOmission{symlink}},
			want:    []string{"Sharing skipped 1 source item in /src"},
			reject:  []string{"1 source items", "more skipped"},
		},
		{
			name:    "one unsampled item",
			summary: catalog.SourceOmissions{DirectoryPath: "/src", Count: 2, Samples: []catalog.SourceOmission{symlink}},
			want:    []string{"Sharing skipped 2 source items in /src", "  1 more skipped item; examples above are limited."},
			reject:  []string{"1 more skipped items"},
		},
		{
			name:    "several unsampled items",
			summary: catalog.SourceOmissions{DirectoryPath: "/src", Count: 1001, Samples: []catalog.SourceOmission{symlink}},
			want:    []string{"Sharing skipped 1,001 source items in /src", "  1,000 more skipped items; examples above are limited."},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			harness := newRenderHarness(t, terminalcanvas.Capabilities{}, false)
			harness.renderer.RenderSourceOmissions(test.summary)
			output := harness.buffer.String()
			for _, want := range test.want {
				if !strings.Contains(output, want) {
					t.Errorf("output lacks %q:\n%s", want, output)
				}
			}
			for _, reject := range test.reject {
				if strings.Contains(output, reject) {
					t.Errorf("output contains %q:\n%s", reject, output)
				}
			}
		})
	}
}
