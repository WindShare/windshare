package checkpointstore

import (
	"bytes"
	"errors"
	"fmt"
	"slices"
	"testing"

	"github.com/windshare/windshare/core/transfer/receivecontract"
	"github.com/windshare/windshare/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/osfs/internal/destinationauthority"
	"github.com/windshare/windshare/osfs/internal/outputcap"
)

func TestOperationPagesIsolateInvalidRecords(t *testing.T) {
	for _, failure := range []string{"decode", "binding", "intent"} {
		t.Run(failure, func(t *testing.T) {
			control := newMemoryDirectory()
			registry, err := OpenOperationRegistry(control)
			if err != nil {
				t.Fatal(err)
			}
			defer registry.Close()
			first := operationPageRecord(t, 0x61)
			broken := operationPageRecord(t, 0x62)
			last := operationPageRecord(t, 0x64)
			installOperationPageRecord(t, &registry, first.OperationID(), encodeOperationPageRecord(t, first))
			installOperationPageRecord(t, &registry, last.OperationID(), encodeOperationPageRecord(t, last))

			admission, _, err := registry.BeginActive(broken.ActiveOperationKey())
			if err != nil {
				t.Fatal(err)
			}
			defer admission.Close()
			if err := admission.PrepareCandidate(broken.OperationID()); err != nil {
				t.Fatal(err)
			}
			encoded := []byte("invalid operation record")
			if failure != "decode" {
				foreign := operationPageRecord(t, 0x63)
				encoded = encodeOperationPageRecord(t, foreign)
				if failure == "intent" {
					// Keep the record encoding and digest valid while its stored
					// intent identifies a different operation.
					encoded = bytes.Replace(encoded, foreign.OperationID().Bytes(), broken.OperationID().Bytes(), 1)
				}
				if _, err := checkpointmodel.DecodeOrdinaryOperationRecord(encoded); err != nil {
					t.Fatalf("fixture must reach binding or intent verification: %v", err)
				}
			}
			directory := installOperationPageRecord(t, &registry, broken.OperationID(), encoded)

			for name, readPage := range map[string]func(OperationPageCursor, int) (OperationPage, error){
				"registry": registry.PageOperations,
				"read only": func(cursor OperationPageCursor, maximum int) (OperationPage, error) {
					return ReadOperationPage(control, cursor, maximum)
				},
			} {
				for _, maximum := range []int{1, MaximumOrdinaryOperationPageSizeV1} {
					t.Run(fmt.Sprintf("%s/page-size-%d", name, maximum), func(t *testing.T) {
						var cursor OperationPageCursor
						var operations []receivecontract.OperationID
						var unknown bool
						for pageNumber := 0; ; pageNumber++ {
							if pageNumber >= 3 {
								t.Fatal("pagination did not advance past the invalid record")
							}
							page, err := readPage(cursor, maximum)
							if err != nil {
								t.Fatal(err)
							}
							unknown = unknown || page.Unknown()
							for _, record := range page.Records() {
								operations = append(operations, record.OperationID())
							}
							if page.Next().IsZero() {
								break
							}
							cursor = page.Next()
						}
						if !unknown || !slices.Equal(operations, []receivecontract.OperationID{first.OperationID(), last.OperationID()}) {
							t.Fatalf("operations=%v unknown=%t; want both healthy siblings and unknown state", operations, unknown)
						}
					})
				}
			}
			t.Run("admission candidate", func(t *testing.T) {
				unknown, err := registry.unknownAdmissionCandidates(registry.root)
				if err != nil || !unknown {
					t.Fatalf("invalid admission operation = unknown %t, error %v", unknown, err)
				}
			})
			actual, err := ReadFile(directory, ordinaryOperationRecordFile)
			if err != nil || !bytes.Equal(actual, encoded) {
				t.Fatalf("inventory changed the invalid record: %v", err)
			}
		})
	}
}

func TestOperationPagesPreserveStorageFailures(t *testing.T) {
	for _, failure := range []string{"open", "read", "close"} {
		t.Run(failure, func(t *testing.T) {
			registry, err := OpenOperationRegistry(newMemoryDirectory())
			if err != nil {
				t.Fatal(err)
			}
			defer registry.Close()
			record := operationPageRecord(t, 0x61)
			installOperationPageRecord(t, &registry, record.OperationID(), encodeOperationPageRecord(t, record))
			admission, _, err := registry.BeginActive(record.ActiveOperationKey())
			if err != nil {
				t.Fatal(err)
			}
			defer admission.Close()
			if err := admission.PrepareCandidate(record.OperationID()); err != nil {
				t.Fatal(err)
			}
			ioFailure := errors.New("injected storage failure")
			operations := registry.operations
			registry.operations = &operationRegistryFaultDirectory{
				Directory: operations,
				openDirectory: func(name string, private bool) (outputcap.Directory, error) {
					if failure == "open" {
						return nil, ioFailure
					}
					directory, err := operations.OpenDirectory(name, private)
					if err != nil {
						return nil, err
					}
					if failure == "read" {
						return &faultDirectory{
							Directory: directory,
							openObserved: func(string, bool) (outputcap.ObservedFile, error) {
								return nil, ioFailure
							},
						}, nil
					}
					return &operationRegistryFaultDirectory{
						Directory: directory, close: func() error { return ioFailure },
					}, nil
				},
			}
			if _, err := registry.PageOperations(OperationPageCursor{}, 1); !errors.Is(err, ioFailure) {
				t.Fatalf("paging hid a storage failure: %v", err)
			}
			if unknown, err := registry.unknownAdmissionCandidates(registry.root); !errors.Is(err, ioFailure) || unknown {
				t.Fatalf("candidate hid a storage failure: unknown=%t error=%v", unknown, err)
			}
		})
	}
}

func operationPageRecord(t *testing.T, seed byte) checkpointmodel.OrdinaryOperationRecord {
	t.Helper()
	return ordinaryRegistryFixture(t, seed).record(t, checkpointmodel.OrdinaryLeaseReleased,
		destinationauthority.ReservationClaim{
			Token: destinationauthority.ReservationClaimToken{seed}, Generation: 1,
		})
}

func encodeOperationPageRecord(t *testing.T, record checkpointmodel.OrdinaryOperationRecord) []byte {
	t.Helper()
	encoded, err := checkpointmodel.EncodeOrdinaryOperationRecord(record)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func installOperationPageRecord(
	t *testing.T,
	registry *OperationRegistry,
	operation receivecontract.OperationID,
	encoded []byte,
) outputcap.Directory {
	t.Helper()
	directory, err := openOrCreateDirectory(registry.operations, operationNamespaceName(operation))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := directory.Close(); err != nil {
			t.Error(err)
		}
	})
	if err := InstallCreate(directory, ordinaryOperationRecordFile, encoded); err != nil {
		t.Fatal(err)
	}
	return directory
}
