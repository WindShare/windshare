package outputsession

import (
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/fault"
	"github.com/windshare/windshare/core/transfer/receivecontract"
)

type OperationKind uint8

const (
	OperationAdmitDirectory OperationKind = iota + 1
	OperationFinalizeDirectory
	OperationBeginFile
	OperationWriteRange
	OperationCheckpointFile
	OperationCommitFile
	OperationPauseFile
	OperationRetireFile
	OperationPauseTree
	OperationFinalizeTree
	OperationFirstWrite
)

type TraceDecision uint8

const (
	TraceReserved TraceDecision = iota + 1
	TraceCoalesced
	TraceRejected
	TraceRolledBack
	TraceAdmitted
	TraceActive
	TraceSealed
	TraceSettled
	TraceAmbiguous
	TraceDraining
	TraceClosed
	TraceCollision
)

type ClaimKind uint8

const (
	ClaimDirectory ClaimKind = iota + 1
	ClaimFile
)

type ClaimState uint8

const (
	ClaimPending ClaimState = iota + 1
	ClaimAdmitted
	ClaimActive
	ClaimSettling
	ClaimSettled
)

type TraceEvent struct {
	ReceiveIntentDigest    transfer.ReceiveIntentDigest
	ReceiveOperationID     receivecontract.OperationID
	SessionID              transfer.OutputSessionID
	OperationID            uint64
	Operation              OperationKind
	Decision               TraceDecision
	ClaimID                ClaimID
	ClaimKind              ClaimKind
	From                   ClaimState
	To                     ClaimState
	Fault                  fault.Fault
	NodeClaims             uint64
	DirectoryClaims        uint64
	FileClaims             uint64
	ActiveFileClaims       uint64
	ReservedFileSlots      uint64
	DirectoryMetadataBytes uint64
}

type TraceSink interface {
	RecordOutputSessionTrace(TraceEvent)
}

type TraceSinkFunc func(TraceEvent)

func (function TraceSinkFunc) RecordOutputSessionTrace(event TraceEvent) {
	if function != nil {
		function(event)
	}
}
