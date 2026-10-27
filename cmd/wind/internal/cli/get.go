package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/windshare/windshare/adapters/filesystem"
	"github.com/windshare/windshare/cmd/wind/internal/clievent"
	"github.com/windshare/windshare/core/link"
	"github.com/windshare/windshare/engine"
)

type getRequest struct {
	outDir       string
	only         []string
	link         link.Link
	connectivity engine.ConnectivityPolicy
	observation  observationOptions
	waitTimeout  time.Duration
}

func (a *App) runGet(ctx context.Context, args []string) int {
	return a.runReceive(ctx, args, engine.ReceiveCreateOrResume)
}

func (a *App) runReceive(ctx context.Context, args []string, admission engine.ReceiveOutputAdmission) int {
	request, parsed := a.parseGetRequest(args)
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
	task, err := a.application.StartReceive(ctx, engine.ReceiveRequest{Admission: admission, Capability: request.link, Only: request.only, Connectivity: request.connectivity, WaitTimeout: request.waitTimeout, Output: factory, Destination: destination, Diagnostics: runtime.diagnosticSink(), OnPause: a.receivePauseHandler()})
	if err != nil {
		return observation.commandFailure(ExitFailure, err)
	}
	for value := range task.Observations() {
		observeEngineTask(runtime, value)
		a.projectReceiveObservation(observation, value.Event)
	}
	result, err := task.Wait(context.Background())
	if err != nil {
		return observation.commandFailure(ExitFailure, err)
	}
	return a.reportReceiveTask(result, observation)
}

const (
	invalidCapabilityDiagnostic    = "invalid capability link"
	missingCapabilityKeyDiagnostic = "key string is required"
)

func (a *App) parseGetRequest(args []string) (getRequest, requestParseOutcome) {
	flags := a.newFlagSet("get")
	var observation observationOptions
	if err := bindObservationOptions(flags, &observation); err != nil {
		a.writeCompleteLine("get: observation options are unavailable")
		return getRequest{}, requestParseInternalFailure
	}
	outDir := flags.String("o", ".", "output directory")
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
	if parse := a.projectFlagParse("get", flags, "get [options] <link>", flagParse); parse != requestParseReady {
		return getRequest{}, parse
	}
	if err := observation.validate(); err != nil {
		a.writeCompleteLine("get: %s", observationOptionDiagnostic(err))
		return getRequest{}, requestParseUsageFailure
	}
	if *waitTimeout < 0 {
		a.writeCompleteLine("get: wait-timeout must not be negative")
		return getRequest{}, requestParseUsageFailure
	}
	if len(positional) != 1 {
		a.writeCompleteLine("get: exactly one link argument is required")
		return getRequest{}, requestParseUsageFailure
	}
	connectivity, err := engine.ParseConnectivityPolicy(*connectivityName)
	if err != nil {
		a.writeCompleteLine("get: connectivity must be auto, relay-only, or p2p-only")
		return getRequest{}, requestParseUsageFailure
	}
	capability, err := a.resolveLink(positional[0], *keyString)
	if err != nil {
		if errors.Is(err, engine.ErrReceiveKeyRequired) {
			a.writeCompleteLine("get: %s", missingCapabilityKeyDiagnostic)
		} else {
			a.writeCompleteLine("get: %s", invalidCapabilityDiagnostic)
		}
		return getRequest{}, requestParseUsageFailure
	}
	return getRequest{
		outDir: *outDir, only: append([]string(nil), only...), link: capability, connectivity: connectivity,
		observation: observation, waitTimeout: *waitTimeout,
	}, requestParseReady
}

func (a *App) resolveLink(raw, keyString string) (link.Link, error) {
	input := engine.ReceiveLinkInput{URL: raw, SeparateKey: keyString}
	capability, err := engine.ResolveReceiveLink(input)
	if !errors.Is(err, engine.ErrReceiveKeyRequired) {
		return capability, err
	}
	_, _ = fmt.Fprint(a.stderrWriter(), "Link has no key; enter the key string: ")
	line, readErr := bufio.NewReader(a.Stdin).ReadString('\n')
	line = strings.TrimSpace(line)
	if line == "" {
		if readErr != nil {
			return link.Link{}, fmt.Errorf("%w: read key string: %w", engine.ErrReceiveKeyRequired, readErr)
		}
		return link.Link{}, engine.ErrReceiveKeyRequired
	}
	input.SeparateKey = line
	return engine.ResolveReceiveLink(input)
}
