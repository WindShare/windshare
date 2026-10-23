package lanes

import (
	"bytes"
	"errors"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/content/records"
	"github.com/windshare/windshare/core/transfer/fault"
)

var errTestBlockIdentity = errors.New("test consumer rejected the block identity")

func testProtocolFault() fault.Fault {
	value, _ := fault.NewSession(fault.ScopeSessionTerminal, fault.SessionProtocol)
	return value
}
func testSourceFault() fault.Fault {
	value, _ := fault.NewSource(fault.ScopeFileLocal, fault.SourcePermanent)
	return value
}
func benchmarkTransferIdentity[T ~[catalog.IdentityBytes]byte](seed byte) T {
	var identity T
	for index := range identity {
		identity[index] = seed + byte(index)
	}
	return identity
}

func benchmarkTransferData(tb testing.TB, blockBytes uint32, blockCount int) (content.FileRevisionDescriptor, []records.BlockRecord) {
	tb.Helper()
	share := benchmarkTransferIdentity[catalog.ShareInstance](13)
	file := benchmarkTransferIdentity[catalog.FileID](37)
	revision := benchmarkTransferIdentity[content.FileRevision](61)
	geometry, err := content.NewFileGeometry(uint64(blockBytes)*uint64(blockCount), blockBytes)
	if err != nil {
		tb.Fatal(err)
	}
	descriptor, err := content.NewFileRevisionDescriptor(share, file, revision, geometry, catalog.ModifiedTime{})
	if err != nil {
		tb.Fatal(err)
	}
	result := make([]records.BlockRecord, blockCount)
	for index := range result {
		result[index], err = records.NewBlockRecord(
			descriptor, uint64(index), bytes.Repeat([]byte{byte(index)}, int(blockBytes)),
		)
		if err != nil {
			tb.Fatal(err)
		}
	}
	return descriptor, result
}
