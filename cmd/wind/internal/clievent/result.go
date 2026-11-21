package clievent

import (
	"errors"
	"time"

	appobs "github.com/windshare/windshare/engine/observation"
)

var ErrInvalidResult = errors.New("CLI command result is invalid")

type DriftReason uint8

const (
	DriftNone DriftReason = iota
	DriftSource
)

func (value DriftReason) Name() (string, bool) {
	switch value {
	case DriftNone:
		return "none", true
	case DriftSource:
		return "source", true
	default:
		return "", false
	}
}

type TransferResultSpec struct {
	SourceOmittedEntries uint64
	Status               ResultStatus
	ExitCode             ExitCode
	Drift                DriftReason
	Elapsed              time.Duration
	Destination          appobs.DisplayPath
	DestinationAdjusted  bool
	RepairedNames        uint64
	NameReport           appobs.DisplayName
	Files                appobs.FileOutcomes
	DirectoryFailures    uint64
	OmittedDiagnostics   uint64
	PublishedBytes       uint64
	CountersExact        bool
	Failure              appobs.Failure
}

type TransferResult struct {
	sourceOmittedEntries uint64
	status               ResultStatus
	exitCode             ExitCode
	drift                DriftReason
	elapsed              time.Duration
	destination          appobs.DisplayPath
	destinationAdjusted  bool
	repairedNames        uint64
	nameReport           appobs.DisplayName
	files                appobs.FileOutcomes
	directoryFailures    uint64
	omittedDiagnostics   uint64
	publishedBytes       uint64
	countersExact        bool
	failure              appobs.Failure
	hasFailure           bool
}

func NewTransferResult(spec TransferResultSpec) (TransferResult, error) {
	if !spec.Status.Valid() || !spec.ExitCode.Valid() || spec.Elapsed < 0 || spec.Destination.Empty() ||
		(spec.RepairedNames == 0) != spec.NameReport.Empty() ||
		spec.CountersExact && !appobs.ValidExactItemBlockCounts(spec.Files) {
		return TransferResult{}, ErrInvalidResult
	}
	if _, ok := spec.Drift.Name(); !ok {
		return TransferResult{}, ErrInvalidResult
	}
	hasFailure := spec.Failure.Valid()
	switch {
	case spec.Status == ResultSuccess && (spec.ExitCode != ExitSuccess || spec.Drift != DriftNone || hasFailure || spec.SourceOmittedEntries != 0):
		return TransferResult{}, ErrInvalidResult
	case spec.Status != ResultSuccess && spec.ExitCode == ExitSuccess:
		return TransferResult{}, ErrInvalidResult
	case spec.Status != ResultSuccess && !hasFailure:
		return TransferResult{}, ErrInvalidResult
	case spec.Drift == DriftSource && spec.ExitCode != ExitDrift:
		return TransferResult{}, ErrInvalidResult
	case spec.ExitCode == ExitDrift && spec.Drift != DriftSource:
		return TransferResult{}, ErrInvalidResult
	}
	return TransferResult{
		sourceOmittedEntries: spec.SourceOmittedEntries,
		status:               spec.Status, exitCode: spec.ExitCode, drift: spec.Drift,
		elapsed: spec.Elapsed, destination: spec.Destination,
		destinationAdjusted: spec.DestinationAdjusted,
		repairedNames:       spec.RepairedNames, nameReport: spec.NameReport,
		files:             spec.Files,
		directoryFailures: spec.DirectoryFailures, omittedDiagnostics: spec.OmittedDiagnostics,
		publishedBytes: spec.PublishedBytes, countersExact: spec.CountersExact,
		failure: spec.Failure, hasFailure: hasFailure,
	}, nil
}

func (result TransferResult) Status() ResultStatus            { return result.status }
func (result TransferResult) ExitCode() ExitCode              { return result.exitCode }
func (result TransferResult) Drift() DriftReason              { return result.drift }
func (result TransferResult) Elapsed() time.Duration          { return result.elapsed }
func (result TransferResult) Destination() appobs.DisplayPath { return result.destination }
func (result TransferResult) DestinationAdjusted() bool       { return result.destinationAdjusted }
func (result TransferResult) RepairedNames() uint64           { return result.repairedNames }
func (result TransferResult) NameReport() appobs.DisplayName  { return result.nameReport }
func (result TransferResult) Files() appobs.FileOutcomes      { return result.files }
func (result TransferResult) SourceOmittedEntries() uint64    { return result.sourceOmittedEntries }
func (result TransferResult) DirectoryFailures() uint64       { return result.directoryFailures }
func (result TransferResult) OmittedDiagnostics() uint64      { return result.omittedDiagnostics }
func (result TransferResult) PublishedBytes() uint64          { return result.publishedBytes }
func (result TransferResult) CountersExact() bool             { return result.countersExact }
func (result TransferResult) Failure() (appobs.Failure, bool) {
	return result.failure, result.hasFailure
}
func (result TransferResult) Valid() bool {
	_, err := NewTransferResult(TransferResultSpec{
		SourceOmittedEntries: result.sourceOmittedEntries,
		Status:               result.status, ExitCode: result.exitCode, Drift: result.drift,
		Elapsed: result.elapsed, Destination: result.destination,
		DestinationAdjusted: result.destinationAdjusted,
		RepairedNames:       result.repairedNames, NameReport: result.nameReport,
		Files:             result.files,
		DirectoryFailures: result.directoryFailures, OmittedDiagnostics: result.omittedDiagnostics,
		PublishedBytes: result.publishedBytes, CountersExact: result.countersExact,
		Failure: func() appobs.Failure {
			if result.hasFailure {
				return result.failure
			}
			return appobs.Failure{}
		}(),
	})
	return err == nil
}

type SharePublication uint8

const (
	ShareUnpublished SharePublication = iota + 1
	SharePublished
)

func (publication SharePublication) Name() (string, bool) {
	switch publication {
	case ShareUnpublished:
		return "unpublished", true
	case SharePublished:
		return "published", true
	default:
		return "", false
	}
}

type ShareResultSpec struct {
	Publication SharePublication
	ExitCode    ExitCode
	Elapsed     time.Duration
	Failure     appobs.Failure
}

type ShareResult struct {
	publication SharePublication
	exitCode    ExitCode
	elapsed     time.Duration
	failure     appobs.Failure
	hasFailure  bool
}

func NewShareResult(spec ShareResultSpec) (ShareResult, error) {
	if _, valid := spec.Publication.Name(); !valid {
		return ShareResult{}, ErrInvalidResult
	}
	hasFailure := spec.Failure.Valid()
	if !spec.ExitCode.Valid() || spec.Elapsed < 0 ||
		(spec.ExitCode == ExitSuccess) != !hasFailure || spec.ExitCode == ExitUsage || spec.ExitCode == ExitDrift {
		return ShareResult{}, ErrInvalidResult
	}
	return ShareResult{
		publication: spec.Publication,
		exitCode:    spec.ExitCode, elapsed: spec.Elapsed,
		failure: spec.Failure, hasFailure: hasFailure,
	}, nil
}

func (result ShareResult) Publication() SharePublication { return result.publication }
func (result ShareResult) ExitCode() ExitCode            { return result.exitCode }
func (result ShareResult) Elapsed() time.Duration        { return result.elapsed }
func (result ShareResult) StoppedCleanly() bool          { return result.exitCode == ExitSuccess }
func (result ShareResult) Failure() (appobs.Failure, bool) {
	return result.failure, result.hasFailure
}
func (result ShareResult) Valid() bool {
	_, err := NewShareResult(ShareResultSpec{
		Publication: result.publication,
		ExitCode:    result.exitCode, Elapsed: result.elapsed,
		Failure: func() appobs.Failure {
			if result.hasFailure {
				return result.failure
			}
			return appobs.Failure{}
		}(),
	})
	return err == nil
}
