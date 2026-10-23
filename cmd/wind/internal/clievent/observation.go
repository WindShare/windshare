package clievent

import (
	appobs "github.com/windshare/windshare/engine/observation"
)

type CatalogStorageObserved struct {
	appobs.CatalogStorageObserved
}

func (value CatalogStorageObserved) event()           {}
func (value CatalogStorageObserved) Command() Command { return commandForWorkflow(value.Workflow()) }
func (value CatalogStorageObserved) Accept(visitor Visitor) error {
	if visitor == nil {
		return ErrInvalidEvent
	}
	if err := value.Validate(); err != nil {
		return err
	}
	return visitor.VisitCatalogStorageObserved(value)
}

type ContentPathSelected struct {
	appobs.ContentPathSelected
}

func (value ContentPathSelected) event()           {}
func (value ContentPathSelected) Command() Command { return commandForWorkflow(value.Workflow()) }
func (value ContentPathSelected) Accept(visitor Visitor) error {
	if visitor == nil {
		return ErrInvalidEvent
	}
	if err := value.Validate(); err != nil {
		return err
	}
	return visitor.VisitContentPathSelected(value)
}

type EngineTaskObserved struct{ appobs.EngineTaskObserved }

func (value EngineTaskObserved) event()           {}
func (value EngineTaskObserved) Command() Command { return commandForWorkflow(value.Workflow()) }
func (value EngineTaskObserved) Accept(visitor Visitor) error {
	if visitor == nil {
		return ErrInvalidEvent
	}
	if err := value.Validate(); err != nil {
		return err
	}
	return visitor.VisitEngineTaskObserved(value)
}

type Fallback struct{ appobs.Fallback }

func (value Fallback) event()           {}
func (value Fallback) Command() Command { return commandForWorkflow(value.Workflow()) }
func (value Fallback) Accept(visitor Visitor) error {
	if visitor == nil {
		return ErrInvalidEvent
	}
	if err := value.Validate(); err != nil {
		return err
	}
	return visitor.VisitFallback(value)
}

type FilesystemOutputObserved struct {
	appobs.FilesystemOutputObserved
}

func (value FilesystemOutputObserved) event()           {}
func (value FilesystemOutputObserved) Command() Command { return commandForWorkflow(value.Workflow()) }
func (value FilesystemOutputObserved) Accept(visitor Visitor) error {
	if visitor == nil {
		return ErrInvalidEvent
	}
	if err := value.Validate(); err != nil {
		return err
	}
	return visitor.VisitFilesystemOutputObserved(value)
}

type LaneAdopted struct{ appobs.LaneAdopted }

func (value LaneAdopted) event()           {}
func (value LaneAdopted) Command() Command { return commandForWorkflow(value.Workflow()) }
func (value LaneAdopted) Accept(visitor Visitor) error {
	if visitor == nil {
		return ErrInvalidEvent
	}
	if err := value.Validate(); err != nil {
		return err
	}
	return visitor.VisitLaneAdopted(value)
}

type LaneSettlementObserved struct {
	appobs.LaneSettlementObserved
}

func (value LaneSettlementObserved) event()           {}
func (value LaneSettlementObserved) Command() Command { return commandForWorkflow(value.Workflow()) }
func (value LaneSettlementObserved) Accept(visitor Visitor) error {
	if visitor == nil {
		return ErrInvalidEvent
	}
	if err := value.Validate(); err != nil {
		return err
	}
	return visitor.VisitLaneSettlementObserved(value)
}

type NativeConnectivityObserved struct {
	appobs.NativeConnectivityObserved
}

func (value NativeConnectivityObserved) event() {}
func (value NativeConnectivityObserved) Command() Command {
	return commandForWorkflow(value.Workflow())
}
func (value NativeConnectivityObserved) Accept(visitor Visitor) error {
	if visitor == nil {
		return ErrInvalidEvent
	}
	if err := value.Validate(); err != nil {
		return err
	}
	return visitor.VisitNativeConnectivityObserved(value)
}

type ObserverLossObserved struct {
	appobs.ObserverLossObserved
}

func (value ObserverLossObserved) event()           {}
func (value ObserverLossObserved) Command() Command { return commandForWorkflow(value.Workflow()) }
func (value ObserverLossObserved) Accept(visitor Visitor) error {
	if visitor == nil {
		return ErrInvalidEvent
	}
	if err := value.Validate(); err != nil {
		return err
	}
	return visitor.VisitObserverLossObserved(value)
}

type PeerAttemptObserved struct {
	appobs.PeerAttemptObserved
}

func (value PeerAttemptObserved) event()           {}
func (value PeerAttemptObserved) Command() Command { return commandForWorkflow(value.Workflow()) }
func (value PeerAttemptObserved) Accept(visitor Visitor) error {
	if visitor == nil {
		return ErrInvalidEvent
	}
	if err := value.Validate(); err != nil {
		return err
	}
	return visitor.VisitPeerAttemptObserved(value)
}

type ProtocolObservationObserved struct {
	appobs.ProtocolObservationObserved
}

func (value ProtocolObservationObserved) event() {}
func (value ProtocolObservationObserved) Command() Command {
	return commandForWorkflow(value.Workflow())
}
func (value ProtocolObservationObserved) Accept(visitor Visitor) error {
	if visitor == nil {
		return ErrInvalidEvent
	}
	if err := value.Validate(); err != nil {
		return err
	}
	return visitor.VisitProtocolObservationObserved(value)
}

type Ready struct{ appobs.Ready }

func (value Ready) event()           {}
func (value Ready) Command() Command { return commandForWorkflow(value.Workflow()) }
func (value Ready) Accept(visitor Visitor) error {
	if visitor == nil {
		return ErrInvalidEvent
	}
	if err := value.Validate(); err != nil {
		return err
	}
	return visitor.VisitReady(value)
}

type ReceiverTerminationObserved struct {
	appobs.ReceiverTerminationObserved
}

func (value ReceiverTerminationObserved) event() {}
func (value ReceiverTerminationObserved) Command() Command {
	return commandForWorkflow(value.Workflow())
}
func (value ReceiverTerminationObserved) Accept(visitor Visitor) error {
	if visitor == nil {
		return ErrInvalidEvent
	}
	if err := value.Validate(); err != nil {
		return err
	}
	return visitor.VisitReceiverTerminationObserved(value)
}

type RelayAvailability struct{ appobs.RelayAvailability }

func (value RelayAvailability) event()           {}
func (value RelayAvailability) Command() Command { return commandForWorkflow(value.Workflow()) }
func (value RelayAvailability) Accept(visitor Visitor) error {
	if visitor == nil {
		return ErrInvalidEvent
	}
	if err := value.Validate(); err != nil {
		return err
	}
	return visitor.VisitRelayAvailability(value)
}

type RelayConnected struct{ appobs.RelayConnected }

func (value RelayConnected) event()           {}
func (value RelayConnected) Command() Command { return commandForWorkflow(value.Workflow()) }
func (value RelayConnected) Accept(visitor Visitor) error {
	if visitor == nil {
		return ErrInvalidEvent
	}
	if err := value.Validate(); err != nil {
		return err
	}
	return visitor.VisitRelayConnected(value)
}

type RelayLifecycleObserved struct {
	appobs.RelayLifecycleObserved
}

func (value RelayLifecycleObserved) event()           {}
func (value RelayLifecycleObserved) Command() Command { return commandForWorkflow(value.Workflow()) }
func (value RelayLifecycleObserved) Accept(visitor Visitor) error {
	if visitor == nil {
		return ErrInvalidEvent
	}
	if err := value.Validate(); err != nil {
		return err
	}
	return visitor.VisitRelayLifecycleObserved(value)
}

type RelayRecovering struct{ appobs.RelayRecovering }

func (value RelayRecovering) event()           {}
func (value RelayRecovering) Command() Command { return commandForWorkflow(value.Workflow()) }
func (value RelayRecovering) Accept(visitor Visitor) error {
	if visitor == nil {
		return ErrInvalidEvent
	}
	if err := value.Validate(); err != nil {
		return err
	}
	return visitor.VisitRelayRecovering(value)
}

type RootPrefetchObserved struct {
	appobs.RootPrefetchObserved
}

func (value RootPrefetchObserved) event()           {}
func (value RootPrefetchObserved) Command() Command { return commandForWorkflow(value.Workflow()) }
func (value RootPrefetchObserved) Accept(visitor Visitor) error {
	if visitor == nil {
		return ErrInvalidEvent
	}
	if err := value.Validate(); err != nil {
		return err
	}
	return visitor.VisitRootPrefetchObserved(value)
}

type SenderCapacityObserved struct {
	appobs.SenderCapacityObserved
}

func (value SenderCapacityObserved) event()           {}
func (value SenderCapacityObserved) Command() Command { return commandForWorkflow(value.Workflow()) }
func (value SenderCapacityObserved) Accept(visitor Visitor) error {
	if visitor == nil {
		return ErrInvalidEvent
	}
	if err := value.Validate(); err != nil {
		return err
	}
	return visitor.VisitSenderCapacityObserved(value)
}

type SenderRevisionObserved struct {
	appobs.SenderRevisionObserved
}

func (value SenderRevisionObserved) event()           {}
func (value SenderRevisionObserved) Command() Command { return commandForWorkflow(value.Workflow()) }
func (value SenderRevisionObserved) Accept(visitor Visitor) error {
	if visitor == nil {
		return ErrInvalidEvent
	}
	if err := value.Validate(); err != nil {
		return err
	}
	return visitor.VisitSenderRevisionObserved(value)
}

type SenderSessionTerminated struct {
	appobs.SenderSessionTerminated
}

func (value SenderSessionTerminated) event()           {}
func (value SenderSessionTerminated) Command() Command { return commandForWorkflow(value.Workflow()) }
func (value SenderSessionTerminated) Accept(visitor Visitor) error {
	if visitor == nil {
		return ErrInvalidEvent
	}
	if err := value.Validate(); err != nil {
		return err
	}
	return visitor.VisitSenderSessionTerminated(value)
}

type SenderTerminalSendObserved struct {
	appobs.SenderTerminalSendObserved
}

func (value SenderTerminalSendObserved) event() {}
func (value SenderTerminalSendObserved) Command() Command {
	return commandForWorkflow(value.Workflow())
}
func (value SenderTerminalSendObserved) Accept(visitor Visitor) error {
	if visitor == nil {
		return ErrInvalidEvent
	}
	if err := value.Validate(); err != nil {
		return err
	}
	return visitor.VisitSenderTerminalSendObserved(value)
}

type SharingSubjectSelected struct {
	appobs.SharingSubjectSelected
}

func (value SharingSubjectSelected) event()           {}
func (value SharingSubjectSelected) Command() Command { return commandForWorkflow(value.Workflow()) }
func (value SharingSubjectSelected) Accept(visitor Visitor) error {
	if visitor == nil {
		return ErrInvalidEvent
	}
	if err := value.Validate(); err != nil {
		return err
	}
	return visitor.VisitSharingSubjectSelected(value)
}

type TransferLifecycleObserved struct {
	appobs.TransferLifecycleObserved
}

func (value TransferLifecycleObserved) event()           {}
func (value TransferLifecycleObserved) Command() Command { return commandForWorkflow(value.Workflow()) }
func (value TransferLifecycleObserved) Accept(visitor Visitor) error {
	if visitor == nil {
		return ErrInvalidEvent
	}
	if err := value.Validate(); err != nil {
		return err
	}
	return visitor.VisitTransferLifecycleObserved(value)
}

type TransferProgress struct{ appobs.TransferProgress }

func (value TransferProgress) event()           {}
func (value TransferProgress) Command() Command { return commandForWorkflow(value.Workflow()) }
func (value TransferProgress) Accept(visitor Visitor) error {
	if visitor == nil {
		return ErrInvalidEvent
	}
	if err := value.Validate(); err != nil {
		return err
	}
	return visitor.VisitTransferProgress(value)
}

type Warning struct{ appobs.Warning }

func (value Warning) event()           {}
func (value Warning) Command() Command { return commandForWorkflow(value.Workflow()) }
func (value Warning) Accept(visitor Visitor) error {
	if visitor == nil {
		return ErrInvalidEvent
	}
	if err := value.Validate(); err != nil {
		return err
	}
	return visitor.VisitWarning(value)
}

type WebRTCLifecycleObserved struct {
	appobs.WebRTCLifecycleObserved
}

func (value WebRTCLifecycleObserved) event()           {}
func (value WebRTCLifecycleObserved) Command() Command { return commandForWorkflow(value.Workflow()) }
func (value WebRTCLifecycleObserved) Accept(visitor Visitor) error {
	if visitor == nil {
		return ErrInvalidEvent
	}
	if err := value.Validate(); err != nil {
		return err
	}
	return visitor.VisitWebRTCLifecycleObserved(value)
}
