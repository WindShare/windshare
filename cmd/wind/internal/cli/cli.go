// Package cli 承载 wind 的子命令逻辑(执行计划 §6.9)。与 main 分离:
// IO(stdin/stdout/stderr)全部注入,命令函数返回退出码而非直接 os.Exit,
// 集成测试得以在进程内驱动完整 share/get 流程(DfT)。
package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"time"

	"github.com/windshare/windshare/cmd/wind/internal/accountcommand"
	"github.com/windshare/windshare/cmd/wind/internal/command"
	"github.com/windshare/windshare/cmd/wind/internal/configcommand"
	"github.com/windshare/windshare/cmd/wind/internal/networkcommand"
	"github.com/windshare/windshare/core/diagnostics"
	"github.com/windshare/windshare/engine"
	"github.com/windshare/windshare/engine/builtinnetwork"
	"github.com/windshare/windshare/engine/relayconfig"
	"github.com/windshare/windshare/engine/relaysetup"
	"github.com/windshare/windshare/internal/platformsetup"
	"github.com/windshare/windshare/internal/powerinhibit"
)

// 退出码语义(§6.9 工程要求):脚本据此区分"该重试"(网络)与"该改命令"
// (用户错误)与"该重新分享"(漂移)。唯一定义在 internal/command。
const (
	ExitOK      = int(command.ExitOK)
	ExitFailure = int(command.ExitFailure)
	ExitUsage   = int(command.ExitUsage)
	ExitNetwork = int(command.ExitNetwork)
	ExitDrift   = int(command.ExitDrift)
)

const usageText = `Usage:
  ` + command.Name + ` share <path...> [--relay <url>] [--block-size <bytes>] [--split-key] [--front-url <url>] [--browser-trace] [-v|--verbose] [--trace <file>|--trace-dir <directory>]
      Commit selected roots, wait for relay registration, print a suite-02 link, and scan descendants on demand.
      --split-key prints a bare link and key string for delivery over separate channels.
      Sharing defaults load from the user config directory's windshare/config.json (or WINDSHARE_CONFIG).
      --front-url overrides the saved frontend; otherwise links use ws.ourworld.top.
      --configured-relays-only disables built-in candidates for this share.
      --relay overrides the saved candidate set, retaining credentials for matching addresses.

  ` + command.Name + ` get <link> [--only <path>]... [--key <key-string>] [--connectivity auto|relay-only|p2p-only] [-v|--verbose] [--trace <file>|--trace-dir <directory>]
  ` + command.Name + ` get -o <directory> <link> [--only <path>]... [--key <key-string>] [--connectivity auto|relay-only|p2p-only] [-v|--verbose] [--trace <file>|--trace-dir <directory>]
      Save one ordinary named result inside the output container; -o defaults to the current directory.
      Compatible active downloads reuse their frozen result name and verified progress.
      Temporary outages reconnect automatically; --wait-timeout <duration> limits initial or each later connection wait.
      The default waits 10 seconds on first join and indefinitely after joining; Ctrl-C preserves resumable progress.
      relay-only skips direct peer setup and transfers content through the configured relay.
      p2p-only uses the relay for bootstrap and signaling but never for content; direct-path failure stops the download.
      If the link has no key, use --key or enter the key interactively.
      -v and --verbose show static diagnostic milestones; --trace creates one exact file and --trace-dir generates one run-specific file.
      Trace targets are mutually exclusive; '-' is unsupported because trace data never shares a human or capability stream.

  ` + command.Name + ` relay add <url> [--access-key <key>] [--name <device>] [--no-browser]
  ` + command.Name + ` relay onboard <url> [--name <device>] [--no-browser]
      Pair this device in the browser. New relays are enabled when a slot is available.
  ` + command.Name + ` relay list | mode <automatic|selected> | enable|disable <url> | frontend <url|default>
  ` + command.Name + ` relay cancel <url>
      Cancel a pending enrollment.

  ` + command.Name + ` account status [--json] | register | refresh
  ` + command.Name + ` account login <username> | password <username> [--password-stdin]
  ` + command.Name + ` account email <address> | verify <code> | revoke <credential>
      Manage the account shared with the desktop app. Login applies to new shares.

  ` + command.Name + ` config import <file|-> [--front-url <url>] [-v|--verbose]
      Validate and replace sharing defaults. --front-url saves the website address; '-' reads JSON from stdin.

  ` + command.Name + ` doctor network [--json]
  ` + command.Name + ` setup network [--skip|--remove] [--non-interactive] [--json]

  ` + command.Name + ` resume retry [-o <directory>] [--only <path>] <link>
      Continue an existing download with its original link and selection.

  ` + command.Name + ` resume list -o <directory>
      List destination-owned incomplete, resumable, cleanup-pending, and attention state.
      Locally ambiguous children are reported as item-blocked.

  ` + command.Name + ` resume discard -o <directory> --item <N>
      Re-list and exactly confirm one operation on a terminal before discarding identity-matched unfinished state.
      Published files and unknown objects are always preserved.
`

// App 是一次 CLI 调用的执行环境;字段即全部外部依赖。
type App struct {
	Stdout io.Writer
	Stderr io.Writer
	Stdin  io.Reader

	terminal             terminalOutputState
	clock                commandClock
	terminalCapabilities terminalCapabilityProvider
	terminalCellWidth    terminalCellWidthFunc
	openUserTrace        userTraceOpener

	receiverRecoveryOptions engine.ReceiveRecoverySettings
	receiverPeerFactory     func() (engine.ReceivePeerStarter, error)
	receiverDial            func(context.Context, engine.ReceiveDialRequest) (engine.ReceiveConnection, error)
	application             *engine.Engine
	engineConfig            engine.Config
	getOutputFactory        engine.OutputFactory
	networkSetup            *platformsetup.Service
	loadShareConfig         func() (relayconfig.Config, error)
	saveShareConfig         func(relayconfig.Config) error
	openBrowser             func(context.Context, string) error
}

// Main 是 os 进程入口的接线:真实标准流 + 终止信号取消(Ctrl-C、SIGTERM、
// SIGHUP 都是"停止分享"/"中断下载"语义,§6.9)。
func Main() int {
	configPath, err := relayconfig.Path()
	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		return ExitFailure
	}
	network, err := builtinnetwork.Config(configPath)
	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		return ExitFailure
	}
	app := &App{
		engineConfig: engine.Config{Network: network, SleepInhibitor: powerinhibit.New()},
		Stdout:       os.Stdout, Stderr: os.Stderr, Stdin: os.Stdin,
		networkSetup:    platformsetup.New(),
		loadShareConfig: relayconfig.Load,
		saveShareConfig: relayconfig.Save,
		openBrowser:     openRelayBrowser,
	}
	interrupts := make(chan os.Signal, interruptSignalBuffer)
	defer notifyTerminationSignals(interrupts, signal.Ignored)()
	app.engineConfig.RelaySetup.Store = relaysetup.FileStore{Settings: relayconfig.FileStore{}}
	code := runCLIWithInterruptEscalation(
		interrupts,
		os.Exit,
		func(ctx context.Context) int { return app.Run(ctx, os.Args[1:]) },
	)
	app.closeTerminalOutput()
	return code
}

// Run dispatches top-level commands. Every command parses arguments and answers
// help requests before acquiring transfer-engine or OS authority.
func (a *App) Run(ctx context.Context, args []string) int {
	if len(args) == 0 {
		a.usage()
		return ExitUsage
	}
	if command.IsHelpToken(args[0]) {
		a.usage()
		return ExitOK
	}
	switch args[0] {
	case "share":
		return a.runShare(ctx, args[1:])
	case "get":
		return a.runGet(ctx, args[1:])
	case "relay":
		return a.runRelay(ctx, args[1:])
	case "account":
		return a.runAccount(ctx, args[1:])
	case "config":
		return a.runConfig(ctx, args[1:])
	case "doctor", "setup":
		return a.runNetwork(ctx, args[0], args[1:])
	case "resume":
		return a.runResume(ctx, args[1:])
	default:
		// A command token is untrusted input and can itself be a capability or
		// private credential accidentally pasted in the wrong position.
		a.writeCompleteLine("%s: unknown command", command.Name)
		a.usage()
		return ExitUsage
	}
}

func (a *App) runNetwork(ctx context.Context, cmd string, args []string) int {
	service := a.networkSetup
	if service == nil {
		service = platformsetup.New()
	}
	runner := networkcommand.Runner{
		Service:    service,
		Output:     a.stderrLines(),
		JSONOutput: a.Stdout,
		Interactive: func() bool {
			return networkcommand.IsInteractive(a.Stdin, a.Stderr)
		},
	}
	return runner.Run(ctx, cmd, args).Int()
}

func (a *App) runConfig(ctx context.Context, args []string) int {
	output := a.stderrLines()
	runner := configcommand.Runner{
		Input:        a.Stdin,
		Output:       output,
		TraceHandler: output.jsonHandler(),
		Save:         a.saveShareConfig,
	}
	return runner.Run(ctx, args).Int()
}

func (a *App) runAccount(ctx context.Context, args []string) (code int) {
	request, exit, ready := accountcommand.Parse(args, a.stderrLines())
	if !ready {
		return exit.Int()
	}
	closeApp, err := a.ensureApplication(diagnostics.Sink{})
	if err != nil {
		return ExitFailure
	}
	defer closeApp(&code)
	runner := accountcommand.Runner{Input: a.Stdin, Output: a.stderrLines(), JSONOutput: a.Stdout}
	if service := a.application.Accounts(); service != nil {
		runner.Service = service
	}
	return runner.Execute(ctx, request).Int()
}

// ensureApplication constructs a command-scoped engine after flags and trace
// streams are parsed so startup directory and frontend checks enter the trace.
// Deferring the returned closer after commandRuntime.Close joins engine loops
// before the diagnostic stream seals.
func (a *App) ensureApplication(diagnosticSink diagnostics.Sink) (func(*int), error) {
	if a.application != nil {
		return func(*int) {}, nil
	}
	application, err := a.newApplication(diagnosticSink)
	if err != nil {
		a.writeCompleteLine("%s: initialize application: %v", command.Name, err)
		return nil, err
	}
	a.application = application
	return func(code *int) {
		if err := application.Close(context.Background()); err != nil {
			a.writeCompleteLine("%s: close application: %v", command.Name, err)
			if code != nil && *code == ExitOK {
				*code = ExitFailure
			}
		}
		a.application = nil
	}, nil
}

type engineCommandClock struct{ commandClock }

func (c engineCommandClock) NewTicker(d time.Duration) engine.ReceiveTicker {
	return c.commandClock.NewTicker(d)
}
func (c engineCommandClock) Wait(ctx context.Context, delay time.Duration, wake <-chan struct{}) error {
	timer := c.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-wake:
	case <-timer.C():
	}
	return nil
}

func (a *App) newApplication(diagnosticSink diagnostics.Sink) (*engine.Engine, error) {
	config := a.engineConfig
	if diagnosticSink.Enabled() {
		config.Diagnostics = diagnosticSink
	}
	if config.Clock == nil && a.clock != nil {
		config.Clock = engineCommandClock{a.clock}
	}
	config.Receive = a.receiveDependencies()
	return engine.New(config)
}

func (a *App) usage() {
	a.stderrLines().WriteLines(usageText)
}

// writeCompleteLine is reserved for command parsing, prompts, resume, and
// private-test diagnostics that occur outside a command event runtime.
func (a *App) writeCompleteLine(format string, args ...any) {
	message := fmt.Sprintf(format, args...)
	a.stderrLines().WriteLines(message)
}

// stderrLines keeps record boundaries explicit while TerminalCanvas coordinates
// every insertion with the active command progress row.
func (a *App) stderrLines() canvasLineOutput {
	return canvasLineOutput{canvas: a.terminalOutput().canvas}
}

type requestParseOutcome uint8

const (
	requestParseReady requestParseOutcome = iota
	requestParseHelp
	requestParseUsageFailure
	requestParseCancelled
	requestParseInternalFailure
)

func (outcome requestParseOutcome) exitCode() int {
	switch outcome {
	case requestParseReady, requestParseHelp:
		return ExitOK
	case requestParseUsageFailure:
		return ExitUsage
	case requestParseCancelled, requestParseInternalFailure:
		return ExitFailure
	default:
		return ExitFailure
	}
}

func (a *App) projectFlagParse(
	name string,
	fs *flag.FlagSet,
	synopsis string,
	outcome command.ParseOutcome,
) requestParseOutcome {
	switch outcome {
	case command.ParseReady:
		return requestParseReady
	case command.ParseHelp:
		command.WriteFlagHelp(a.stderrLines(), fs, synopsis)
		return requestParseHelp
	case command.ParseInternalFailure:
		a.writeCompleteLine("%s: command arguments could not be parsed", name)
		return requestParseInternalFailure
	default:
		a.writeCompleteLine("%s: %s", name, outcome.Diagnostic())
		return requestParseUsageFailure
	}
}
