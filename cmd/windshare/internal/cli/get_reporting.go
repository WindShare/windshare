package cli

import (
	"fmt"
	"io"
	"math/bits"
	"strings"
	"sync"

	"github.com/windshare/windshare/core/osfs"
	"github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/ordinaryoutput"
	"golang.org/x/term"
)

const liveOnlyOutputWarning = "This destination supports safe saving, but this download cannot be resumed.\n" +
	"If interrupted, completed files stay; the next get uses a new name and downloads the selection again."

type TTYDetector interface {
	IsTTY(io.Writer) bool
}

type TTYDetectorFunc func(io.Writer) bool

func (function TTYDetectorFunc) IsTTY(writer io.Writer) bool {
	return function != nil && function(writer)
}

type nativeTTYDetector struct{}

func (nativeTTYDetector) IsTTY(writer io.Writer) bool {
	file, ok := writer.(interface{ Fd() uintptr })
	return ok && term.IsTerminal(int(file.Fd()))
}

type ProgressSink interface {
	Update(transfer.SelectionMeasure)
	Finish()
}

type silentProgressSink struct{}

func (silentProgressSink) Update(transfer.SelectionMeasure) {}
func (silentProgressSink) Finish()                          {}

type terminalProgressSink struct {
	writer io.Writer
	width  int
}

func (sink *terminalProgressSink) Update(measure transfer.SelectionMeasure) {
	if sink == nil || sink.writer == nil {
		return
	}
	line := formatGetProgress(measure)
	padding := ""
	if sink.width > len(line) {
		padding = strings.Repeat(" ", sink.width-len(line))
	}
	_, _ = fmt.Fprintf(sink.writer, "\r%s%s", line, padding)
	sink.width = len(line)
}

func (sink *terminalProgressSink) Finish() {
	if sink == nil || sink.writer == nil || sink.width == 0 {
		return
	}
	_, _ = fmt.Fprintf(sink.writer, "\r%s\r", strings.Repeat(" ", sink.width))
	sink.width = 0
}

func formatGetProgress(measure transfer.SelectionMeasure) string {
	if measure.Discovery != transfer.DiscoveryComplete {
		return fmt.Sprintf(
			"get: discovering files=%d bytes=%d completed_files=%d completed_bytes=%d",
			measure.DiscoveredFiles, measure.DiscoveredBytes,
			measure.CompletedFiles, measure.CompletedBytes,
		)
	}
	percent := uint64(100)
	if measure.DiscoveredBytes > 0 {
		completed := min(measure.CompletedBytes, measure.DiscoveredBytes)
		high, low := bits.Mul64(completed, 100)
		percent, _ = bits.Div64(high, low, measure.DiscoveredBytes)
	}
	return fmt.Sprintf(
		"get: files=%d/%d bytes=%d/%d %d%%",
		measure.CompletedFiles, measure.DiscoveredFiles,
		measure.CompletedBytes, measure.DiscoveredBytes, percent,
	)
}

type WarningSink interface {
	Warn(string)
}

type WarningSinkFunc func(string)

func (function WarningSinkFunc) Warn(message string) {
	if function != nil {
		function(message)
	}
}

type warningOnce struct {
	sink WarningSink
	once sync.Once
}

func (warning *warningOnce) Warn(message string) {
	if warning == nil || warning.sink == nil {
		return
	}
	warning.once.Do(func() { warning.sink.Warn(message) })
}

type GetTraceStage uint8

const (
	GetTraceDestinationBound GetTraceStage = iota + 1
	GetTraceActiveLookup
	GetTraceShapeProbe
	GetTraceOperationReady
	GetTraceContentAdmitted
	GetTraceFilesystemOutput
	GetTraceTransferLifecycle
)

type GetTraceEvent struct {
	Stage             GetTraceStage
	ProtocolSessionID protocolsession.ProtocolSessionID
	SelectionDigest   transfer.SelectionSpecDigest
	IntentDigest      transfer.ReceiveIntentDigest
	Mode              getOutputMode
	Lookup            getOutputLookupKind
	ShapeKind         ordinaryoutput.ShapeKind
	ShapeFallback     ordinaryoutput.ShapeFallbackReason
	DirectoryRequests uint32
	Pages             uint32
	Entries           uint32
	MetadataBytes     uint64
	Renamed           bool
	Failed            bool
	FilesystemOutput  osfs.FilesystemOutputTrace
	TransferLifecycle transfer.TransferLifecycleTrace
}

type GetTraceSink interface {
	TraceGet(GetTraceEvent)
}

type GetTraceSinkFunc func(GetTraceEvent)

func (function GetTraceSinkFunc) TraceGet(event GetTraceEvent) {
	if function != nil {
		function(event)
	}
}

func (a *App) getProgressReporter() ProgressSink {
	detector := a.getTTYDetector
	if detector == nil {
		detector = nativeTTYDetector{}
	}
	if !detector.IsTTY(a.Stderr) {
		return silentProgressSink{}
	}
	if a.getProgress != nil {
		return a.getProgress
	}
	return &terminalProgressSink{writer: a.stderrWriter()}
}

func (a *App) getWarningReporter() *warningOnce {
	sink := a.getWarnings
	if sink == nil {
		sink = WarningSinkFunc(func(message string) { a.logf("get: warning: %s", message) })
	}
	return &warningOnce{sink: sink}
}

func (a *App) traceGet(event GetTraceEvent) {
	if a == nil || a.getTraces == nil {
		return
	}
	a.getTraces.TraceGet(event)
}

func (a *App) traceOrdinaryOutputShape(event ordinaryoutput.ShapeTrace) {
	a.traceGet(GetTraceEvent{
		Stage: GetTraceShapeProbe, ProtocolSessionID: event.ProtocolSessionID,
		SelectionDigest: transfer.SelectionSpecDigest(event.SelectionDigest),
		ShapeKind:       event.Kind, ShapeFallback: event.Fallback,
		DirectoryRequests: event.DirectoryRequests, Pages: event.AuthenticatedPages,
		Entries: event.AuthenticatedEntries, MetadataBytes: event.AuthenticatedMetadataBytes,
	})
}

type getTransferSummary struct {
	status             DirectGetStatus
	files              transfer.FileOutcomeSummary
	directoryFailures  uint64
	omittedDiagnostics uint64
	renamed            bool
	bytes              uint64
}

type DirectGetStatus uint8

const (
	DirectGetSuccess DirectGetStatus = iota + 1
	DirectGetPartial
	DirectGetPaused
	DirectGetFailed
)

func summarizeGetTransfer(result transfer.JobResult, renamed bool) getTransferSummary {
	// Per-item diagnostics are intentionally bounded. Exact file counts therefore
	// come only from the settlement-owned aggregate, never from retained paths.
	summary := getTransferSummary{
		status: DirectGetFailed, files: result.FileOutcomes, renamed: renamed, bytes: result.Measure.CompletedBytes,
		directoryFailures:  uint64(len(result.Directories)) + result.OmittedDirectoryFailures,
		omittedDiagnostics: result.OmittedFileFailures + result.OmittedDirectoryFailures,
	}
	switch result.Outcome {
	case transfer.DirectTreeOutcomeSuccess:
		if successfulGetResult(result) {
			summary.status = DirectGetSuccess
		} else {
			summary.status = DirectGetFailed
		}
	case transfer.DirectTreeOutcomePartial:
		summary.status = DirectGetPartial
	case transfer.DirectTreeOutcomePaused:
		summary.status = DirectGetPaused
	case transfer.DirectTreeOutcomeFailed:
		summary.status = DirectGetFailed
	}
	return summary
}

func successfulGetResult(result transfer.JobResult) bool {
	files := result.FileOutcomes
	return result.TerminationCause == nil && !result.TerminationFault.Valid() &&
		result.SettlementFailure == nil && !result.SettlementFault.Valid() &&
		result.SelectionResolutionFailure == nil && result.SourceDriftFailure == nil &&
		!result.SourceDriftFault.Valid() && len(result.Directories) == 0 && len(result.Files) == 0 &&
		result.OmittedDirectoryFailures == 0 && result.OmittedFileFailures == 0 &&
		files.PausedFiles == 0 && files.CollisionFiles == 0 && files.FailedFiles == 0 &&
		files.ItemBlockedFiles == 0 && result.Settlement.Kind() == transfer.DirectTreeSettlementSuccess &&
		result.Measure.Discovery == transfer.DiscoveryComplete && result.Measure.DiscoveryTerminalSuccess &&
		!successfulTransferIncomplete(result)
}

func (a *App) logGetTransferSummary(result transfer.JobResult, renamed bool) {
	summary := summarizeGetTransfer(result, renamed)
	a.logf(
		"get: result=%s downloaded=%d resumed=%d paused=%d collision=%d failed=%d item-blocked=%d directories-failed=%d renamed=%t metadata-warnings=%d omitted-diagnostics=%d bytes=%d",
		getStatusName(summary.status),
		summary.files.DownloadedFiles, summary.files.ResumedFiles, summary.files.PausedFiles,
		summary.files.CollisionFiles,
		summary.files.FailedFiles,
		summary.files.ItemBlockedFiles, summary.directoryFailures, summary.renamed,
		summary.files.ModifiedTimeWarnings, summary.omittedDiagnostics, summary.bytes,
	)
}

func getStatusName(status DirectGetStatus) string {
	switch status {
	case DirectGetSuccess:
		return "success"
	case DirectGetPartial:
		return "partial"
	case DirectGetPaused:
		return "paused"
	case DirectGetFailed:
		return "failed"
	default:
		return "invalid"
	}
}
