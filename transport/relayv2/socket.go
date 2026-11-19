package relayv2

import (
	"context"
	"errors"

	"github.com/coder/websocket"
)

// BinarySocket is the narrow WebSocket surface owned by this protocol
// consumer. A coder WebSocket satisfies it directly.
type BinarySocket interface {
	Read(context.Context) (websocket.MessageType, []byte, error)
	Write(context.Context, websocket.MessageType, []byte) error
	Close(websocket.StatusCode, string) error
	SetReadLimit(int64)
}

// SocketError exposes a closed transport while retaining the I/O cause for diagnostics.
type SocketError struct {
	Cause error
}

func (err *SocketError) Error() string { return ErrClosed.Error() }

func (err *SocketError) Is(target error) bool { return err != nil && target == ErrClosed }

func (err *SocketError) Unwrap() error {
	if err == nil {
		return nil
	}
	return err.Cause
}

func socketFailure(err error) error {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return &SocketError{Cause: err}
}

func readBinary(ctx context.Context, socket BinarySocket) ([]byte, error) {
	kind, encoded, err := socket.Read(ctx)
	if err != nil {
		return nil, socketFailure(err)
	}
	if kind != websocket.MessageBinary {
		return nil, ErrProtocol
	}
	return encoded, nil
}

func writeBinary(ctx context.Context, socket BinarySocket, encoded []byte) error {
	return socketFailure(socket.Write(ctx, websocket.MessageBinary, encoded))
}
