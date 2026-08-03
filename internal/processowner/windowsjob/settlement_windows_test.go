//go:build windows

package windowsjob

import (
	"bytes"
	"testing"

	ownerprotocol "github.com/windshare/windshare/internal/processowner/protocol"
)

func decodeTestSettlement(t *testing.T, output *bytes.Buffer) ownerprotocol.Settlement {
	t.Helper()
	settlement, err := ownerprotocol.DecodeLine[ownerprotocol.Settlement](output.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if err := ownerprotocol.ValidateSettlement(settlement); err != nil {
		t.Fatal(err)
	}
	return settlement
}
