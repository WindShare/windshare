package cli

import (
	"context"
	"errors"
	"net"
	"strconv"
	"sync"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/liveshare"
	"github.com/windshare/windshare/internal/testrun"
)

const (
	processTraceCatalogGateReady    testrun.Milestone = "catalog_gate_ready"
	processTraceCatalogScanWaiting  testrun.Milestone = "catalog_scan_waiting"
	processTraceCatalogScanReleased testrun.Milestone = "catalog_scan_released"
)

// CatalogEnumerationGate is consumed only by the dedicated process entry. Its
// control address is published through the authenticated owner event sink and
// never becomes a production CLI or environment capability.
type CatalogEnumerationGate interface {
	liveshare.DirectoryScanAdmission
	ListenerAddress() string
	Close() error
}

type tracedCatalogEnumerationGate struct {
	gate  CatalogEnumerationGate
	trace *processTrace

	waiting  sync.Once
	released sync.Once
}

func prepareCatalogEnumerationGate(
	trace *processTrace,
	gate CatalogEnumerationGate,
	args []string,
) (liveshare.DirectoryScanAdmission, error) {
	if gate == nil {
		return nil, nil
	}
	if trace == nil || len(args) == 0 || args[0] != "share" {
		return nil, errors.New("windshare: catalog enumeration gate requires an owned share operation")
	}
	address, err := canonicalLoopbackListenerAddress(gate.ListenerAddress())
	if err != nil {
		return nil, err
	}
	trace.record(
		processTraceShareComponent,
		processTraceCatalogGateReady,
		testrun.OutcomeSucceeded,
		&testrun.ListenerReadyContext{Address: address},
	)
	if err := trace.err(); err != nil {
		return nil, errors.New("windshare: publish private catalog gate readiness")
	}
	return &tracedCatalogEnumerationGate{gate: gate, trace: trace}, nil
}

func (gate *tracedCatalogEnumerationGate) AdmitDirectoryScan(
	ctx context.Context,
	request catalog.ScanRequest,
) error {
	gate.waiting.Do(func() {
		gate.trace.record(
			processTraceShareComponent,
			processTraceCatalogScanWaiting,
			testrun.OutcomeStarted,
			nil,
		)
	})
	if err := gate.trace.err(); err != nil {
		return errors.New("windshare: publish private catalog scan wait evidence")
	}
	err := gate.gate.AdmitDirectoryScan(ctx, request)
	if err == nil {
		// A canceled receiver attempt does not settle the global gate. Publish the
		// release milestone only when an admitted scan actually crosses it.
		gate.released.Do(func() {
			gate.trace.record(
				processTraceShareComponent,
				processTraceCatalogScanReleased,
				testrun.OutcomeSucceeded,
				nil,
			)
		})
	}
	if traceErr := gate.trace.err(); traceErr != nil {
		return errors.Join(err, errors.New("windshare: publish private catalog scan release evidence"))
	}
	return err
}

func canonicalLoopbackListenerAddress(value string) (string, error) {
	host, portText, err := net.SplitHostPort(value)
	if err != nil || net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback() {
		return "", errors.New("windshare: catalog enumeration gate listener is not loopback")
	}
	port, err := strconv.ParseUint(portText, 10, 16)
	if err != nil || port == 0 || net.JoinHostPort(host, strconv.FormatUint(port, 10)) != value {
		return "", errors.New("windshare: catalog enumeration gate listener is invalid")
	}
	return value, nil
}

func settleCatalogGateSetupFailure(
	primary error,
	gate CatalogEnumerationGate,
	trace *processTrace,
) error {
	var gateErr error
	if gate != nil {
		gateErr = gate.Close()
	}
	return errors.Join(primary, gateErr, trace.close())
}
