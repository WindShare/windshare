package cli

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync"

	ownerprotocol "github.com/windshare/windshare/internal/processowner/protocol"
	"github.com/windshare/windshare/internal/testrun"
)

const (
	testLifecycleControlActionStop = "stop"
	testLifecycleControlMaxBytes   = 1024
)

type testLifecycleControlRequest struct {
	RunID       string `json:"run_id"`
	OperationID string `json:"operation_id"`
	Scenario    string `json:"scenario"`
	Action      string `json:"action"`
}

type testLifecycleControl struct {
	listener net.Listener
	expected testLifecycleControlRequest
	trace    *processTrace
	stop     chan<- os.Signal
	done     chan struct{}

	mu         sync.Mutex
	connection net.Conn
	closeOnce  sync.Once
	closeErr   error
}

func startTestLifecycleControl(trace *processTrace, stop chan<- os.Signal) (*testLifecycleControl, error) {
	if trace == nil || stop == nil {
		return nil, errors.New("windshare: test lifecycle control requires trace identity and stop delivery")
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("windshare: listen for test lifecycle control: %w", err)
	}
	request := testLifecycleControlRequest{
		RunID: trace.operation.RunID(), OperationID: trace.operation.ID(),
		Scenario: trace.operation.Scenario(), Action: testLifecycleControlActionStop,
	}
	control := &testLifecycleControl{
		listener: listener,
		expected: request,
		trace:    trace,
		stop:     stop,
		done:     make(chan struct{}),
	}
	trace.record(
		processTraceShareComponent,
		processTraceLifecycleControlReady,
		testrun.OutcomeSucceeded,
		&testrun.ListenerReadyContext{Address: listener.Addr().String()},
	)
	if err := trace.err(); err != nil {
		_ = listener.Close()
		return nil, fmt.Errorf("windshare: publish test lifecycle control readiness: %w", err)
	}
	go control.serve()
	return control, nil
}

func (control *testLifecycleControl) serve() {
	defer close(control.done)
	connection, err := control.listener.Accept()
	if err != nil {
		return
	}
	control.mu.Lock()
	control.connection = connection
	control.mu.Unlock()
	defer func() {
		control.mu.Lock()
		control.connection = nil
		control.mu.Unlock()
		_ = connection.Close()
	}()

	document, err := io.ReadAll(io.LimitReader(connection, testLifecycleControlMaxBytes+1))
	decodeErr := validateTestLifecycleControl(document, control.expected)
	if err != nil || decodeErr != nil {
		control.trace.record(
			processTraceShareComponent,
			processTraceLifecycleControlStop,
			testrun.OutcomeFailed,
			nil,
		)
		return
	}
	control.trace.record(
		processTraceShareComponent,
		processTraceLifecycleControlStop,
		testrun.OutcomeSucceeded,
		nil,
	)
	control.stop <- os.Interrupt
}

func validateTestLifecycleControl(document []byte, expected testLifecycleControlRequest) error {
	if len(document) > testLifecycleControlMaxBytes {
		return errors.New("windshare: test lifecycle control request is oversized")
	}
	request, err := ownerprotocol.DecodeLine[testLifecycleControlRequest](document)
	if err != nil {
		return fmt.Errorf("windshare: decode test lifecycle control request: %w", err)
	}
	if request != expected {
		return errors.New("windshare: test lifecycle control identity or action does not match")
	}
	return nil
}

func (control *testLifecycleControl) Close() error {
	if control == nil {
		return nil
	}
	control.closeOnce.Do(func() {
		control.closeErr = control.listener.Close()
		control.mu.Lock()
		if control.connection != nil {
			control.closeErr = errors.Join(control.closeErr, control.connection.Close())
		}
		control.mu.Unlock()
		<-control.done
	})
	if errors.Is(control.closeErr, net.ErrClosed) {
		return nil
	}
	return control.closeErr
}
