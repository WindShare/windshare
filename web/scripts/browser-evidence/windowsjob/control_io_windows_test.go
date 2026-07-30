//go:build windows

package main

import (
	"bytes"
	"reflect"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

func TestReadControlRequestsAuthenticatesTheOnlyTerminationAuthority(t *testing.T) {
	start := validStartRequest(t)
	control := terminateRequest{
		SchemaVersion: protocolSchemaVersion,
		Type:          requestTypeTerminate,
		OperationID:   start.OperationID,
		Nonce:         start.Nonce,
		Reason:        terminateReasonParentRequest,
	}
	var framed bytes.Buffer
	if err := writeCanonicalFrame(&framed, control); err != nil {
		t.Fatal(err)
	}
	if result := <-readControlRequests(&framed, start); result.err != nil ||
		!reflect.DeepEqual(result.request, control) {
		t.Fatalf("authenticated control result=%#v", result)
	}

	if result := <-readControlRequests(bytes.NewReader(nil), start); result.err == nil ||
		!strings.Contains(result.err.Error(), "parent control channel disconnected") {
		t.Fatalf("disconnected parent result=%#v", result)
	}

	hostile := control
	hostile.OperationID += "-other"
	framed.Reset()
	if err := writeCanonicalFrame(&framed, hostile); err != nil {
		t.Fatal(err)
	}
	if result := <-readControlRequests(&framed, start); result.err == nil ||
		!strings.Contains(result.err.Error(), "identity does not match") {
		t.Fatalf("cross-operation control result=%#v", result)
	}
}

func TestReadExactRawStdinOwnsItsDeclaredPipeFraming(t *testing.T) {
	if payload, err := readExactRawStdin(0, nil); err != nil || payload != nil {
		t.Fatalf("absent raw stdin payload=%v err=%v", payload, err)
	}
	if _, err := readExactRawStdin(1, nil); err == nil {
		t.Fatal("undeclared raw stdin handle was accepted")
	}
	if _, err := readExactRawStdin(0, &rawStdin{ByteLength: 1}); err == nil {
		t.Fatal("declared raw stdin omitted its handle")
	}

	exact := []byte{0, 1, 2, 0xff}
	if payload, err := readRawStdinPipe(t, exact, int64(len(exact))); err != nil ||
		!bytes.Equal(payload, exact) {
		t.Fatalf("exact raw stdin payload=%v err=%v", payload, err)
	}
	if _, err := readRawStdinPipe(t, exact[:2], int64(len(exact))); err == nil ||
		!strings.Contains(err.Error(), "read declared raw stdin bytes") {
		t.Fatalf("short raw stdin err=%v", err)
	}
	if _, err := readRawStdinPipe(t, exact, int64(len(exact)-1)); err == nil ||
		!strings.Contains(err.Error(), "exceeds its declared byte length") {
		t.Fatalf("overlong raw stdin err=%v", err)
	}
}

func TestRawEOFAndLauncherEventsRejectAmbiguousFraming(t *testing.T) {
	if err := requireExactRawEOF(bytes.NewReader(nil)); err != nil {
		t.Fatal(err)
	}
	if err := requireExactRawEOF(bytes.NewReader([]byte{1})); err == nil {
		t.Fatal("undeclared trailing byte was accepted")
	}
	if err := requireExactRawEOF(readerWithoutEOF{}); err == nil {
		t.Fatal("zero-byte non-EOF read was accepted")
	}

	tooLong := strings.Repeat("x", maximumDiagnosticBytes+1)
	tests := []struct {
		name  string
		event launcherEvent
	}{
		{
			name: "schema",
			event: launcherEvent{
				SchemaVersion: protocolSchemaVersion + 1,
				Type:          launcherEventRootStarted, PID: 1, ProcessHandle: 1,
			},
		},
		{
			name: "root identity",
			event: launcherEvent{
				SchemaVersion: protocolSchemaVersion,
				Type:          launcherEventRootStarted,
			},
		},
		{
			name: "spawn identity",
			event: launcherEvent{
				SchemaVersion: protocolSchemaVersion,
				Type:          launcherEventSpawnFailed,
			},
		},
		{
			name: "spawn diagnostic",
			event: launcherEvent{
				SchemaVersion: protocolSchemaVersion,
				Type:          launcherEventSpawnFailed,
				SpawnFailure:  &tooLong,
			},
		},
		{
			name: "type",
			event: launcherEvent{
				SchemaVersion: protocolSchemaVersion,
				Type:          "other",
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var framed bytes.Buffer
			if err := writeCanonicalFrame(&framed, test.event); err != nil {
				t.Fatal(err)
			}
			if _, err := readLauncherEvent(&framed); err == nil {
				t.Fatal("ambiguous launcher event was accepted")
			}
		})
	}
}

func readRawStdinPipe(t *testing.T, payload []byte, declaredLength int64) ([]byte, error) {
	t.Helper()
	var reader windows.Handle
	var writer windows.Handle
	if err := windows.CreatePipe(&reader, &writer, nil, 0); err != nil {
		t.Fatal(err)
	}
	remaining := payload
	for len(remaining) != 0 {
		var written uint32
		if err := windows.WriteFile(writer, remaining, &written, nil); err != nil {
			_ = windows.CloseHandle(reader)
			_ = windows.CloseHandle(writer)
			t.Fatal(err)
		}
		if written == 0 {
			_ = windows.CloseHandle(reader)
			_ = windows.CloseHandle(writer)
			t.Fatal("raw stdin test pipe made no write progress")
		}
		remaining = remaining[written:]
	}
	if err := windows.CloseHandle(writer); err != nil {
		_ = windows.CloseHandle(reader)
		t.Fatal(err)
	}
	return readExactRawStdin(uintptr(reader), &rawStdin{ByteLength: declaredLength})
}

type readerWithoutEOF struct{}

func (readerWithoutEOF) Read([]byte) (int, error) { return 0, nil }
