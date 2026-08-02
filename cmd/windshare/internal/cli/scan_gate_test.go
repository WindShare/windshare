package cli

import (
	"context"
	"errors"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/internal/testrun"
)

type catalogGateCloseFailure struct {
	err        error
	closeCalls int
}

type catalogGateAdmissionSequence struct {
	errors []error
	calls  int
}

func (gate *catalogGateAdmissionSequence) AdmitDirectoryScan(
	context.Context,
	catalog.ScanRequest,
) error {
	err := gate.errors[gate.calls]
	gate.calls++
	return err
}

func (*catalogGateAdmissionSequence) ListenerAddress() string {
	return "127.0.0.1:1"
}

func (*catalogGateAdmissionSequence) Close() error {
	return nil
}

func (*catalogGateCloseFailure) AdmitDirectoryScan(context.Context, catalog.ScanRequest) error {
	return nil
}

func (*catalogGateCloseFailure) ListenerAddress() string {
	return "127.0.0.1:1"
}

func (gate *catalogGateCloseFailure) Close() error {
	gate.closeCalls++
	return gate.err
}

type processTraceCloseFailure struct {
	err        error
	closeCalls int
}

func (*processTraceCloseFailure) WriteEvent(testrun.Event) error {
	return nil
}

func (sink *processTraceCloseFailure) Close() error {
	sink.closeCalls++
	return sink.err
}

func TestPrepareCatalogEnumerationGateZeroConfigReturnsTrueNilAdmission(t *testing.T) {
	admission, err := prepareCatalogEnumerationGate(nil, nil, []string{"share"})
	if err != nil {
		t.Fatal(err)
	}
	if admission != nil {
		t.Fatalf("zero-config scan admission has dynamic type %T, want nil interface", admission)
	}
}

func TestTracedCatalogGatePublishesReleaseAfterCanceledAttempt(t *testing.T) {
	lookup := func(name string) (string, bool) {
		value, present := map[string]string{
			testrun.RunIDEnvironment:       "catalog-gate-run",
			testrun.OperationIDEnvironment: "catalog-gate-operation",
			testrun.ScenarioEnvironment:    "catalog-gate-scenario",
		}[name]
		return value, present
	}
	sink := &recordingProcessTraceSink{}
	trace, err := newProcessTraceWithSink(
		lookup,
		func(testrun.Identity) (processTraceEventSink, error) { return sink, nil },
	)
	if err != nil {
		t.Fatal(err)
	}
	canceled := errors.New("receiver attempt canceled")
	admission := &catalogGateAdmissionSequence{errors: []error{canceled, nil}}
	gate := &tracedCatalogEnumerationGate{gate: admission, trace: trace}

	if err := gate.AdmitDirectoryScan(context.Background(), catalog.ScanRequest{}); !errors.Is(err, canceled) {
		t.Fatalf("first admission error = %v, want %v", err, canceled)
	}
	if sink.event.Milestone == string(processTraceCatalogScanReleased) {
		t.Fatal("canceled attempt published the global release milestone")
	}
	if err := gate.AdmitDirectoryScan(context.Background(), catalog.ScanRequest{}); err != nil {
		t.Fatal(err)
	}
	if sink.event.Milestone != string(processTraceCatalogScanReleased) ||
		sink.event.Outcome != string(testrun.OutcomeSucceeded) || admission.calls != 2 {
		t.Fatalf("release event=%+v admission_calls=%d", sink.event, admission.calls)
	}
}

func TestSettleCatalogGateSetupFailureJoinsGateAndTraceCleanupFailures(t *testing.T) {
	primaryErr := errors.New("setup failed")
	gateErr := errors.New("gate close failed")
	traceErr := errors.New("trace close failed")
	gate := &catalogGateCloseFailure{err: gateErr}
	sink := &processTraceCloseFailure{err: traceErr}
	trace := &processTrace{events: sink}

	err := settleCatalogGateSetupFailure(primaryErr, gate, trace)

	for _, expected := range []error{primaryErr, gateErr, traceErr} {
		if !errors.Is(err, expected) {
			t.Fatalf("settled error %v does not contain %v", err, expected)
		}
	}
	if gate.closeCalls != 1 || sink.closeCalls != 1 {
		t.Fatalf("cleanup calls gate=%d trace=%d, want one each", gate.closeCalls, sink.closeCalls)
	}
}
