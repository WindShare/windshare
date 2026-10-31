package nativepeer

import (
	"math"
	"net/netip"
	"testing"
	"time"

	"github.com/windshare/windshare/connectivity/pionpeer"
	"github.com/windshare/windshare/connectivity/socketauthority"
	"github.com/windshare/windshare/connectivity/throughput"
)

func TestDiagnosticSeparatesSocketTimeoutFromProviderFailure(t *testing.T) {
	at := time.Unix(12, 34)
	source := Observation{Subject: Subject{ProtocolSessionID: [16]byte{1}, PeerPathID: [16]byte{2}, AttemptID: [16]byte{3}, Side: SideReceiver}, Socket: &socketauthority.Event{At: at, Kind: "stun_transaction", Local: netip.MustParseAddrPort("127.0.0.1:1111"), Server: netip.MustParseAddrPort("127.0.0.1:2222"), Result: "cancelled", Duration: 250 * time.Millisecond}}
	record := source.DiagnosticRecord()
	p := record.Payload()
	if !record.ObservedAt().Equal(at) || p.Get("socket").Get("result").Text() != "cancelled" || p.Get("socket").Get("duration_ms").Uint() != 250 || p.Get("socket").Get("local_endpoint").Text() != "127.0.0.1:1111" {
		t.Fatal("socket cancellation reclassified")
	}
	source.Socket = nil
	source.Provider = &pionpeer.Event{At: at, Milestone: "provider_diagnostic", Diagnostic: &pionpeer.Diagnostic{Scope: "ice", Severity: "warning", Message: "unknown provider evidence"}}
	record = source.DiagnosticRecord()
	source.Provider.Diagnostic.Message = "mutated"
	if record.Payload().Get("diagnostic").Get("message").Text() != "unknown provider evidence" {
		t.Fatal("provider message borrowed")
	}
}
func TestDiagnosticDistinguishesUsefulWorkFromNetworkDemand(t *testing.T) {
	source := Observation{Parallelism: &ParallelismFacts{Decision: throughput.Decision{UsefulRate: 123, OutputRate: 456, RetryAfter: 30 * time.Second}, Demand: throughput.Observation{NetworkWaitingBytes: math.MaxUint64, InFlightBytes: 10, RemainingBytes: 30, RemainingKnown: true, CapacityWaitingBytes: 20}}}
	p := source.DiagnosticRecord().Payload().Get("parallelism")
	if p.Get("network_waiting_bytes").Uint() != math.MaxUint64 || p.Get("capacity_waiting_bytes").Uint() != 20 || p.Get("useful_rate").Uint() != 123 || p.Get("output_rate").Uint() != 456 || p.Get("retry_after_ms").Uint() != 30000 {
		t.Fatal("demand and measured delivery conflated")
	}
}
