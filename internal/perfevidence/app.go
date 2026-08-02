package perfevidence

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	defaultSampleCount        = MinimumBaselineSamples
	maximumCLIDiagnosticBytes = 2 << 10
)

type RunConfig struct {
	RepositoryRoot string
	OutputRoot     string
	SampleCount    int
	WorkloadIDs    []string
	ProfileIDs     []string
}

type Application struct {
	Commands        CommandRunner
	Logger          EventLogger
	Now             func() time.Time
	NewRunID        func() (string, error)
	Snapshots       SnapshotPreparer
	MutationDomains MutationDomainFactory
}

type SnapshotPreparer interface {
	Prepare(
		context.Context, CommandRunner, string, string, string, []Workload,
	) (PreparedSnapshot, error)
}

type SnapshotPreparerFunc func(
	context.Context, CommandRunner, string, string, string, []Workload,
) (PreparedSnapshot, error)

func (function SnapshotPreparerFunc) Prepare(
	ctx context.Context,
	runner CommandRunner,
	repositoryRoot, artifactRoot, runtimeRoot string,
	workloads []Workload,
) (PreparedSnapshot, error) {
	return function(ctx, runner, repositoryRoot, artifactRoot, runtimeRoot, workloads)
}

type productionSnapshotPreparer struct {
	mutationDomains MutationDomainFactory
}

func (preparer productionSnapshotPreparer) Prepare(
	ctx context.Context,
	runner CommandRunner,
	repositoryRoot, artifactRoot, runtimeRoot string,
	workloads []Workload,
) (PreparedSnapshot, error) {
	return prepareSnapshot(
		ctx, runner, repositoryRoot, artifactRoot, runtimeRoot, workloads, preparer.mutationDomains,
	)
}

func (application Application) Run(ctx context.Context, config RunConfig) (
	publication Publication,
	resultErr error,
) {
	if application.Commands == nil {
		return Publication{}, errors.New("performance evidence requires a command runner")
	}
	if application.MutationDomains == nil {
		return Publication{}, errors.New("performance evidence requires a private mutation domain factory")
	}
	if err := validateSampleCount(config.SampleCount); err != nil {
		return Publication{}, err
	}
	repositoryRoot, err := filepath.Abs(config.RepositoryRoot)
	if err != nil {
		return Publication{}, fmt.Errorf("resolve repository root: %w", err)
	}
	workloads, err := SelectWorkloads(config.WorkloadIDs)
	if err != nil {
		return Publication{}, err
	}
	profiled, err := profileSet(config.ProfileIDs, workloads)
	if err != nil {
		return Publication{}, err
	}
	now := application.Now
	if now == nil {
		now = time.Now
	}
	newRunID := application.NewRunID
	if newRunID == nil {
		newRunID = randomRunID
	}
	runID, err := newRunID()
	if err != nil {
		return Publication{}, fmt.Errorf("create performance run ID: %w", err)
	}
	startedAt := now().UTC()
	outputRoot := config.OutputRoot
	if outputRoot == "" {
		outputRoot = filepath.Join(repositoryRoot, "tmp", "performance-evidence")
	} else if !filepath.IsAbs(outputRoot) {
		outputRoot = filepath.Join(repositoryRoot, outputRoot)
	}
	outputRoot, err = filepath.Abs(outputRoot)
	if err != nil {
		return Publication{}, fmt.Errorf("resolve evidence output root: %w", err)
	}
	// The retained directory handle is the authority validated below and then
	// consumed by recovery, staging, and publication. Opening it first prevents
	// validation of one pathname object followed by mutation of another.
	outputAuthority, err := openOutputRootAuthority(outputRoot)
	if err != nil {
		return Publication{}, err
	}
	if err := validateEvidenceOutputAuthority(
		ctx, application.Commands, repositoryRoot, outputAuthority, runID,
	); err != nil {
		return Publication{}, errors.Join(err, outputAuthority.close())
	}
	stage, err := newStageWithAuthority(outputAuthority, runID, time.Now().UTC(), nil)
	if err != nil {
		return Publication{}, err
	}
	defer func() { resultErr = errors.Join(resultErr, stage.Abort()) }()
	snapshots := application.Snapshots
	if snapshots == nil {
		snapshots = productionSnapshotPreparer{mutationDomains: application.MutationDomains}
	}
	if err := application.log(runID, "source-snapshot-started", "running", ""); err != nil {
		return Publication{}, fmt.Errorf("log source snapshot start: %w", err)
	}
	snapshot, err := snapshots.Prepare(
		ctx, application.Commands, repositoryRoot, stage.ArtifactRoot, stage.RuntimeRoot, workloads,
	)
	if err != nil {
		logErr := application.log(runID, "source-snapshot-finished", "failed", err.Error())
		return Publication{}, errors.Join(fmt.Errorf("prepare sealed source snapshot: %w", err), logErr)
	}
	defer func() { resultErr = errors.Join(resultErr, snapshot.Close()) }()
	if err := application.log(runID, "source-snapshot-finished", "succeeded", snapshot.Identity.SHA256); err != nil {
		return Publication{}, fmt.Errorf("log source snapshot completion: %w", err)
	}
	host := InspectHost(ctx, application.Commands, repositoryRoot)
	host.RequiredErrors = requiredIdentityErrors(host, snapshot.Identity)
	evidence := Evidence{
		SchemaVersion: SchemaVersion, Kind: EvidenceKind, RunID: runID, Status: "running",
		StartedAt: startedAt, Source: snapshot.Identity,
		Host:     host,
		Sampling: samplingPolicy(config.SampleCount),
	}
	measurementCommands := application.Commands
	mutationDomain := snapshot.domain
	mutationDomainClosed := false
	if application.MutationDomains != nil && mutationDomain == nil {
		mutationDomain, err = application.MutationDomains.Open(ctx, MutationDomainSpec{
			RuntimeRoot: filepath.Join(stage.OutputRoot, stage.runtimeName),
			Roots: []MutationRoot{
				{Name: "snapshot", HostPath: snapshot.Root},
				{Name: "goroot", HostPath: snapshot.Environment.ToolchainLocations.GoRoot},
				{Name: "gomodcache", HostPath: snapshot.Environment.GoModCache},
			},
		})
		if err != nil {
			return Publication{}, fmt.Errorf("open private mutation domain: %w", err)
		}
		if mutationDomain == nil {
			return Publication{}, errors.New("private mutation domain factory returned no domain")
		}
	}
	if mutationDomain != nil {
		defer func() {
			if !mutationDomainClosed {
				resultErr = errors.Join(resultErr, mutationDomain.Close())
			}
		}()
		measurementCommands, err = runnerWithMutationDomain(application.Commands, mutationDomain)
		if err != nil {
			return Publication{}, err
		}
	}
	measurementRunner := Runner{Commands: measurementCommands, Logger: application.Logger, RunID: runID}
	var runFailure error
	for _, workload := range workloads {
		measured, measureErr := measurementRunner.Measure(
			ctx, stage.ArtifactRoot, workload, snapshot.Workloads[workload.ID], snapshot.Environment,
			config.SampleCount, profiled[workload.ID],
		)
		evidence.Workloads = append(evidence.Workloads, measured)
		if measureErr != nil {
			runFailure = fmt.Errorf("measure workload %s: %w", workload.ID, measureErr)
			break
		}
	}
	if err := application.log(runID, "source-revalidation-started", "running", ""); err != nil {
		return Publication{}, fmt.Errorf("log source revalidation start: %w", err)
	}
	if validationErr := snapshot.Revalidate(); validationErr != nil {
		logErr := application.log(runID, "source-revalidation-finished", "failed", validationErr.Error())
		return Publication{}, errors.Join(
			runFailure, fmt.Errorf("revalidate sealed source snapshot: %w", validationErr), logErr,
		)
	}
	if err := application.log(runID, "source-revalidation-finished", "succeeded", snapshot.Identity.SHA256); err != nil {
		return Publication{}, errors.Join(runFailure, fmt.Errorf("log source revalidation completion: %w", err))
	}
	if mutationDomain != nil {
		if err := mutationDomain.Close(); err != nil {
			return Publication{}, fmt.Errorf("close private mutation domain: %w", err)
		}
		mutationDomainClosed = true
	}
	evidence.FinishedAt = now().UTC()
	evidence.Baseline = assessBaseline(
		config.SampleCount,
		snapshot.Identity,
		host,
		len(workloads),
		len(DefaultWorkloads()),
		runFailure,
	)
	if runFailure == nil {
		evidence.Status = "succeeded"
	} else {
		evidence.Status = "failed"
		evidence.Error = runFailure.Error()
	}
	if err := application.log(runID, "publication-started", "running", ""); err != nil {
		return Publication{}, errors.Join(runFailure, fmt.Errorf("log publication start: %w", err))
	}
	publication, publishErr := stage.Commit(evidence, snapshot.Revalidate, snapshot.Close)
	if publishErr != nil {
		logErr := application.log(runID, "publication-finished", "failed", publishErr.Error())
		return publication, errors.Join(runFailure, publishErr, logErr)
	}
	logErr := application.log(runID, "publication-finished", "succeeded", publication.EvidenceID)
	return publication, errors.Join(runFailure, logErr)
}

func validateSampleCount(sampleCount int) error {
	if sampleCount < 1 {
		return errors.New("sample count must be positive")
	}
	if sampleCount > MaximumSampleCount {
		return fmt.Errorf("sample count must not exceed %d", MaximumSampleCount)
	}
	return nil
}

func samplingPolicy(sampleCount int) SamplingPolicy {
	classification := "ad-hoc"
	if sampleCount >= MinimumBaselineSamples {
		classification = "baseline-sized"
	}
	return SamplingPolicy{
		ProcessesPerWorkload: sampleCount, MinimumBaselineSamples: MinimumBaselineSamples,
		Classification: classification,
	}
}

func assessBaseline(
	sampleCount int,
	source SnapshotIdentity,
	host HostMetadata,
	selectedWorkloads int,
	maintainedWorkloads int,
	runFailure error,
) BaselineAssessment {
	var reasons []string
	if sampleCount < MinimumBaselineSamples {
		reasons = append(reasons, "insufficient-samples")
	}
	if source.Git.WorktreeDirty {
		reasons = append(reasons, "dirty-source")
	}
	if source.SHA256 == "" {
		reasons = append(reasons, "missing-source-snapshot")
	}
	if !source.CompiledInputsMatchCommit {
		reasons = append(reasons, "uncommitted-build-input")
	}
	if len(host.RequiredErrors) != 0 {
		reasons = append(reasons, "incomplete-host-build-identity")
	}
	if selectedWorkloads != maintainedWorkloads {
		reasons = append(reasons, "partial-workload-set")
	}
	if runFailure != nil {
		reasons = append(reasons, "measurement-failed")
	}
	return BaselineAssessment{Eligible: len(reasons) == 0, Reasons: reasons}
}

type JSONLogger struct {
	Writer io.Writer
	mu     sync.Mutex
}

func (logger *JSONLogger) Log(event Event) error {
	if logger == nil || logger.Writer == nil {
		return nil
	}
	encoded, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("encode structured event: %w", err)
	}
	logger.mu.Lock()
	defer logger.mu.Unlock()
	if _, err := fmt.Fprintln(logger.Writer, string(encoded)); err != nil {
		return fmt.Errorf("write structured event: %w", err)
	}
	return nil
}

func Main(ctx context.Context, arguments []string, stdout, stderr io.Writer) int {
	return mainWithMutationDomains(ctx, arguments, stdout, stderr, nil)
}

func MainWithMutationDomains(
	ctx context.Context,
	arguments []string,
	stdout, stderr io.Writer,
	factory MutationDomainFactory,
) int {
	return mainWithMutationDomains(ctx, arguments, stdout, stderr, factory)
}

func mainWithMutationDomains(
	ctx context.Context,
	arguments []string,
	stdout, stderr io.Writer,
	factory MutationDomainFactory,
) int {
	flags := flag.NewFlagSet("perfevidence", flag.ContinueOnError)
	flags.SetOutput(stderr)
	repository := flags.String("repository", ".", "repository root")
	output := flags.String("output", "", "content-addressed evidence root")
	samples := flags.Int("samples", defaultSampleCount, "fresh process samples per workload")
	workloadList := flags.String("workloads", "", "comma-separated workload IDs; empty selects all")
	profileList := flags.String("profile", "", "comma-separated workload IDs to profile, or all")
	list := flags.Bool("list", false, "list workload IDs without running them")
	if err := flags.Parse(arguments); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		writeCLIDiagnostic(stderr, "perfevidence does not accept positional arguments")
		return 2
	}
	if *list {
		for _, id := range WorkloadIDs() {
			if _, err := fmt.Fprintln(stdout, id); err != nil {
				writeCLIDiagnostic(stderr, "write workload list: "+err.Error())
				return 1
			}
		}
		return 0
	}
	config := RunConfig{
		RepositoryRoot: *repository, OutputRoot: *output, SampleCount: *samples,
		WorkloadIDs: splitList(*workloadList), ProfileIDs: splitList(*profileList),
	}
	application := Application{
		Commands: ProcessRunner{}, Logger: &JSONLogger{Writer: stderr}, MutationDomains: factory,
	}
	publication, err := application.Run(ctx, config)
	if publication.Path != "" {
		if outputErr := writePublicationResult(stdout, publication); outputErr != nil {
			writeCLIDiagnostic(stderr, "write publication result: "+outputErr.Error())
			return 1
		}
	}
	if err != nil {
		writeCLIDiagnostic(stderr, "performance evidence failed: "+err.Error())
		return 1
	}
	return 0
}

func writePublicationResult(writer io.Writer, publication Publication) error {
	result := struct {
		EvidenceID string `json:"evidenceId"`
		Path       string `json:"path"`
	}{publication.EvidenceID, publication.Path}
	if err := json.NewEncoder(writer).Encode(result); err != nil {
		return fmt.Errorf("encode publication result: %w", err)
	}
	return nil
}

func writeCLIDiagnostic(writer io.Writer, message string) {
	if len(message) > maximumCLIDiagnosticBytes {
		message = message[:maximumCLIDiagnosticBytes]
	}
	_, _ = fmt.Fprintln(writer, message)
}

func profileSet(requested []string, workloads []Workload) (map[string]bool, error) {
	selected := make(map[string]bool, len(requested))
	available := make(map[string]struct{}, len(workloads))
	for _, workload := range workloads {
		available[workload.ID] = struct{}{}
	}
	if len(requested) == 1 && requested[0] == "all" {
		for id := range available {
			selected[id] = true
		}
		return selected, nil
	}
	for _, id := range requested {
		if _, found := available[id]; !found {
			return nil, fmt.Errorf("profile workload %q is not selected", id)
		}
		if selected[id] {
			return nil, fmt.Errorf("profile workload %q was selected more than once", id)
		}
		selected[id] = true
	}
	return selected, nil
}

func splitList(value string) []string {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	parts := strings.Split(value, ",")
	result := make([]string, 0, len(parts))
	for _, part := range parts {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			result = append(result, trimmed)
		}
	}
	return result
}

func randomRunID() (string, error) {
	identifier := make([]byte, 16)
	if _, err := rand.Read(identifier); err != nil {
		return "", err
	}
	return hex.EncodeToString(identifier), nil
}

func (application Application) log(runID, milestone, outcome, detail string) error {
	if application.Logger == nil {
		return nil
	}
	return application.Logger.Log(Event{
		Timestamp: time.Now().UTC(), RunID: runID, OperationID: runID,
		Scenario: "performance-evidence", Component: "perfevidence",
		Milestone: milestone, Outcome: outcome, Detail: detail,
	})
}
