package diagnostics

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/windshare/windshare/core/diagnosticerror"
)

func TestFailureRetainsJoinedCausesAndCaptureLocation(t *testing.T) {
	if Failure(diagnosticerror.Snapshot{}).Present() {
		t.Fatal("absent failure became present")
	}
	snapshot := diagnosticerror.Capture(errors.Join(errors.New("read failed"), errors.New("close failed")), "source.shutdown")
	record := New("failure", Fields{"failure": Failure(snapshot)})
	nodes := snapshot.Nodes()
	nodes[0].Message = "mutated"
	raw, err := json.Marshal(record.Payload())
	if err != nil {
		t.Fatal(err)
	}
	var p struct {
		Failure struct {
			Source    string
			Available bool `json:"error_available"`
			Nodes     []struct {
				Message string
				Parent  *string `json:"parent_index"`
			}
			Stack []struct{ Function string } `json:"capture_stack"`
		}
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatal(err)
	}
	if p.Failure.Source != "source.shutdown" || !p.Failure.Available || len(p.Failure.Nodes) != 3 || p.Failure.Nodes[1].Message != "read failed" || p.Failure.Nodes[2].Message != "close failed" || p.Failure.Nodes[0].Parent != nil || *p.Failure.Nodes[1].Parent != "0" || len(p.Failure.Stack) == 0 {
		t.Fatalf("failure topology lost: %s", raw)
	}
}
func TestCorrelationEnrichmentPreservesOriginalAndScope(t *testing.T) {
	original := New("event", Fields{}).Correlate(Fields{"operation": Text("op")})
	enriched := original.Correlate(Fields{"session": Text("session")})
	if original.Correlation().Get("session").Present() || enriched.Correlation().Get("operation").Text() != "op" || enriched.Correlation().Get("session").Text() != "session" {
		t.Fatal("correlation merge changed source")
	}
	if _, err := NewStream(-1, nil); err == nil {
		t.Fatal("negative capacity accepted")
	}
}
