package cli

import (
	"errors"
	"fmt"
	"sync"

	"github.com/windshare/windshare/cmd/wind/internal/commandmeta"
	"github.com/windshare/windshare/internal/testrun"
	"github.com/windshare/windshare/internal/testtrace"
)

const (
	processTraceShareComponent testrun.Component = commandmeta.Name + "_share"
	processTraceGetComponent   testrun.Component = commandmeta.Name + "_get"

	processTraceSenderReady          testrun.Milestone = "sender_ready"
	processTraceSenderDirectLane     testrun.Milestone = "sender_direct_lane"
	processTraceSenderSessionRetired testrun.Milestone = "sender_session_retired"
	processTraceSenderRelayRecovery  testrun.Milestone = "sender_relay_recovery"
	processTraceSenderStop           testrun.Milestone = "sender_stop"
	processTraceReceiverDirectLane   testrun.Milestone = "receiver_direct_lane"
	processTraceReceiverRelayContent testrun.Milestone = "receiver_relay_content"
	processTraceReceiverJoinStopped  testrun.Milestone = "receiver_join_stopped"
)

type processTraceEventSink interface {
	testrun.EventSink
	Close() error
}

type processTraceEventSinkOpener func(testrun.Identity) (processTraceEventSink, error)

// processTrace is dormant outside a correlated correctness-test child. Keeping
// the reporter on App makes concurrent peer and relay callbacks share the same
// private event sink and preserves production behavior when no test operation
// was propagated.
type processTrace struct {
	operation testrun.Operation
	events    processTraceEventSink
	recorders map[testrun.Component]*testrun.Recorder

	mu        sync.Mutex
	firstErr  error
	closeOnce sync.Once
	closeErr  error
}

func newProcessTrace(lookup testrun.EnvironmentLookup) (*processTrace, error) {
	return newProcessTraceWithSink(lookup, func(identity testrun.Identity) (processTraceEventSink, error) {
		return testtrace.OpenEventSink(identity)
	})
}

func newProcessTraceWithSink(
	lookup testrun.EnvironmentLookup,
	openSink processTraceEventSinkOpener,
) (*processTrace, error) {
	operation, present, err := testrun.OperationFromEnvironment(lookup)
	if err != nil {
		return nil, fmt.Errorf("%s: load test trace context: %w", commandmeta.Name, err)
	}
	if !present {
		return nil, nil
	}
	if openSink == nil {
		return nil, errors.New(commandmeta.Name + ": private test event sink opener is nil")
	}
	events, err := openSink(operation.EventIdentity())
	if err != nil {
		return nil, fmt.Errorf("%s: open private test event sink: %w", commandmeta.Name, err)
	}
	if events == nil {
		return nil, errors.New(commandmeta.Name + ": private test event sink opener returned nil")
	}
	recorders := make(map[testrun.Component]*testrun.Recorder, 2)
	for _, component := range []testrun.Component{processTraceShareComponent, processTraceGetComponent} {
		recorder, recorderErr := testrun.NewRecorder(operation, component, events)
		if recorderErr != nil {
			return nil, errors.Join(
				fmt.Errorf("%s: create %s test event recorder: %w", commandmeta.Name, component, recorderErr),
				events.Close(),
			)
		}
		recorders[component] = recorder
	}
	return &processTrace{operation: operation, events: events, recorders: recorders}, nil
}

func (trace *processTrace) record(
	component testrun.Component,
	milestone testrun.Milestone,
	outcome testrun.Outcome,
	context any,
) {
	if trace == nil {
		return
	}
	recorder, present := trace.recorders[component]
	if !present {
		trace.mu.Lock()
		trace.firstErr = errors.Join(
			trace.firstErr,
			fmt.Errorf("test event component %q is not registered", component),
		)
		trace.mu.Unlock()
		return
	}
	err := recorder.Record(milestone, outcome, context)
	if err == nil {
		return
	}
	trace.mu.Lock()
	trace.firstErr = errors.Join(trace.firstErr, err)
	trace.mu.Unlock()
}

func (trace *processTrace) close() error {
	if trace == nil {
		return nil
	}
	trace.closeOnce.Do(func() {
		trace.closeErr = errors.Join(trace.events.Close(), trace.err())
	})
	return trace.closeErr
}

func (trace *processTrace) err() error {
	if trace == nil {
		return nil
	}
	trace.mu.Lock()
	defer trace.mu.Unlock()
	return trace.firstErr
}

func (a *App) recordProcessTrace(
	component testrun.Component,
	milestone testrun.Milestone,
	outcome testrun.Outcome,
) {
	if a == nil {
		return
	}
	a.processTrace.record(component, milestone, outcome, nil)
}
