package receivecontract

import (
	"encoding/hex"
	"strings"
	"testing"
)

func TestCollisionNameFitsEveryValidNameWithoutSplittingScalars(t *testing.T) {
	operation := OperationID{1}
	for _, test := range []struct {
		name, requested, stem, extension string
		fileLike                         bool
	}{
		{"ordinary", "report.txt", "report", ".txt", true},
		{"long stem", strings.Repeat("a", 251) + ".txt", strings.Repeat("a", 240), ".txt", true},
		{"unicode stem", strings.Repeat("界", 83) + ".txt", strings.Repeat("界", 80), ".txt", true},
		{"negative budget", "x." + strings.Repeat("y", 253), "x." + strings.Repeat("y", 242), "", true},
		{"zero budget", "x." + strings.Repeat("y", 243), "x." + strings.Repeat("y", 242), "", true},
		{"partial scalar budget", "😀." + strings.Repeat("y", 240), "😀." + strings.Repeat("y", 239), "", true},
		{"exact scalar budget", "😀." + strings.Repeat("y", 239), "😀", "." + strings.Repeat("y", 239), true},
		{"unicode extension", "x." + strings.Repeat("界", 84), "x." + strings.Repeat("界", 80), "", true},
		{"no extension", strings.Repeat("a", 255), strings.Repeat("a", 244), "", true},
		{"dotfile", "." + strings.Repeat("a", 254), "." + strings.Repeat("a", 243), "", true},
		{"multiple dots", "report.tar.gz", "report.tar", ".gz", true},
		{"dotted directory", "x." + strings.Repeat("y", 253), "x." + strings.Repeat("y", 242), "", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			unchanged, err := CollisionName(operation, test.requested, 0, test.fileLike)
			if err != nil || unchanged != test.requested {
				t.Fatalf("first name = %q, %v", unchanged, err)
			}
			reserved, err := CollisionName(operation, test.requested, 1, test.fileLike)
			if err != nil || canonicalComponent(reserved) != nil || len(reserved) > MaxResultComponentBytes {
				t.Fatalf("collision name = %q, %v", reserved, err)
			}
			if !strings.HasPrefix(reserved, test.stem) || !strings.HasSuffix(reserved, test.extension) ||
				len(reserved) != len(test.stem)+1+CollisionSuffixHexChars+len(test.extension) {
				t.Fatalf("collision name %q did not preserve prefix %q and extension %q", reserved, test.stem, test.extension)
			}
			suffix := reserved[len(test.stem) : len(reserved)-len(test.extension)]
			if _, err := hex.DecodeString(suffix[1:]); err != nil || suffix[0] != '-' {
				t.Fatalf("collision token = %q, %v", suffix, err)
			}
			repeated, err := CollisionName(operation, test.requested, 1, test.fileLike)
			if err != nil || repeated != reserved {
				t.Fatalf("same reservation changed = %q, %v", repeated, err)
			}
			next, err := CollisionName(operation, test.requested, 2, test.fileLike)
			if err != nil || next == reserved {
				t.Fatalf("next collision reused name = %q, %v", next, err)
			}
		})
	}
}
