// Package commandprojection adapts engine facts to command-specific results.
package commandprojection

import (
	"errors"

	"github.com/windshare/windshare/adapters/filesystem"
	"github.com/windshare/windshare/cmd/wind/internal/clievent"
	"github.com/windshare/windshare/core/liveshare"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/receivecontract"
	"github.com/windshare/windshare/engine"
	appobs "github.com/windshare/windshare/engine/observation"
	"github.com/windshare/windshare/osfs"
)

var ErrInvalidProjection = errors.New("command presentation is invalid")

func ClassifyError(cause error) (appobs.Failure, bool) { return engine.DescribeFailure(cause) }
func ObserverLossReason(cause error) appobs.ObserverLossReason {
	return engine.ObservationLossReason(cause)
}
func ProjectionRejection(cause error) appobs.ObservationRejection {
	return engine.ObservationRejection(cause)
}

func mustFailure(code appobs.FailureCode) appobs.Failure {
	value, _ := appobs.NewFailure(code)
	return value
}
func projectFileOutcomes(value transfer.FileOutcomeSummary) appobs.FileOutcomes {
	return engine.DescribeFileOutcomes(value)
}
func NormalizeRelayAuthority(raw string) (appobs.RelayAuthority, error) {
	return engine.NormalizeRelayAuthority(raw)
}
func SealFilesystemOutputFailure(value osfs.FilesystemOutputDiagnostic) (error, bool) {
	return filesystem.SealFilesystemOutputFailure(value)
}
func ProjectProgress(value transfer.ReceiveProgressSnapshot, visible bool) (appobs.ProgressSnapshot, error) {
	return engine.DescribeProgressSnapshot(value, visible)
}
func ProjectTransferProgress(operation receivecontract.OperationID, job transfer.TransferJobID, value transfer.ReceiveProgressSnapshot, visible bool) (clievent.TransferProgress, error) {
	fact, err := engine.DescribeProgress(operation, job, value, visible)
	return clievent.TransferProgress{TransferProgress: fact}, err
}
func ProjectSharingSubject(value liveshare.SelectedRootSummary) (clievent.SharingSubjectSelected, error) {
	fact, err := engine.DescribeSharingSubject(value)
	return clievent.SharingSubjectSelected{SharingSubjectSelected: fact}, err
}
