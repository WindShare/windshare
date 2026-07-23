// Package cli 承载 windshare 的子命令逻辑(执行计划 §6.9)。与 main 分离:
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
	"strings"
	"sync"
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

// 部署期默认值:M1 无注册域名,一律指向本机开发环境(§6.3"前端域为
// 部署期配置")。
const (
	// DefaultRelayURL 与 wsrelay 的默认监听端口(:8484)对齐。
	DefaultRelayURL = "ws://127.0.0.1:8484"
	// DefaultFrontURL 是链接的前端基址占位(Vite 开发服务器默认端口)。
	DefaultFrontURL = "http://localhost:5173"
)

// App 是一次 CLI 调用的执行环境;字段即全部外部依赖。
type App struct {
	Stdout io.Writer
	Stderr io.Writer
	Stdin  io.Reader

	stderrMu            sync.Mutex
	receiverPeerFactory func() (receiverPeerStarter, error)
	receiverClock       receiverAdmissionClock
}

// Main 是 os 进程入口的接线:真实标准流 + SIGINT 取消(Ctrl-C 即"停止分享"
// /"中断下载"语义,§6.9)。
func Main() int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	app := &App{Stdout: os.Stdout, Stderr: os.Stderr, Stdin: os.Stdin}
	return app.Run(ctx, os.Args[1:])
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
	case "help", "-h", "--help":
		a.usage()
		return ExitOK
	default:
		fmt.Fprintf(a.stderrWriter(), "windshare: unknown command %q\n", args[0])
		a.usage()
		return ExitUsage
	}
}

func (a *App) usage() {
	fmt.Fprint(a.stderrWriter(), `Usage:
	  windshare share <path...> [--relay <url>] [--block-size <bytes>] [--split-key] [--front-url <url>]
	      Commit selected roots, wait for relay registration, print a suite-02 link, and scan descendants on demand.
	      --split-key prints a bare link and key string for delivery over separate channels.

	  windshare get <link> [-o <directory>] [--only <path>]... [--key <key-string>]
	      Authenticate the descriptor, browse the progressive catalog, and publish files through a durable output session.
	      If the link has no key, use --key or enter the key interactively.
`)
}

// logf 输出运行状态到 stderr(stdout 只留给链接等机器可读产物)。
func (a *App) logf(format string, args ...any) {
	fmt.Fprintf(a.stderrWriter(), format+"\n", args...)
}

type synchronizedStderr struct{ app *App }

func (w synchronizedStderr) Write(p []byte) (int, error) {
	w.app.stderrMu.Lock()
	defer w.app.stderrMu.Unlock()
	return w.app.Stderr.Write(p)
}

// stderrWriter gives progress, relay callbacks, and orchestration diagnostics
// one output serialization boundary. Those producers are intentionally
// concurrent even though bytes.Buffer-based tests and many io.Writer values are
// not safe for concurrent writes.
func (a *App) stderrWriter() io.Writer { return synchronizedStderr{app: a} }

// newFlagSet 统一 flag 行为:错误不退出进程(返回给调用方定退出码),
// 用法输出走注入的 stderr。
func (a *App) newFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(a.stderrWriter())
	return fs
}

// parseInterleaved 支持 §6.9 的「位置参数在前、flag 在后」书写:stdlib flag
// 遇首个非 flag 参数即停止解析,这里循环续解,位置参数按原相对顺序收集。
func parseInterleaved(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		rest := fs.Args()
		if len(rest) == 0 {
			return pos, nil
		}
		pos = append(pos, rest[0])
		args = rest[1:]
	}
}

// repeatedFlag 收集可重复 flag(--only a --only b)。
type repeatedFlag []string

func (f *repeatedFlag) String() string { return strings.Join(*f, ",") }

func (f *repeatedFlag) Set(v string) error {
	*f = append(*f, v)
	return nil
}
