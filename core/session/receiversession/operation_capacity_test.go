package receiversession

import (
	"bytes"
	"context"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/transfer"
	"testing"
	"testing/synctest"
	"time"
)

func TestTransferResumesAcrossOperationRetentionWindows(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fixture := newVerticalFixture(t)
		limits := protocolsession.OperationLimits{MaxActive: 2, MaxTracked: 2}
		fixture.senderConfig.OperationLimits = limits
		fixture.rebuildSender(t)
		fixture.receiverFactory.operationLimits = limits
		sender, receiver := connectVerticalPair(t, fixture.senderFactory, fixture.receiverFactory)
		defer receiver.Close()
		defer sender.Close()
		opened, err := receiver.OpenRevision(context.Background(), fixture.fileID)
		if err != nil {
			t.Fatal(err)
		}
		started := time.Now()
		output := make([]byte, len(fixture.fileData))
		err = receiver.BlockBroker().ReadRange(context.Background(), opened.LeaseID, opened.Descriptor,
			content.Range{Offset: 0, End: uint64(len(output))},
			transfer.RangeSinkFunc(func(_ context.Context, offset uint64, data []byte) error {
				copy(output[offset:], data)
				return nil
			}))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(output, fixture.fileData) || time.Since(started) < protocolsession.OperationTombstoneLifetime {
			t.Fatal("transfer did not preserve bytes across the retention wait")
		}
		if receiver.Lifetime().Err() != nil || sender.Lifetime().Err() != nil || receiver.ProtocolSessionID() != sender.ProtocolSessionID() {
			t.Fatal("capacity pressure replaced or ended the session")
		}
	})
}
