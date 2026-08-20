// Package cli 承载 wind 的子命令逻辑(执行计划 §6.9)。与 main 分离:
// IO(stdin/stdout/stderr)全部注入,命令函数返回退出码而非直接 os.Exit,
// 集成测试得以在进程内驱动完整 share/get 流程(DfT)。
package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"

	"github.com/windshare/windshare/cmd/wind/internal/commandmeta"
)

// 退出码语义(§6.9 工程要求):脚本据此区分"该重试"(网络)与"该改命令"
// (用户错误)与"该重新分享"(漂移)。
const (
	ExitOK      = 0
	ExitFailure = 1 // 运行期失败:传输中断、本地 IO、内部错误
	ExitUsage   = 2 // 用户错误:参数、链接/密钥、路径选择、清单超限
	ExitNetwork = 3 // 网络/中转不可达或被中转拒绝
	ExitDrift   = 4 // 快照漂移中止:分享期间文件被修改(§6.3)
)

// The local defaults describe the browser-facing gateway started by dev.ps1.
// Vite owns that origin and proxies the relay path, so generated capabilities
// never expose the relay's private development listener.
const (
	DefaultRelayURL = "ws://localhost:38384"
	DefaultFrontURL = "http://localhost:38384"
)

const usageText = `Usage:
  ` + commandmeta.Name + ` share <path...> [--relay <url>] [--block-size <bytes>] [--split-key] [--front-url <url>] [-v|--verbose] [--trace <file>|--trace-dir <directory>]
      Commit selected roots, wait for relay registration, print a suite-02 link, and scan descendants on demand.
      --split-key prints a bare link and key string for delivery over separate channels.

  ` + commandmeta.Name + ` get <link> [--only <path>]... [--key <key-string>] [--connectivity auto|relay-only|p2p-only] [-v|--verbose] [--trace <file>|--trace-dir <directory>]
  ` + commandmeta.Name + ` get -o <directory> <link> [--only <path>]... [--key <key-string>] [--connectivity auto|relay-only|p2p-only] [-v|--verbose] [--trace <file>|--trace-dir <directory>]
      Save one ordinary named result inside the output container; -o defaults to the current directory.
      Compatible active downloads reuse their frozen result name and verified progress.
      relay-only skips direct peer setup and transfers content through the configured relay.
      p2p-only uses the relay for bootstrap and signaling but never for content; direct-path failure stops the download.
      If the link has no key, use --key or enter the key interactively.
      -v and --verbose show static diagnostic milestones; --trace creates one exact file and --trace-dir generates one run-specific file.
      Trace targets are mutually exclusive; '-' is unsupported because trace data never shares a human or capability stream.

  ` + commandmeta.Name + ` resume list -o <directory>
      List destination-owned incomplete, resumable, cleanup-pending, and attention state.
      Locally ambiguous children are reported as item-blocked.

  ` + commandmeta.Name + ` resume discard -o <directory> --item <N>
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
	commandEventCapacity int

	receiverPeerFactory func() (receiverPeerStarter, error)
	receiverClock       receiverAdmissionClock
	processTrace        *processTrace
	getOutputFactory    getOutputAuthorityFactory
}

// Main 是 os 进程入口的接线:真实标准流 + SIGINT 取消(Ctrl-C 即"停止分享"
// /"中断下载"语义,§6.9)。
func Main() int {
	app := &App{
		Stdout: os.Stdout, Stderr: os.Stderr, Stdin: os.Stdin,
	}
	trace, err := newProcessTrace(os.LookupEnv)
	if err != nil {
		app.writeCompleteLine("%v", err)
		app.closeTerminalOutput()
		return ExitFailure
	}
	interrupts := make(chan os.Signal, interruptSignalBuffer)
	signal.Notify(interrupts, os.Interrupt)
	defer signal.Stop(interrupts)
	app.processTrace = trace
	code := runCLIWithInterruptEscalation(
		interrupts,
		os.Exit,
		func(ctx context.Context) int { return app.Run(ctx, os.Args[1:]) },
	)
	if err := trace.close(); err != nil {
		app.writeCompleteLine("%s: publish test trace: %v", commandmeta.Name, err)
		if code == ExitOK {
			code = ExitFailure
		}
	}
	app.closeTerminalOutput()
	return code
}

// Run 分派子命令。stdlib flag 不认子命令,这里手工分派(§6.9 工程要求:
// 不引 CLI 框架)。
func (a *App) Run(ctx context.Context, args []string) int {
	if len(args) == 0 {
		a.usage()
		return ExitUsage
	}
	switch args[0] {
	case "share":
		return a.runShare(ctx, args[1:])
	case "get":
		return a.runGet(ctx, args[1:])
	case "resume":
		return a.runResume(ctx, args[1:])
	case "help", "-h", "--help":
		a.usage()
		return ExitOK
	default:
		// A command token is untrusted input and can itself be a capability or
		// private credential accidentally pasted in the wrong position.
		a.writeCompleteLine("%s: unknown command", commandmeta.Name)
		a.usage()
		return ExitUsage
	}
}

func (a *App) usage() {
	fmt.Fprint(a.stderrWriter(), usageText)
}

// writeCompleteLine is reserved for command parsing, prompts, resume, and
// private-test diagnostics that occur outside a command event runtime.
func (a *App) writeCompleteLine(format string, args ...any) {
	message := fmt.Sprintf(format, args...)
	_, _ = fmt.Fprintln(a.stderrWriter(), message)
}

// stderrWriter gives top-level, prompt, resume, and private-trace diagnostics a
// complete-line adapter so TerminalCanvas remains the only stderr writer and
// can coordinate each insertion with an active command progress row.
func (a *App) stderrWriter() io.Writer {
	return completeLineCanvasWriter{canvas: a.terminalOutput().canvas}
}

type requestParseOutcome uint8

const (
	requestParseReady requestParseOutcome = iota
	requestParseHelp
	requestParseUsageFailure
	requestParseInternalFailure
)

func (outcome requestParseOutcome) exitCode() int {
	switch outcome {
	case requestParseReady, requestParseHelp:
		return ExitOK
	case requestParseUsageFailure:
		return ExitUsage
	case requestParseInternalFailure:
		return ExitFailure
	default:
		return ExitFailure
	}
}

type flagParseOutcome uint8

const (
	flagParseReady flagParseOutcome = iota
	flagParseHelp
	flagParseUnknownOption
	flagParseMissingOptionValue
	flagParseInvalidOptionValue
	flagParseInvalidOptionSyntax
	flagParseInternalFailure
)

func flagParseDiagnostic(outcome flagParseOutcome) string {
	switch outcome {
	case flagParseUnknownOption:
		return "unknown option"
	case flagParseMissingOptionValue:
		return "option value is required"
	case flagParseInvalidOptionValue:
		return "option value is invalid"
	case flagParseInvalidOptionSyntax:
		return "option syntax is invalid"
	default:
		return "command arguments could not be parsed"
	}
}

// newFlagSet gives the standard parser a discard-only sink because its errors
// quote rejected values. Human output is produced later from the closed
// flagParseOutcome classification, never from error text.
func (a *App) newFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Usage = func() {}
	return fs
}

// writeFlagHelp renders only registration-time metadata. PrintDefaults uses
// DefValue rather than the parsed Value, so help after a positional capability
// cannot reflect that capability back to the terminal.
func (a *App) writeFlagHelp(fs *flag.FlagSet, synopsis string) {
	var help strings.Builder
	fmt.Fprintf(&help, "Usage: %s %s\n\nOptions:\n", commandmeta.Name, synopsis)
	fs.SetOutput(&help)
	fs.PrintDefaults()
	fs.SetOutput(io.Discard)
	_, _ = io.WriteString(a.stderrWriter(), help.String())
}

func (a *App) projectFlagParse(
	command string,
	fs *flag.FlagSet,
	synopsis string,
	outcome flagParseOutcome,
) requestParseOutcome {
	switch outcome {
	case flagParseReady:
		return requestParseReady
	case flagParseHelp:
		a.writeFlagHelp(fs, synopsis)
		return requestParseHelp
	case flagParseInternalFailure:
		a.writeCompleteLine("%s: command arguments could not be parsed", command)
		return requestParseInternalFailure
	default:
		a.writeCompleteLine("%s: %s", command, flagParseDiagnostic(outcome))
		return requestParseUsageFailure
	}
}

// parseInterleaved preserves the flag package's individual flag semantics
// while treating positionals as collectable between options. Parsing one
// occurrence at a time lets the boundary classify a failure from option
// structure without examining the standard library's value-bearing error.
func parseInterleaved(fs *flag.FlagSet, args []string) ([]string, flagParseOutcome) {
	if fs == nil {
		return nil, flagParseInternalFailure
	}
	positionals := make([]string, 0, len(args))
	for len(args) > 0 {
		if args[0] == "--" {
			return append(positionals, args[1:]...), flagParseReady
		}
		occurrence := inspectFlagOccurrence(fs, args)
		if occurrence.positional {
			positionals = append(positionals, args[0])
			args = args[1:]
			continue
		}
		err := fs.Parse(args[:occurrence.width])
		if errors.Is(err, flag.ErrHelp) {
			return nil, flagParseHelp
		}
		if err != nil {
			return nil, occurrence.failure
		}
		args = args[occurrence.width:]
	}
	return positionals, flagParseReady
}

type flagOccurrence struct {
	width      int
	failure    flagParseOutcome
	positional bool
}

type boolFlag interface {
	IsBoolFlag() bool
}

func inspectFlagOccurrence(fs *flag.FlagSet, args []string) flagOccurrence {
	argument := args[0]
	if len(argument) < 2 || argument[0] != '-' || argument == "-" {
		return flagOccurrence{width: 1, positional: true}
	}
	numMinuses := 1
	if argument[1] == '-' {
		numMinuses++
		if len(argument) == numMinuses {
			return flagOccurrence{width: 1, positional: true}
		}
	}
	nameAndValue := argument[numMinuses:]
	if nameAndValue == "" || nameAndValue[0] == '-' || nameAndValue[0] == '=' {
		return flagOccurrence{width: 1, failure: flagParseInvalidOptionSyntax}
	}
	name, _, hasValue := strings.Cut(nameAndValue, "=")
	registered := fs.Lookup(name)
	if registered == nil {
		return flagOccurrence{width: 1, failure: flagParseUnknownOption}
	}
	if candidate, ok := registered.Value.(boolFlag); ok && candidate.IsBoolFlag() {
		return flagOccurrence{width: 1, failure: flagParseInvalidOptionValue}
	}
	if hasValue {
		return flagOccurrence{width: 1, failure: flagParseInvalidOptionValue}
	}
	if len(args) == 1 {
		return flagOccurrence{width: 1, failure: flagParseMissingOptionValue}
	}
	return flagOccurrence{width: 2, failure: flagParseInvalidOptionValue}
}

// repeatedFlag 收集可重复 flag(--only a --only b)。
type repeatedFlag []string

func (f *repeatedFlag) String() string { return strings.Join(*f, ",") }

func (f *repeatedFlag) Set(v string) error {
	*f = append(*f, v)
	return nil
}
