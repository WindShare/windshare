package relayv2_test

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/diagnostics"
	"github.com/windshare/windshare/core/session/receivercontinuation"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/transport/relayv2"
)

type receiveRecoveryEvidence struct {
	cutAt             time.Time
	timeToResume      time.Duration
	receivedBeforeCut uint64
	redownloadedBytes uint64
	postCutRevisions  []content.FileRevision
	readmission       transfer.RevisionReadmissionTrace
}

func (evidence *receiveRecoveryEvidence) observeDiagnostic(rec diagnostics.Record) {
	payload := rec.Payload()
	if payload.Get("stage").Text() != transfer.TransferFileRevisionReadmission.DiagnosticName() {
		return
	}
	rr := payload.Get("revision_readmission")
	if rr.Get("decision").Text() != "readmitted" {
		return
	}
	prevBytes, _ := base64.RawURLEncoding.DecodeString(rr.Get("previous_revision").Text())
	currBytes, _ := base64.RawURLEncoding.DecodeString(rr.Get("current_revision").Text())
	var prev, curr content.FileRevision
	copy(prev[:], prevBytes)
	copy(curr[:], currBytes)
	evidence.readmission = transfer.RevisionReadmissionTrace{
		Previous:         prev,
		Current:          curr,
		Decision:         "readmitted",
		ComparisonReason: content.RevisionComparisonReason(rr.Get("comparison_reason").Text()),
		Retained:         rr.Get("retained_bytes").Uint(),
		Reused:           rr.Get("reused_bytes").Uint(),
	}
}

func (evidence *receiveRecoveryEvidence) validate() error {
	readmission := evidence.readmission
	if evidence.cutAt.IsZero() || evidence.receivedBeforeCut == 0 || evidence.timeToResume <= 0 ||
		readmission.Previous.IsZero() || readmission.Current.IsZero() || readmission.Current == readmission.Previous ||
		readmission.ComparisonReason != content.ComparisonMetadataAgreement ||
		readmission.Retained > evidence.receivedBeforeCut || readmission.Reused > readmission.Retained ||
		readmission.Reused+evidence.redownloadedBytes != evidence.receivedBeforeCut {
		return fmt.Errorf("incomplete reopened transfer evidence: received before cut=%d, resume=%v, readmission=%+v, redownloaded=%d",
			evidence.receivedBeforeCut, evidence.timeToResume, readmission, evidence.redownloadedBytes)
	}
	for _, rev := range evidence.postCutRevisions {
		if rev != readmission.Current {
			return errors.New("received payload before the new revision was readmitted")
		}
	}
	return nil
}

// The single-file job supplies contiguous requests and acknowledges each one
// before asking for the next. Cut at that boundary so the received prefix is
// exact and neither races with a writer nor depends on the machine's speed.
type reopeningEvidenceReader struct {
	*receivercontinuation.Session
	initial  *relayv2.ReceiverConnection
	recovery *receiveRecoveryEvidence
}

func (reader *reopeningEvidenceReader) ReadRange(
	ctx context.Context, lease transfer.RevisionHandle, descriptor content.FileRevisionDescriptor,
	requested content.Range, sink transfer.RangeSink,
) error {
	evidence := reader.recovery
	if evidence.cutAt.IsZero() && requested.Offset != 0 {
		if requested.Offset != evidence.receivedBeforeCut {
			return fmt.Errorf("first received prefix ends at %d but the next request starts at %d",
				evidence.receivedBeforeCut, requested.Offset)
		}
		current, err := reader.Session.Runtime().TransferDependencies()
		if err != nil {
			return err
		}
		// A normal detach keeps the old handle warm. Retire it through the existing
		// protocol before cutting the socket to model a reopen without a grace sleep.
		if err := current.ReleaseRevision(ctx, lease); err != nil {
			return fmt.Errorf("retire initial revision before the cut: %w", err)
		}
		evidence.cutAt = time.Now()
		if err := reader.initial.Close(); err != nil {
			return err
		}
	}
	return reader.Session.ReadRange(ctx, lease, descriptor, requested, transfer.RangeSinkFunc(func(ctx context.Context, offset uint64, data []byte) error {
		if err := sink.WriteRange(ctx, offset, data); err != nil {
			return err
		}
		return evidence.observePayload(descriptor.FileRevision(), offset, uint64(len(data)))
	}))
}

func (evidence *receiveRecoveryEvidence) observePayload(revision content.FileRevision, offset, length uint64) error {
	if evidence.cutAt.IsZero() {
		evidence.receivedBeforeCut += length
		return nil
	}
	if !evidence.readmission.Current.IsZero() && revision != evidence.readmission.Current {
		return errors.New("received payload before the new revision was readmitted")
	}
	evidence.postCutRevisions = append(evidence.postCutRevisions, revision)
	if evidence.timeToResume == 0 {
		evidence.timeToResume = time.Since(evidence.cutAt)
	}
	// Only the intersection with the received prefix was downloaded before.
	// A durable checkpoint and the network can account for different bytes.
	if offset < evidence.receivedBeforeCut {
		evidence.redownloadedBytes += min(length, evidence.receivedBeforeCut-offset)
	}
	return nil
}

func TestReceiveRecoveryEvidenceCountsOnlyPreviouslyReceivedOffsets(t *testing.T) {
	const prefixBytes = 8
	const blockBytes = 4
	const repeatedBytes = 2
	current := content.FileRevision{2}
	evidence := receiveRecoveryEvidence{
		cutAt: time.Now().Add(-time.Millisecond), receivedBeforeCut: prefixBytes,
		readmission: transfer.RevisionReadmissionTrace{Current: current},
	}
	if err := evidence.observePayload(current, prefixBytes-repeatedBytes, blockBytes); err != nil {
		t.Fatal(err)
	}
	firstReceipt := evidence.timeToResume
	if err := evidence.observePayload(current, prefixBytes+repeatedBytes, blockBytes); err != nil {
		t.Fatal(err)
	}
	if evidence.redownloadedBytes != repeatedBytes || firstReceipt <= 0 || evidence.timeToResume != firstReceipt {
		t.Fatalf("redownloaded=%d first receipt=%v resume=%v", evidence.redownloadedBytes, firstReceipt, evidence.timeToResume)
	}
}
