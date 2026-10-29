package outputruntime

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/osfs/internal/checkpointstore"
	"github.com/windshare/windshare/osfs/internal/resumeauthority"
)

func TestNativeInterruptedCheckpointCleanupCompletesAndDiscards(t *testing.T) {
	for _, action := range []string{"complete", "discard-partial", "discard-published"} {
		for _, shape := range []string{"empty", "truncated", "zero-padded", "complete"} {
			// A complete successor becomes obsolete only after lazy resume advances
			// past it. Immediate discard may instead adopt that valid successor.
			if action == "discard-partial" && shape == "complete" {
				continue
			}
			t.Run(action+"/"+shape, func(t *testing.T) {
				ctx := context.Background()
				root := newRuntimeTestRootSpec(t).path
				fixture := openOrdinaryResumeSession(t, root, 0x68, 4)
				if err := fixture.transaction.WriteRange(ctx, 0, []byte("ab")); err != nil {
					t.Fatal(err)
				}
				if _, err := fixture.transaction.Checkpoint(ctx); err != nil {
					t.Fatal(err)
				}
				pauseOrdinaryResumeFixture(t, fixture)
				candidate := interruptNativeCheckpointImage(t, root, shape)

				var events []FilesystemOutputTrace
				tracer := FilesystemOutputTraceFunc(func(event FilesystemOutputTrace) {
					events = append(events, event)
				})
				if action != "discard-partial" {
					restarted := openOrdinaryResumeFileWithTracer(t, root, 0x68, 4, true, tracer)
					t.Cleanup(func() { _ = restarted.authority.Close() })
					if restarted.intent.OperationID() != fixture.intent.OperationID() ||
						restarted.durable.Ranges().Len() != 1 || restarted.durable.Ranges().Ranges()[0].End != 2 {
						t.Fatal("interrupted successor changed the verified resume cut")
					}
					if err := restarted.transaction.WriteRange(ctx, 2, []byte("cd")); err != nil {
						t.Fatal(err)
					}
					if _, err := restarted.transaction.Checkpoint(ctx); err != nil {
						t.Fatal(err)
					}
					if settled, err := restarted.transaction.Commit(ctx); err != nil || settled.Kind() != transfer.FilePublished {
						t.Fatalf("resumed publication = %v, %v", settled.Kind(), err)
					}
					if action == "complete" {
						if settled, err := restarted.session.FinalizeTree(ctx, transfer.DirectTreeOutcomeSuccess); err != nil ||
							settled.Kind() != transfer.DirectTreeSettlementSuccess {
							t.Fatalf("completed tree = %v, %v", settled.Kind(), err)
						}
					} else if _, err := restarted.session.PauseTree(ctx, transfer.JobPauseInterrupted); err != nil {
						t.Fatal(err)
					}
					if err := restarted.authority.Close(); err != nil {
						t.Fatal(err)
					}
				}

				repository, err := NewNativeResumeRepository(root, openOutputRuntimeTestPlatform, tracer)
				if err != nil {
					t.Fatal(err)
				}
				if action != "complete" {
					recovery, err := resumeauthority.New(repository)
					if err != nil {
						t.Fatal(err)
					}
					result, err := recovery.Discard(ctx, fixture.intent.OperationID())
					if err != nil || result.State() != resumeauthority.OperationDiscarded {
						t.Fatalf("discard did not finish: %v, %v", result.State(), err)
					}
				}
				page, err := repository.Page(ctx, resumeauthority.PageCursor{}, 8)
				if err != nil || len(page.Headers()) != 0 {
					t.Fatalf("terminal operation remains in inventory: %v, %v", page.Headers(), err)
				}
				if _, err := os.Stat(candidate); !errors.Is(err, fs.ErrNotExist) {
					t.Fatalf("interrupted candidate remains: %v", err)
				}
				if _, err := os.Stat(filepath.Join(root, checkpointstore.ControlDirectory)); !errors.Is(err, fs.ErrNotExist) {
					t.Fatalf("terminal cleanup retained private state: %v", err)
				}
				if action == "discard-partial" {
					if _, err := os.Stat(fixture.finalPath); !errors.Is(err, fs.ErrNotExist) {
						t.Fatalf("discard published an unfinished file: %v", err)
					}
				} else if actual, err := os.ReadFile(fixture.finalPath); err != nil || string(actual) != "abcd" {
					t.Fatalf("cleanup changed the published result: %q, %v", actual, err)
				}
				observed := false
				for _, event := range events {
					if event.CheckpointReclaimedCandidateCount != 0 {
						if event.ReceiveOperationID != fixture.intent.OperationID() ||
							event.CheckpointReclaimedCandidateCount != 1 || event.Failed {
							t.Fatalf("candidate reclamation trace = %+v", event)
						}
						observed = true
					}
				}
				if !observed {
					t.Fatal("candidate reclamation had no operation-correlated diagnostic")
				}
			})
		}
	}
}

func interruptNativeCheckpointImage(t *testing.T, root, shape string) string {
	t.Helper()
	var candidate string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || !strings.HasSuffix(entry.Name(), ".checkpoint") {
			return err
		}
		encoded, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		previous, err := checkpointmodel.DecodeRecord(encoded)
		if err != nil {
			return err
		}
		next, err := checkpointmodel.AdvanceDurableRanges(previous, previous.VerifiedRanges())
		if err != nil {
			return err
		}
		encoded, err = checkpointmodel.EncodeRecord(next)
		if err != nil {
			return err
		}
		candidate = filepath.Join(filepath.Dir(path),
			checkpointstore.OperationTemporaryName(next.OperationID(), entry.Name(), encoded, 0))
		var interrupted []byte
		switch shape {
		case "complete":
			interrupted = encoded
		case "truncated":
			interrupted = encoded[:len(encoded)/2]
		case "zero-padded":
			interrupted = make([]byte, len(encoded))
			copy(interrupted, encoded[:len(encoded)/2])
		}
		if err := os.WriteFile(candidate, interrupted, 0o600); err != nil {
			return err
		}
		return fs.SkipAll
	})
	if err != nil || candidate == "" {
		t.Fatalf("interrupt checkpoint image: %v", err)
	}
	return candidate
}
