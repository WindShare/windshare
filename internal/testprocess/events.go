package testprocess

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/windshare/windshare/internal/processowner/protocol"
)

const maximumPendingEvents = 128

// EventReader separates the consumer-visible terminal state from ownership of
// the transport. A malformed stream fails consumers immediately, while the one
// tracked reader goroutine keeps draining so target logging cannot deadlock
// process-tree settlement.
type EventReader struct {
	events chan protocol.Event
	done   chan struct{}
	source io.ReadCloser

	terminalOnce sync.Once
	closeOnce    sync.Once
	mu           sync.Mutex
	err          error
	closeErr     error
}

func newEventReader(source io.ReadCloser, identity protocol.Identity) *EventReader {
	reader := &EventReader{
		events: make(chan protocol.Event, maximumPendingEvents),
		done:   make(chan struct{}),
		source: source,
	}
	go reader.read(identity)
	return reader
}

func (reader *EventReader) Next(ctx context.Context) (protocol.Event, error) {
	if event, terminal, ready := reader.nextReady(); ready {
		return event, terminal
	}
	select {
	case event, open := <-reader.events:
		return reader.eventResult(event, open)
	case <-ctx.Done():
		// Buffered or terminal event evidence wins a simultaneous cancellation.
		if event, terminal, ready := reader.nextReady(); ready {
			return event, terminal
		}
		return protocol.Event{}, ctx.Err()
	}
}

func (reader *EventReader) nextReady() (protocol.Event, error, bool) {
	select {
	case event, open := <-reader.events:
		value, err := reader.eventResult(event, open)
		return value, err, true
	default:
		return protocol.Event{}, nil, false
	}
}

func (reader *EventReader) eventResult(event protocol.Event, open bool) (protocol.Event, error) {
	if open {
		return event, nil
	}
	if err := reader.Err(); err != nil {
		return protocol.Event{}, err
	}
	return protocol.Event{}, io.EOF
}

func (reader *EventReader) Done() <-chan struct{} { return reader.done }

func (reader *EventReader) Err() error {
	reader.mu.Lock()
	defer reader.mu.Unlock()
	return reader.err
}

func (reader *EventReader) read(identity protocol.Identity) {
	defer close(reader.done)
	defer reader.closeSource()
	defer reader.closeEvents()

	buffered := bufio.NewReaderSize(reader.source, protocol.MaximumDocumentBytes+1)
	for {
		line, err := buffered.ReadSlice('\n')
		if errors.Is(err, io.EOF) && len(line) == 0 {
			return
		}
		if err != nil {
			reader.fail(fmt.Errorf("read test event: %w", err))
			_, _ = io.Copy(io.Discard, buffered)
			return
		}
		event, err := protocol.DecodeLine[protocol.Event](line)
		if err == nil {
			err = protocol.ValidateEvent(event)
		}
		if err == nil && event.Identity != identity {
			err = errors.New("test event identity does not match its owned process")
		}
		if err != nil {
			reader.fail(fmt.Errorf("validate test event: %w", err))
			_, _ = io.Copy(io.Discard, buffered)
			return
		}
		select {
		case reader.events <- event:
		default:
			reader.fail(errors.New("test event consumer exceeded its bounded queue"))
			_, _ = io.Copy(io.Discard, buffered)
			return
		}
	}
}

func (reader *EventReader) fail(err error) {
	reader.mu.Lock()
	if reader.err == nil {
		reader.err = err
	}
	reader.mu.Unlock()
	reader.closeEvents()
}

func (reader *EventReader) closeEvents() {
	reader.terminalOnce.Do(func() { close(reader.events) })
}

func (reader *EventReader) closeSource() error {
	reader.closeOnce.Do(func() {
		if reader.source != nil {
			reader.closeErr = reader.source.Close()
		}
	})
	return reader.closeErr
}
