package protocolsession

import (
	"context"
	"testing"
)

func TestSessionWriterWaitPathDeliversEveryQueueClass(t *testing.T) {
	t.Run("control", func(t *testing.T) {
		channel := newRuntimeChannel(0)
		writer, err := NewSessionWriter(
			channel,
			&passthroughSealer{},
			&permissivePolicy{direction: DirectionReceiverToSender},
		)
		if err != nil {
			t.Fatal(err)
		}
		operationID := testOperationID(62)
		message := mustMessage(t, MessageListChildren, &operationID, map[uint64]any{0: uint64(1)})
		receipt, err := writer.TryControl(message)
		if err != nil {
			t.Fatal(err)
		}
		schedule := writerSchedule{dataBurst: 3}
		terminal, err := writer.waitAndDeliver(context.Background(), &schedule)
		if err != nil || terminal {
			t.Fatalf("control wakeup = terminal %v, err %v", terminal, err)
		}
		if schedule.dataBurst != 0 || schedule.controlBurst != 1 || writer.controlUsage != (queueUsage{}) {
			t.Fatalf("control wakeup schedule=%+v usage=%+v", schedule, writer.controlUsage)
		}
		if outcome, waitErr := receipt.Wait(context.Background()); outcome != SendOutcomeDelivered || waitErr != nil {
			t.Fatalf("control receipt = %d, %v", outcome, waitErr)
		}
	})

	t.Run("data", func(t *testing.T) {
		channel := newRuntimeChannel(0)
		writer, err := NewSessionWriter(
			channel,
			&passthroughSealer{},
			&permissivePolicy{direction: DirectionSenderToReceiver},
		)
		if err != nil {
			t.Fatal(err)
		}
		operationID := testOperationID(63)
		receipt, err := writer.TryData(mustFragmentMessage(t, operationID, 1))
		if err != nil {
			t.Fatal(err)
		}
		schedule := writerSchedule{controlBurst: 3}
		terminal, err := writer.waitAndDeliver(context.Background(), &schedule)
		if err != nil || terminal {
			t.Fatalf("data wakeup = terminal %v, err %v", terminal, err)
		}
		if schedule.dataBurst != 1 || schedule.controlBurst != 0 || writer.dataUsage != (queueUsage{}) {
			t.Fatalf("data wakeup schedule=%+v usage=%+v", schedule, writer.dataUsage)
		}
		if outcome, waitErr := receipt.Wait(context.Background()); outcome != SendOutcomeDelivered || waitErr != nil {
			t.Fatalf("data receipt = %d, %v", outcome, waitErr)
		}
	})

	t.Run("terminal", func(t *testing.T) {
		channel := newRuntimeChannel(0)
		writer, err := NewSessionWriter(
			channel,
			&passthroughSealer{},
			&permissivePolicy{direction: DirectionSenderToReceiver},
		)
		if err != nil {
			t.Fatal(err)
		}
		receipt, err := writer.TrySenderControl(mustPreparedControl(t, MessageSessionTerminal, nil))
		if err != nil {
			t.Fatal(err)
		}
		terminal, err := writer.waitAndDeliver(context.Background(), &writerSchedule{})
		if err != nil || !terminal {
			t.Fatalf("terminal wakeup = terminal %v, err %v", terminal, err)
		}
		if outcome, waitErr := receipt.Wait(context.Background()); outcome != SendOutcomeDelivered || waitErr != nil {
			t.Fatalf("terminal receipt = %d, %v", outcome, waitErr)
		}
		channel.mu.Lock()
		terminalCalls := len(channel.terminal)
		channel.mu.Unlock()
		if terminalCalls != 1 {
			t.Fatalf("terminal transport calls = %d", terminalCalls)
		}
	})
}
