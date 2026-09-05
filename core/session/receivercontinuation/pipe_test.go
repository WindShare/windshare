package receivercontinuation

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/windshare/windshare/core/framechannel"
)

type retireBeforeTerminalChannel struct {
	framechannel.Channel
	attempts int
}

func (channel *retireBeforeTerminalChannel) SendTerminal(ctx context.Context, frame framechannel.Frame) error {
	channel.attempts++
	// Fix the race at transport admission: the sender already owns its terminal
	// receipt, but peer closure wins before the pipe can accept the frame.
	if err := channel.Channel.Close(); err != nil {
		return err
	}
	return channel.Channel.SendTerminal(ctx, frame)
}

func TestSenderStopJoinsPipeRetirementBeforeTerminalAcceptance(t *testing.T) {
	f := newFixture(t)
	sender, receiver := newPipe()
	terminal := &retireBeforeTerminalChannel{Channel: sender}
	runtime := f.connectChannels(terminal, receiver)
	if err := f.sender.Close(); err != nil {
		t.Fatalf("sender cleanup failed after transport retired before terminal acceptance: %v", err)
	}
	if terminal.attempts != 1 {
		t.Fatalf("terminal attempts = %d, want 1", terminal.attempts)
	}
	<-runtime.Done()
	if !runtime.PathsExhausted() {
		t.Fatal("receiver retained authority after pipe retirement")
	}
}

func TestPipeSendClassifiesFailuresBeforeAcceptance(t *testing.T) {
	for _, retired := range []bool{false, true} {
		for _, terminal := range []bool{false, true} {
			sender, receiver := newPipe()
			t.Cleanup(func() { _ = sender.Close() })
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			wantCause, wantDisposition := error(context.Canceled), framechannel.SendRejected
			if retired {
				if err := sender.Close(); err != nil {
					t.Fatal(err)
				}
				wantCause, wantDisposition = io.ErrClosedPipe, framechannel.SendRetired
			} else {
				// A full inbox makes cancellation the only selectable branch without
				// replacing the pipe's real frame delivery with a synthetic failure.
				inbox := receiver.pipe.inbox[receiver.index]
				for range cap(inbox) {
					inbox <- []byte("accepted")
				}
			}
			send := sender.Send
			if terminal {
				send = sender.SendTerminal
			}
			err := send(ctx, []byte("not accepted"))
			if !errors.Is(err, wantCause) || framechannel.SendDispositionOf(err) != wantDisposition {
				t.Fatalf("retired=%v terminal=%v disposition=%v err=%v", retired, terminal, framechannel.SendDispositionOf(err), err)
			}
		}
	}
}
