package protocolcontract

import (
	"encoding/base64"
	"strings"
	"testing"

	"github.com/windshare/windshare/core/transfer/receivecontract"
)

func buildNameCollisionVectorCases(t *testing.T) []any {
	t.Helper()
	operation := receivecontract.OperationID{1}
	var cases []any
	for _, test := range []struct {
		name, requested string
		fileLike        bool
	}{
		{"ordinary", "report.txt", true},
		{"long-stem", strings.Repeat("a", 251) + ".txt", true},
		{"unicode-stem", strings.Repeat("界", 83) + ".txt", true},
		{"negative-stem-budget", "x." + strings.Repeat("y", 253), true},
		{"zero-stem-budget", "x." + strings.Repeat("y", 243), true},
		{"partial-scalar-budget", "😀." + strings.Repeat("y", 240), true},
		{"exact-scalar-budget", "😀." + strings.Repeat("y", 239), true},
		{"unicode-extension", "x." + strings.Repeat("界", 84), true},
		{"no-extension", strings.Repeat("a", 255), true},
		{"dotfile", "." + strings.Repeat("a", 254), true},
		{"multiple-dots", "report.tar.gz", true},
		{"dotted-directory", "x." + strings.Repeat("y", 253), false},
	} {
		reserved, err := receivecontract.CollisionName(operation, test.requested, 1, test.fileLike)
		if err != nil {
			t.Fatalf("collision vector %s: %v", test.name, err)
		}
		cases = append(cases, map[string]any{
			"name": test.name,
			"input": map[string]any{
				"operationId":    base64.RawURLEncoding.EncodeToString(operation.Bytes()),
				"requestedName":  test.requested,
				"collisionIndex": 1,
				"fileLike":       test.fileLike,
			},
			"expected": map[string]any{"reservedName": reserved},
		})
	}
	return cases
}
