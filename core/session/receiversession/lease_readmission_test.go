package receiversession

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/session/revisionaccess"
	"github.com/windshare/windshare/core/transfer"
)

// Only the first directory item loses its lease. Its sibling uses the live
// sender, so a cleanup error promoted to job cancellation is visible here.
type expiringFileSource struct {
	revisionaccess.Source
	before, after content.FileRevisionDescriptor
	timeout       content.LeaseID
	opens         byte
	released      []content.LeaseID
}

const expiringLeasePrefix = 0xde

func testFileLease(n byte) content.LeaseID { return content.LeaseID{expiringLeasePrefix, n} }

func (s *expiringFileSource) OpenLease(ctx context.Context, file catalog.FileID) (revisionaccess.Lease, error) {
	if file != s.before.FileID() {
		return s.Source.OpenLease(ctx, file)
	}
	s.opens++
	descriptor := s.after
	if s.opens == 1 {
		descriptor = s.before
	}
	return revisionaccess.NewLease(testFileLease(s.opens), descriptor)
}

func (s *expiringFileSource) ReadLeaseRange(ctx context.Context, id content.LeaseID, descriptor content.FileRevisionDescriptor, part content.Range, sink transfer.RangeSink) error {
	if descriptor.FileID() != s.before.FileID() {
		return s.Source.ReadLeaseRange(ctx, id, descriptor, part, sink)
	}
	if id == testFileLease(1) {
		return content.ErrLeaseExpired
	}
	if descriptor != s.after {
		return transfer.ErrBlockIdentity
	}
	return sink.WriteRange(ctx, part.Offset, syntheticCapacityPayload[part.Offset:part.End])
}

func (s *expiringFileSource) ReleaseLease(ctx context.Context, id content.LeaseID) error {
	if id[0] != expiringLeasePrefix {
		return s.Source.ReleaseLease(ctx, id)
	}
	s.released = append(s.released, id)
	if id == s.timeout {
		return context.DeadlineExceeded
	}
	return nil
}

func TestLeaseReplacementIsolatesCleanupFromDirectoryTransfer(t *testing.T) {
	oldLease, newLease, readmittedLease := testFileLease(1), testFileLease(2), testFileLease(3)
	for _, tc := range []struct {
		name               string
		revision, metadata byte
		timeout            content.LeaseID
		durable            bool
		published          uint64
		opens              byte
		releases           []content.LeaseID
	}{
		{"same descriptor", 1, 1, oldLease, true, 2, 2, []content.LeaseID{oldLease, newLease}},
		{"metadata permits readmission", 2, 1, readmittedLease, true, 2, 3, []content.LeaseID{newLease, oldLease, readmittedLease}},
		{"metadata changed", 2, 2, newLease, true, 1, 2, []content.LeaseID{newLease, oldLease}},
		{"output cannot resume", 2, 1, newLease, false, 1, 2, []content.LeaseID{newLease, oldLease}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSyntheticCapacityFixture(t, 2)
			sender, receiver := connectVerticalPair(t, f.senderFactory, f.receiverFactory)
			defer sender.Close()
			defer receiver.Close()
			deps, err := receiver.TransferDependencies()
			if err != nil {
				t.Fatal(err)
			}
			file := syntheticCapacityID[catalog.FileID](10)
			geometry, _ := content.NewFileGeometry(uint64(len(syntheticCapacityPayload)), catalog.MinChunkSize)
			descriptor := func(revision, metadata byte) content.FileRevisionDescriptor {
				d, err := content.NewFileRevisionDescriptor(f.share, file, content.FileRevision{revision}, geometry, catalog.ModifiedTime{})
				if err != nil {
					t.Fatal(err)
				}
				return content.BindRevisionMetadata(d, content.SourceMetadata{metadata})
			}
			source := &expiringFileSource{
				Source: receiverLeaseDependencies{deps}, before: descriptor(1, 1),
				after: descriptor(tc.revision, tc.metadata), timeout: tc.timeout,
			}
			receiver.access.Stop()
			receiver.access = revisionaccess.New(receiver.Lifetime(), source)
			intent := syntheticCapacityReceiveIntent(t, f.share, f.syntheticRoot)
			var output transfer.DirectTreeMaterializer = newRevisionTransferOutput(t)
			if !tc.durable {
				output = newSyntheticCapacityOutput(t, intent)
			}
			id, err := transfer.NewTransferJobID()
			if err != nil {
				t.Fatal(err)
			}
			job, err := receiver.NewTransferJob(intent, id, output, nil)
			if err != nil {
				t.Fatal(err)
			}
			result := job.Run(t.Context())
			wantOutcome := transfer.DirectTreeOutcomeSuccess
			if tc.published == 1 {
				wantOutcome = transfer.DirectTreeOutcomePartial
			}
			if result.Outcome != wantOutcome || result.SucceededFiles != tc.published || result.TerminationCause != nil || receiver.Lifetime().Err() != nil {
				t.Fatalf("job result=%+v, session=%v", result, receiver.Lifetime().Err())
			}
			if source.opens != tc.opens || !slices.Equal(source.released, tc.releases) {
				t.Fatalf("target opens=%d releases=%v, want %d opens and %v", source.opens, source.released, tc.opens, tc.releases)
			}
			if failures := result.RevisionCleanupFailures; len(failures) != 1 || failures[0].FileID != file ||
				failures[0].Interruption != transfer.TransferInterruptionDeadline || !errors.Is(failures[0].Cause, context.DeadlineExceeded) {
				t.Fatalf("cleanup diagnostics=%+v", failures)
			}
		})
	}
}
