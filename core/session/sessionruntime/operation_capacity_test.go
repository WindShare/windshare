package sessionruntime

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/session/contentflow"
	"github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/transfer"
)

func TestRPCWaitsForOperationCapacityAndKeepsCancellationLive(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runtime, _ := newUnstartedRuntimeWithPolicy(t, protocolsession.RoleReceiver,
			protocolsession.OperationLimits{MaxActive: 1, MaxTracked: 2}, nil)
		recorder := newProtocolTraceRecorder(runtime)
		physical := runtime.lanes.active[runtime.initial.ID]
		writerContext, stopWriter := context.WithCancel(context.Background())
		writerDone := make(chan error, 1)
		go func() { writerDone <- physical.writer.Run(writerContext) }()
		defer func() { stopWriter(); <-writerDone }()
		client := newRPCClient(runtime, &deterministicReader{next: 70})
		body := []byte{0xa1, 0x00, 0x01}
		first, err := client.begin(context.Background(), protocolsession.MessageListChildren, body)
		if err != nil {
			t.Fatal(err)
		}
		type begun struct {
			call *operationCall
			err  error
		}
		begin := func(ctx context.Context) <-chan begun {
			done := make(chan begun, 1)
			go func() {
				call, err := client.begin(ctx, protocolsession.MessageListChildren, body)
				done <- begun{call, err}
			}()
			return done
		}
		second := begin(context.Background())
		synctest.Wait()
		select {
		case result := <-second:
			t.Fatalf("active capacity did not wait: %v", result.err)
		default:
		}
		if err := client.cancelAndEnd(first, contentflow.CancelReasonOutputAbort); err != nil {
			t.Fatal(err)
		}
		result := <-second
		if result.err != nil {
			t.Fatal(result.err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		third := begin(ctx)
		synctest.Wait()
		if err := client.cancelAndEnd(result.call, contentflow.CancelReasonOutputAbort); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		cancel()
		if result := <-third; !errors.Is(result.err, context.Canceled) {
			t.Fatalf("cancelled capacity wait: %v", result.err)
		}
		fourth := begin(context.Background())
		synctest.Wait()
		time.Sleep(protocolsession.OperationTombstoneLifetime)
		result = <-fourth
		if result.err != nil {
			t.Fatal(result.err)
		}
		if err := client.cancelAndEnd(result.call, contentflow.CancelReasonOutputAbort); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		if runtime.ctx.Err() != nil || !physical.writer.Accepting() || runtime.operations.ActiveCount() != 0 {
			t.Fatal("capacity pressure ended the session or leaked operation authority")
		}
		traces := recorder.snapshot()
		for _, stage := range []ProtocolOperationStage{
			ProtocolOperationReceiverWaitingActiveCapacity,
			ProtocolOperationReceiverWaitingRetainedCapacity,
			ProtocolOperationReceiverAdmissionReady,
		} {
			found := false
			for _, event := range traces {
				if event.Stage == stage && !event.OperationID.IsZero() && event.ProtocolSessionID == runtime.sessionID {
					found = true
				}
			}
			if !found {
				t.Fatalf("missing correlated admission trace stage %d", stage)
			}
		}
	})
}

func TestTransferResumesAcrossOperationRetentionWindows(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fixture := newVerticalFixture(t)
		limits := protocolsession.OperationLimits{MaxActive: 2, MaxTracked: 2}
		fixture.senderFactory.operationLimits = limits
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
		if receiver.ctx.Err() != nil || sender.ctx.Err() != nil || receiver.ProtocolSessionID() != sender.ProtocolSessionID() {
			t.Fatal("capacity pressure replaced or ended the session")
		}
	})
}
