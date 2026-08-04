package testprocess

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"

	"github.com/windshare/windshare/internal/testrun"
)

const MaximumCapturedEvents = 1024

type Event = testrun.Event

type EventReader struct {
	events chan Event
	done   chan struct{}

	mu  sync.Mutex
	err error
}

func newEventReader(source io.ReadCloser) *EventReader {
	reader := &EventReader{events: make(chan Event, MaximumCapturedEvents), done: make(chan struct{})}
	go reader.consume(source)
	return reader
}

func (reader *EventReader) consume(source io.ReadCloser) {
	defer close(reader.done)
	defer close(reader.events)
	defer func() { reader.setError(source.Close()) }()
	scanner := bufio.NewScanner(source)
	scanner.Buffer(make([]byte, 4096), testrun.MaximumEventDocumentBytes+1)
	for scanner.Scan() {
		event, err := decodeEvent(scanner.Bytes())
		if err != nil {
			reader.setError(err)
			continue
		}
		select {
		case reader.events <- event:
		default:
			reader.setError(errors.New("test process event history exceeded its bound"))
		}
	}
	reader.setError(scanner.Err())
}

func decodeEvent(encoded []byte) (Event, error) {
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	var event Event
	if err := decoder.Decode(&event); err != nil {
		return Event{}, fmt.Errorf("decode test process event: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return Event{}, errors.New("test process event contains trailing data")
	}
	if err := testrun.ValidateEvent(event); err != nil {
		return Event{}, fmt.Errorf("validate test process event: %w", err)
	}
	event.Payload = bytes.Clone(event.Payload)
	return event, nil
}

func (reader *EventReader) Next(ctx context.Context) (Event, error) {
	select {
	case event, open := <-reader.events:
		if open {
			event.Payload = bytes.Clone(event.Payload)
			return event, nil
		}
		if err := reader.Err(); err != nil {
			return Event{}, err
		}
		return Event{}, io.EOF
	case <-ctx.Done():
		return Event{}, ctx.Err()
	}
}

func (reader *EventReader) Done() <-chan struct{} { return reader.done }

func (reader *EventReader) Err() error {
	reader.mu.Lock()
	defer reader.mu.Unlock()
	return reader.err
}

func (reader *EventReader) setError(err error) {
	// The lifecycle join closes a stalled event endpoint to preserve its bound;
	// that expected cancellation must not obscure the causal timeout diagnostic.
	if err == nil || errors.Is(err, os.ErrClosed) {
		return
	}
	reader.mu.Lock()
	reader.err = errors.Join(reader.err, err)
	reader.mu.Unlock()
}
