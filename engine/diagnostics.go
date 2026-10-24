package engine

import (
	"github.com/windshare/windshare/core/content/revisioncapacity"
	"github.com/windshare/windshare/core/liveshare"
	"github.com/windshare/windshare/core/session/sessionruntime"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/receivecontract"
	"github.com/windshare/windshare/engine/internal/eventproject"
	"github.com/windshare/windshare/engine/internal/task"
	appobs "github.com/windshare/windshare/engine/observation"
)

// DescribeFailure also serves adapter-owned failures, such as opening a local
// output or publishing a link. Task results already carry this normalized fact.
func DescribeFailure(cause error) (appobs.Failure, bool) { return eventproject.ClassifyError(cause) }

func NormalizeRelayAuthority(raw string) (appobs.RelayAuthority, error) {
	return eventproject.NormalizeRelayAuthority(raw)
}

func DescribeProgress(operation receivecontract.OperationID, job transfer.TransferJobID, snapshot transfer.ReceiveProgressSnapshot, capacityVisible bool) (appobs.TransferProgress, error) {
	return eventproject.ProjectTransferProgress(operation, job, snapshot, capacityVisible)
}
func DescribeSharingSubject(summary liveshare.SelectedRootSummary) (appobs.SharingSubjectSelected, error) {
	return eventproject.ProjectSharingSubject(summary)
}
func DescribeCapacity(value revisioncapacity.TraceEvent) (appobs.SenderCapacityObserved, error) {
	return eventproject.ProjectSenderCapacity(value)
}

func ReceiveLossSnapshot(value ReceiveObservationLoss) appobs.LossSnapshot {
	return eventproject.ReceiveLoss(value)
}
func ShareLossSnapshot(value ShareObservationLoss) appobs.LossSnapshot {
	return eventproject.ShareLoss(value)
}

func ObservationLossReason(cause error) appobs.ObserverLossReason {
	return eventproject.ObserverLossReason(cause)
}
func ObservationRejection(cause error) appobs.ObservationRejection {
	return eventproject.ProjectionRejection(cause)
}
func DescribeProgressSnapshot(value transfer.ReceiveProgressSnapshot, visible bool) (appobs.ProgressSnapshot, error) {
	return eventproject.ProjectProgress(value, visible)
}
func DescribeFileOutcomes(value transfer.FileOutcomeSummary) appobs.FileOutcomes {
	return eventproject.FileOutcomes(value)
}

func describeSettlement(value task.Settlement) task.Settlement {
	value = value.Checked()
	value.Failure, _ = eventproject.ClassifyError(value.Err)
	value.CleanupFailure, _ = eventproject.ClassifyError(value.CleanupError)
	return value
}

func DescribeProtocolObservation(workflow appobs.Workflow, value sessionruntime.ProtocolObservation) (appobs.ProtocolObservationObserved, error) {
	return eventproject.ProjectProtocolObservation(workflow, value)
}
func DescribeSenderTermination(value sessionruntime.SenderSessionTerminated) (appobs.SenderSessionTerminated, error) {
	return eventproject.ProjectSenderSessionTerminated(value)
}
