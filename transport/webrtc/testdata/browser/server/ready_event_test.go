package main

import (
	"bytes"
	"encoding/json"
	"net"
	"testing"
)

func TestInteropReadyReporterPublishesActualAddressAndStableIdentity(t *testing.T) {
	var output bytes.Buffer
	reporter, err := interopReadyReporter(&output, func(name string) string {
		switch name {
		case traceScenarioEnvName:
			return "weekly-pion-interop"
		case traceOperationEnvName:
			return "pion-server-1"
		default:
			return ""
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	address := &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 49_231}
	if err := reporter(address); err != nil {
		t.Fatal(err)
	}
	var record interopReadyRecord
	if err := json.Unmarshal(output.Bytes(), &record); err != nil {
		t.Fatal(err)
	}
	if record.Component != interopReadyComponent || record.ScenarioID != "weekly-pion-interop" ||
		record.OperationID != "pion-server-1" || record.Milestone != "listener-ready" ||
		record.Address != address.String() {
		t.Fatalf("ready record = %+v", record)
	}
}

func TestInteropReadyReporterUsesDirectRunDefaults(t *testing.T) {
	var output bytes.Buffer
	reporter, err := interopReadyReporter(&output, func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	if err := reporter(&net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 49_231}); err != nil {
		t.Fatal(err)
	}
	var record interopReadyRecord
	if err := json.Unmarshal(output.Bytes(), &record); err != nil {
		t.Fatal(err)
	}
	if record.ScenarioID != defaultTraceScenario || record.OperationID != defaultTraceOperation {
		t.Fatalf("default identity = %+v", record)
	}
}

func TestInteropReadyReporterRejectsInvalidInputs(t *testing.T) {
	if _, err := interopReadyReporter(nil, func(string) string { return "" }); err == nil {
		t.Fatal("readiness accepted a nil writer")
	}
	if _, err := interopReadyReporter(&bytes.Buffer{}, func(string) string { return "INVALID" }); err == nil {
		t.Fatal("readiness accepted an invalid trace identity")
	}
	reporter, err := interopReadyReporter(&bytes.Buffer{}, func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	if err := reporter(&net.TCPAddr{IP: net.IPv4(192, 0, 2, 1), Port: 49_231}); err == nil {
		t.Fatal("readiness accepted a non-loopback listener")
	}
	if err := reporter(&net.TCPAddr{IP: net.IPv6loopback, Port: 49_231}); err == nil {
		t.Fatal("readiness accepted an IPv6 listener for its IPv4 endpoint")
	}
}
