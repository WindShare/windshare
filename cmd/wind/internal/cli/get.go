package cli

import (
	"bufio"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/windshare/windshare/adapters/filesystem"
	"github.com/windshare/windshare/cmd/wind/internal/clievent"
	"github.com/windshare/windshare/cmd/wind/internal/networkcommand"
	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/link"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/receivecontract"
	"github.com/windshare/windshare/engine"
)

type receiveCommand string

const (
	receiveGet                    receiveCommand = "get"
	receiveResumeRetry            receiveCommand = "resume retry"
	defaultReceiveOutputDirectory                = "."
)

type receiveRequest struct {
	admission    engine.ReceiveOutputAdmission
	operationID  receivecontract.OperationID
	outDir       string
	selection    receiveSelection
	link         link.Link
	connectivity engine.ConnectivityPolicy
	observation  observationOptions
	waitTimeout  time.Duration
}

func (a *App) runGet(ctx context.Context, args []string) int {
	return a.runReceive(ctx, receiveGet, args)
}

func (a *App) runReceive(ctx context.Context, command receiveCommand, args []string) int {
	request, parsed := a.parseReceiveRequest(command, args)
	if parsed != requestParseReady {
		return parsed.exitCode()
	}
	runtime, err := a.newCommandRuntime(clievent.CommandGet, request.observation)
	if err != nil {
		return ExitFailure
	}
	observation := newGetObservation(runtime)
	defer func() { observation.completeAndFinalize(); runtime.Close() }()
	destination, err := filepath.Abs(request.outDir)
	if err != nil {
		return observation.commandFailure(ExitFailure, err)
	}
	factory := a.getOutputFactory
	if factory == nil {
		factory = filesystem.FilesystemOutput{RootPath: destination, CreateRoot: true}
	}
	task, err := a.application.StartReceive(ctx, engine.ReceiveRequest{Admission: request.admission, OperationID: request.operationID, Capability: request.link, Only: request.selection.paths, Connectivity: request.connectivity, WaitTimeout: request.waitTimeout, Output: factory, Destination: destination, Diagnostics: runtime.diagnosticSink(), OnPause: a.receivePauseHandler()})
	if err != nil {
		return runtime.failToStart(err)
	}
	if a.networkSetup != nil && request.connectivity != engine.ConnectivityRelayOnly {
		networkcommand.ObserveStartup(a.networkSetup, runtime, clievent.CommandGet)
	}
	for value := range task.Observations() {
		observeEngineTask(runtime, value)
		a.projectReceiveObservation(observation, value.Event)
	}
	result, err := task.Wait(context.Background())
	if err != nil {
		return observation.commandFailure(ExitFailure, err)
	}
	return a.reportReceiveTask(result, request.selection, observation)
}

const (
	invalidCapabilityDiagnostic    = "invalid capability link"
	missingCapabilityKeyDiagnostic = "key string is required"
)

func (a *App) parseReceiveRequest(command receiveCommand, args []string) (receiveRequest, requestParseOutcome) {
	name := string(command)
	flags := a.newFlagSet(name)
	admission := engine.ReceiveCreateOrResume
	var redownload bool
	var operation string
	// The commands share transfer options, but expose only the operation choices
	// their contracts allow. Retry can never silently become a new download.
	switch command {
	case receiveGet:
		flags.BoolVar(&redownload, "redownload", false, "start a separate download in a new reserved destination; retain the original task")
	case receiveResumeRetry:
		admission = engine.ReceiveResumeExisting
		flags.StringVar(&operation, "operation", "", "retained operation ID")
	default:
		return receiveRequest{}, requestParseInternalFailure
	}
	var observation observationOptions
	if err := bindObservationOptions(flags, &observation); err != nil {
		a.writeCompleteLine("%s: observation options are unavailable", name)
		return receiveRequest{}, requestParseInternalFailure
	}
	outputHelp := "output directory"
	if command == receiveResumeRetry {
		outputHelp = "directory to search for saved downloads"
	}
	outDir := flags.String("o", defaultReceiveOutputDirectory, outputHelp)
	waitTimeout := flags.Duration("wait-timeout", 0, "maximum wait for initial connection or each reconnection (0: initial 10s, reconnection unlimited)")
	keyString := flags.String("key", "", "separate key string when the link has no fragment")
	connectivityName := flags.String(
		"connectivity",
		engine.ConnectivityAuto.String(),
		"content connectivity policy: auto, relay-only, or p2p-only",
	)
	var only repeatedFlag
	flags.Var(&only, "only", "download only this catalog path; repeatable, directories include descendants")
	positional, flagParse := parseInterleaved(flags, args)
	if parse := a.projectFlagParse(name, flags, name+" [options] <link>", flagParse); parse != requestParseReady {
		return receiveRequest{}, parse
	}
	if err := observation.validate(); err != nil {
		a.writeCompleteLine("%s: %s", name, observationOptionDiagnostic(err))
		return receiveRequest{}, requestParseUsageFailure
	}
	if *outDir == "" {
		a.writeCompleteLine("%s: -o must name an output directory", name)
		return receiveRequest{}, requestParseUsageFailure
	}
	if *waitTimeout < 0 {
		a.writeCompleteLine("%s: wait-timeout must not be negative", name)
		return receiveRequest{}, requestParseUsageFailure
	}
	if len(positional) != 1 {
		a.writeCompleteLine("%s: exactly one link argument is required", name)
		return receiveRequest{}, requestParseUsageFailure
	}
	selection, err := normalizeReceiveSelection(only)
	if err != nil {
		a.writeCompleteLine("%s: %v", name, err)
		return receiveRequest{}, requestParseUsageFailure
	}
	var operationID receivecontract.OperationID
	if operation != "" {
		raw, err := hex.DecodeString(operation)
		if err == nil {
			operationID, err = receivecontract.OperationIDFromBytes(raw)
		}
		if err != nil {
			a.writeCompleteLine("%s: invalid operation ID", name)
			return receiveRequest{}, requestParseUsageFailure
		}
	}
	connectivity, err := engine.ParseConnectivityPolicy(*connectivityName)
	if err != nil {
		a.writeCompleteLine("%s: connectivity must be auto, relay-only, or p2p-only", name)
		return receiveRequest{}, requestParseUsageFailure
	}
	capability, err := a.resolveLink(positional[0], *keyString)
	if err != nil {
		if errors.Is(err, engine.ErrReceiveKeyRequired) {
			a.writeCompleteLine("%s: %s", name, missingCapabilityKeyDiagnostic)
		} else {
			a.writeCompleteLine("%s: %s", name, invalidCapabilityDiagnostic)
		}
		return receiveRequest{}, requestParseUsageFailure
	}
	if redownload {
		admission = engine.ReceiveRedownload
	}
	return receiveRequest{
		admission: admission, operationID: operationID,
		outDir: *outDir, selection: selection, link: capability, connectivity: connectivity,
		observation: observation, waitTimeout: *waitTimeout,
	}, requestParseReady
}

// receiveSelection pairs each canonical --only target with the spelling the
// user typed first, so reports can echo command-line input instead of claiming
// that normalized catalog identity is what the user wrote.
type receiveSelection struct {
	// paths are canonical, deduplicated, and in command-line order.
	paths []string
	// typed[i] is the first command-line spelling of paths[i].
	typed []string
}

// CLI spellings may be equivalent even though core selection identity must be
// canonical. Keep catalog paths independent of the host filesystem's path rules.
func normalizeReceiveSelection(requested []string) (receiveSelection, error) {
	if len(requested) == 0 {
		return receiveSelection{}, nil
	}
	seen := make(map[string]struct{}, min(len(requested), transfer.MaxSelectionPathTargets))
	paths := make([]string, 0, min(len(requested), transfer.MaxSelectionPathTargets))
	typed := make([]string, 0, cap(paths))
	var totalBytes uint64
	for _, raw := range requested {
		path, err := catalog.CanonicalPath(strings.TrimSuffix(raw, "/"))
		if err != nil {
			return receiveSelection{}, fmt.Errorf("--only %q: %w", raw, err)
		}
		if _, duplicate := seen[path]; duplicate {
			continue
		}
		if len(paths) == transfer.MaxSelectionPathTargets {
			return receiveSelection{}, fmt.Errorf("--only: select at most %d distinct catalog paths", transfer.MaxSelectionPathTargets)
		}
		totalBytes += uint64(len(path))
		if totalBytes > transfer.MaxSelectionPathTargetBytes {
			return receiveSelection{}, fmt.Errorf("--only: catalog paths exceed the %d-byte total limit", transfer.MaxSelectionPathTargetBytes)
		}
		seen[path] = struct{}{}
		paths = append(paths, path)
		typed = append(typed, raw)
	}
	return receiveSelection{paths: paths, typed: typed}, nil
}

func (a *App) resolveLink(raw, keyString string) (link.Link, error) {
	input := engine.ReceiveLinkInput{URL: raw, SeparateKey: keyString}
	capability, err := engine.ResolveReceiveLink(input)
	if !errors.Is(err, engine.ErrReceiveKeyRequired) {
		return capability, err
	}
	a.stderrLines().WriteLines("Link has no key; enter the key string: ")
	line, readErr := bufio.NewReader(a.Stdin).ReadString('\n')
	// Use the same trimming as --key and the browser key box.
	line = link.TrimCopiedText(line)
	if line == "" {
		if readErr != nil {
			return link.Link{}, fmt.Errorf("%w: read key string: %w", engine.ErrReceiveKeyRequired, readErr)
		}
		return link.Link{}, engine.ErrReceiveKeyRequired
	}
	input.SeparateKey = line
	return engine.ResolveReceiveLink(input)
}
