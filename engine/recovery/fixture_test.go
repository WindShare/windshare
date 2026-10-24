package recovery

import (
	"encoding/hex"
	"strings"

	"github.com/windshare/windshare/core/transfer/receivecontract"
)

func testResumeOperation(fill string, state OperationState) Operation {
	decoded, _ := hex.DecodeString(strings.Repeat(fill, 32))
	id, _ := receivecontract.OperationIDFromBytes(decoded)
	return Operation{ID: id, State: state}
}
