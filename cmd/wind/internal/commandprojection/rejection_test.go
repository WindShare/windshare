package commandprojection

import (
	"errors"
	"strings"
	"testing"

	"github.com/windshare/windshare/cmd/wind/internal/clievent"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/session/sessionruntime"
)

func TestProjectionRejectionPreservesRuleAndSourceOperands(t *testing.T) {
	context := clievent.CaptureObservationRejection(clievent.ObservationRejection{
		Event: "protocol_operation", Source: "commandprojection.ProjectProtocolObservation", Stage: "sender_request_received",
		Session: "00000000000000000000000000000000", Operation: "invalid raw operation",
	}, clievent.RejectedBool("has_lane", false), clievent.RejectedUint("lane_id", 7), clievent.RejectedUint("lane_epoch", 0))
	err := withRejectionContext(clievent.EventContractError{Field: "lane", Rule: "presence_matches_identity"}, context)
	sample := ProjectionRejection(err)
	if !sample.Valid() || sample.Field != "lane" || sample.Rule != "presence_matches_identity" ||
		sample.Event != context.Event || sample.Source != context.Source || sample.Session != context.Session ||
		len(sample.Evidence()) != 3 {
		t.Fatalf("sample = %+v", sample)
	}
	if sample.Evidence()[1].Value != "7" || sample.Evidence()[2].Value != "0" {
		t.Fatal("conflicting operands were erased")
	}
}

func TestProjectionRejectionSnapshotBoundsAndFallback(t *testing.T) {
	context := clievent.ObservationRejection{Stage: "sender_request_received"}
	oversized := withRejectionContext(rejectedProjection(ProjectionEventContract, strings.Repeat("x", 1000), "rule"), context)
	sample := ProjectionRejection(oversized)
	if !sample.Valid() || len(sample.Field) != 96 || !sample.Truncated() || sample.OmittedBytes() != 904 {
		t.Fatalf("unbounded sample = %+v", sample)
	}
	if fallback := ProjectionRejection(errors.New("provider object text")); !fallback.Valid() ||
		fallback.Rule != "projection_contract" || len(fallback.Evidence()) != 0 {
		t.Fatalf("fallback = %+v", fallback)
	}
}

func TestIdentityRejectionRetainsRawBytesBeforeConversion(t *testing.T) {
	raw := []byte{0, 1, 255}
	_, err := RelaySessionID(raw)
	raw[0] = 3
	sample := ProjectionRejection(err)
	if !errors.Is(err, ErrInvalidProjection) || ObserverLossReason(err) != clievent.ObserverLossInvalidIdentity ||
		!sample.Valid() || sample.Source != "commandprojection.RelaySessionID" ||
		len(sample.Evidence()) != 1 || sample.Evidence()[0].Value != "0001ff" {
		t.Fatalf("identity evidence = %+v", sample)
	}
	_, err = ProtocolSessionID(protocolsession.ProtocolSessionID{})
	sample = ProjectionRejection(err)
	if sample.Evidence()[0].Value != strings.Repeat("0", 32) {
		t.Fatal("invalid zero identity was erased")
	}
	_, err = LaneIdentity(sessionruntime.LaneIdentity{Epoch: 9})
	sample = ProjectionRejection(err)
	if sample.Field != "lane" || len(sample.Evidence()) != 2 || sample.Evidence()[0].Value != "0" || sample.Evidence()[1].Value != "9" {
		t.Fatalf("lane evidence = %+v", sample)
	}
}

func TestSenderRevisionRejectionCapturesOriginalZeroValues(t *testing.T) {
	_, err := ProjectSenderRevision(content.RevisionTrace{})
	sample := ProjectionRejection(err)
	if !sample.Valid() || sample.Event != "sender_revision" || sample.Source != "commandprojection.ProjectSenderRevision" ||
		sample.Field != "stage" || sample.Stage != "unknown" || sample.Rule != "known_enum" {
		t.Fatalf("revision rejection = %+v", sample)
	}
	evidence := sample.Evidence()
	if len(evidence) != 7 || evidence[0].Representation != "enum_number" || evidence[0].Value != "0" ||
		evidence[2].Representation != "identity_hex" || evidence[2].Value != strings.Repeat("0", 32) ||
		evidence[6].Field != "protocol_session_id" || evidence[6].Value != "" {
		t.Fatalf("original revision values = %#v", evidence)
	}
}
