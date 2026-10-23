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
	Status              ResultStatus
	ExitCode            ExitCode
	Drift               DriftReason
	Elapsed             time.Duration
	Destination         appobs.DisplayPath
	DestinationAdjusted bool
	Files               appobs.FileOutcomes
	DirectoryFailures   uint64
	OmittedDiagnostics  uint64
	PublishedBytes      uint64
	CountersExact       bool
	Failure             appobs.Failure
}

type TransferResult struct {
	status              ResultStatus
	exitCode            ExitCode
	drift               DriftReason
	elapsed             time.Duration
	destination         appobs.DisplayPath
	destinationAdjusted bool
	files               appobs.FileOutcomes
	directoryFailures   uint64
	omittedDiagnostics  uint64
	publishedBytes      uint64
	countersExact       bool
	failure             appobs.Failure
	hasFailure          bool
}

func NewTransferResult(spec TransferResultSpec) (TransferResult, error) {
	if !spec.Status.Valid() || !spec.ExitCode.Valid() || spec.Elapsed < 0 || spec.Destination.Empty() ||
		spec.CountersExact && !appobs.ValidExactItemBlockCounts(spec.Files) {
		return TransferResult{}, ErrInvalidResult
	}
	if _, ok := spec.Drift.Name(); !ok {
		return TransferResult{}, ErrInvalidResult
	}
	hasFailure := spec.Failure.Valid()
	switch {
	case spec.Status == ResultSuccess && (spec.ExitCode != ExitSuccess || spec.Drift != DriftNone || hasFailure):
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
		status: spec.Status, exitCode: spec.ExitCode, drift: spec.Drift,
		elapsed: spec.Elapsed, destination: spec.Destination,
		destinationAdjusted: spec.DestinationAdjusted, files: spec.Files,
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
func (result TransferResult) Files() appobs.FileOutcomes      { return result.files }
func (result TransferResult) DirectoryFailures() uint64       { return result.directoryFailures }
func (result TransferResult) OmittedDiagnostics() uint64      { return result.omittedDiagnostics }
func (result TransferResult) PublishedBytes() uint64          { return result.publishedBytes }
func (result TransferResult) CountersExact() bool             { return result.countersExact }
func (result TransferResult) Failure() (appobs.Failure, bool) {
	return result.failure, result.hasFailure
}
func (result TransferResult) Valid() bool {
	_, err := NewTransferResult(TransferResultSpec{
		Status: result.status, ExitCode: result.exitCode, Drift: result.drift,
		Elapsed: result.elapsed, Destination: result.destination,
		DestinationAdjusted: result.destinationAdjusted, Files: result.files,
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

type ShareResultSpec struct {
	ExitCode ExitCode
	Elapsed  time.Duration
	Failure  appobs.Failure
}

type ShareResult struct {
	exitCode   ExitCode
	elapsed    time.Duration
	failure    appobs.Failure
	hasFailure bool
}

func NewShareResult(spec ShareResultSpec) (ShareResult, error) {
	hasFailure := spec.Failure.Valid()
	if !spec.ExitCode.Valid() || spec.Elapsed < 0 ||
		(spec.ExitCode == ExitSuccess) != !hasFailure || spec.ExitCode == ExitUsage || spec.ExitCode == ExitDrift {
		return ShareResult{}, ErrInvalidResult
	}
	return ShareResult{
		exitCode: spec.ExitCode, elapsed: spec.Elapsed,
		failure: spec.Failure, hasFailure: hasFailure,
	}, nil
}

func (result ShareResult) ExitCode() ExitCode     { return result.exitCode }
func (result ShareResult) Elapsed() time.Duration { return result.elapsed }
func (result ShareResult) StoppedCleanly() bool   { return result.exitCode == ExitSuccess }
func (result ShareResult) Failure() (appobs.Failure, bool) {
	return result.failure, result.hasFailure
}
func (result ShareResult) Valid() bool {
	_, err := NewShareResult(ShareResultSpec{
		ExitCode: result.exitCode, Elapsed: result.elapsed,
		Failure: func() appobs.Failure {
			if result.hasFailure {
				return result.failure
			}
			return appobs.Failure{}
		}(),
	})
	return err == nil
}
