// These tests drive a deterministic whole-repo SSA static-analysis gate with
// no concurrency of its own: race instrumentation adds ~4x runtime and zero
// signal, so race builds skip them. The non-race coverage gates remain the
// authoritative execution.
//go:build !race

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSemanticRuntimeBoundary(t *testing.T) {
	// Each fixture owns a complete SSA dependency graph. Serial loading keeps
	// peak memory bounded instead of multiplying that graph by GOMAXPROCS.
	tests := []struct {
		name       string
		source     string
		production string
		wantError  bool
		wantDetail string
	}{
		{
			name: "dominating real gate",
			source: `package owner
import (
    "net"
    "testing"
    "github.com/windshare/windshare/internal/testnetwork"
)
func open(t *testing.T) {
    testnetwork.RequireOSNetwork(t)
    _, _ = net.Listen("tcp", "127.0.0.1:0")
}`,
		},
		{
			name: "compiler resolved gate alias",
			source: `package owner
import (
    "net"
    "github.com/windshare/windshare/internal/testnetwork"
)
func open() {
    gate := testnetwork.AssertOSNetwork
    gate()
    _, _ = net.Listen("tcp", "127.0.0.1:0")
}`,
		},
		{
			name:      "ambiguous gate value fails closed",
			wantError: true,
			source: `package owner
import (
    "net"
    "github.com/windshare/windshare/internal/testnetwork"
)
func inert() {}
func open(skipGate bool) {
    gate := testnetwork.AssertOSNetwork
    if skipGate { gate = inert }
    gate()
    _, _ = net.Listen("tcp", "127.0.0.1:0")
}`,
		},
		{
			name:       "gated direct primitive does not bless higher order sibling",
			wantError:  true,
			wantDetail: "invokeListen",
			source: `package owner
import (
    "net"
    "testing"
    "github.com/windshare/windshare/internal/testnetwork"
)
func safelyClassified(t *testing.T) {
    testnetwork.RequireOSNetwork(t)
    listener, _ := net.Listen("tcp", "127.0.0.1:0")
    if listener != nil { _ = listener.Close() }
}
func invokeListen(open func(string, string) (net.Listener, error)) {
    listener, _ := open("tcp", "127.0.0.1:0")
    if listener != nil { _ = listener.Close() }
}
func escapedThroughFunctionValue() { invokeListen(net.Listen) }`,
		},
		{
			name: "gated higher order wrapper",
			source: `package owner
import (
    "net"
    "github.com/windshare/windshare/internal/testnetwork"
)
func invokeListen(open func(string, string) (net.Listener, error)) {
    testnetwork.AssertOSNetwork()
    listener, _ := open("tcp", "127.0.0.1:0")
    if listener != nil { _ = listener.Close() }
}
func safelyWrapped() { invokeListen(net.Listen) }`,
		},
		{
			name:      "multiple indirections through containers and parameters",
			wantError: true,
			source: `package owner
import "net"
type openFn func(string, string) (net.Listener, error)
type holder struct { open openFn }
var registry = map[string][]holder{"tcp": {{open: net.Listen}}}
func invoke(open openFn) { _, _ = open("tcp", "127.0.0.1:0") }
func forward(value holder) { invoke(value.open) }
func escapedThroughContainers() { forward(registry["tcp"][0]) }`,
		},
		{
			name:      "phi keeps every possible function target",
			wantError: true,
			source: `package owner
import "net"
type openFn func(string, string) (net.Listener, error)
func inert(string, string) (net.Listener, error) { return nil, nil }
func choose(network bool) openFn {
    selected := openFn(net.Listen)
    if !network { selected = inert }
    return selected
}
func escapedThroughPhi(network bool) { _, _ = choose(network)("tcp", "127.0.0.1:0") }`,
		},
		{
			name:      "method value through slice",
			wantError: true,
			source: `package owner
import (
    "context"
    "net"
)
type listenMethod func(context.Context, string, string) (net.Listener, error)
var config net.ListenConfig
var methods = []listenMethod{config.Listen}
func escapedThroughMethodValue() {
    _, _ = methods[0](context.Background(), "tcp", "127.0.0.1:0")
}`,
		},
		{
			name:      "interface dispatch to transparent production target",
			wantError: true,
			production: `package owner
import "net"
type opener interface { Open() }
type socketOpener struct{}
func (socketOpener) Open() { listener, _ := net.Listen("tcp", "127.0.0.1:0"); if listener != nil { _ = listener.Close() } }
func Invoke(open opener) { open.Open() }`,
			source: `package owner
func escapedThroughInterface() { Invoke(socketOpener{}) }`,
		},
		{
			name:      "closure captures resource function",
			wantError: true,
			source: `package owner
import "net"
func makeOpen() func() {
    open := net.Listen
    return func() { _, _ = open("tcp", "127.0.0.1:0") }
}
func escapedThroughClosure() { makeOpen()() }`,
		},
		{
			name:      "caller gate cannot bless captured closure owner",
			wantError: true,
			source: `package owner
import (
    "net"
    "github.com/windshare/windshare/internal/testnetwork"
)
func escapedThroughClosure() {
    testnetwork.AssertOSNetwork()
    open := func() { _, _ = net.Listen("tcp", "127.0.0.1:0") }
    open()
}`,
		},
		{
			name:       "unresolved dynamic target fails closed",
			wantError:  true,
			wantDetail: "unresolved dynamic call target",
			source: `package owner
type token struct{}
type result struct{}
func invokeUnknown(open func(token) result) { _ = open(token{}) }`,
		},
		{
			name: "gated unresolved dynamic target",
			source: `package owner
import "github.com/windshare/windshare/internal/testnetwork"
type token struct{}
type result struct{}
func invokeUnknown(open func(token) result) {
    testnetwork.AssertOSNetwork()
    _ = open(token{})
}`,
		},
		{
			name:       "safe production entry cannot bless unsafe sibling",
			wantError:  true,
			wantDetail: "unsafeSibling",
			production: `package owner
import "net"
type Open func()
func Socket() { listener, _ := net.Listen("tcp", "127.0.0.1:0"); if listener != nil { _ = listener.Close() } }
func Invoke(open Open) { open() }`,
			source: `package owner
import "github.com/windshare/windshare/internal/testnetwork"
func safeEntry() { testnetwork.AssertOSNetwork(); Invoke(Socket) }
func unsafeSibling() { Invoke(Socket) }`,
		},
		{
			name:       "compiler registered unsafe entry remains independent",
			wantError:  true,
			wantDetail: "TestUnsafe",
			production: `package owner
import "net"
type Open func()
func Socket() { listener, _ := net.Listen("tcp", "127.0.0.1:0"); if listener != nil { _ = listener.Close() } }
func Invoke(open Open) { open() }`,
			source: `package owner
import (
    "testing"
    "github.com/windshare/windshare/internal/testnetwork"
)
func TestSafe(t *testing.T) { testnetwork.AssertOSNetwork(); TestUnsafe(t) }
func TestUnsafe(*testing.T) { Invoke(Socket) }`,
		},
		{
			name: "all transparent production entries gated",
			production: `package owner
import "net"
type Open func()
func Socket() { listener, _ := net.Listen("tcp", "127.0.0.1:0"); if listener != nil { _ = listener.Close() } }
func Invoke(open Open) { open() }`,
			source: `package owner
import "github.com/windshare/windshare/internal/testnetwork"
func firstEntry() { testnetwork.AssertOSNetwork(); Invoke(Socket) }
func secondEntry() { testnetwork.AssertOSNetwork(); Invoke(Socket) }`,
		},
		{
			name:      "aliased function primitive",
			wantError: true,
			source: `package owner
import n "net"
func open() { _, _ = n.Listen("tcp", "127.0.0.1:0") }`,
		},
		{
			name:      "method primitive",
			wantError: true,
			source: `package owner
import (
    "context"
    "net"
)
func open() {
    var config net.ListenConfig
    _, _ = config.Listen(context.Background(), "tcp", "127.0.0.1:0")
}`,
		},
		{
			name:      "unreachable gate",
			wantError: true,
			source: `package owner
import (
    "net"
    "github.com/windshare/windshare/internal/testnetwork"
)
func open() {
    if false { testnetwork.AssertOSNetwork() }
    _, _ = net.Listen("tcp", "127.0.0.1:0")
}`,
		},
		{
			name:      "deferred gate executes too late",
			wantError: true,
			source: `package owner
import (
    "net"
    "github.com/windshare/windshare/internal/testnetwork"
)
func open() {
    defer testnetwork.AssertOSNetwork()
    _, _ = net.Listen("tcp", "127.0.0.1:0")
}`,
		},
		{
			name:      "goroutine gate is not ordered",
			wantError: true,
			source: `package owner
import (
    "net"
    "github.com/windshare/windshare/internal/testnetwork"
)
func open() {
    go testnetwork.AssertOSNetwork()
    _, _ = net.Listen("tcp", "127.0.0.1:0")
}`,
		},
		{
			name:      "disjoint branch gate",
			wantError: true,
			source: `package owner
import (
    "net"
    "github.com/windshare/windshare/internal/testnetwork"
)
func open(guard bool) {
    if guard { testnetwork.AssertOSNetwork() } else { _, _ = net.Listen("tcp", "127.0.0.1:0") }
}`,
		},
		{
			name: "every branch establishes the gate",
			source: `package owner
import (
    "net"
    "github.com/windshare/windshare/internal/testnetwork"
)
func open(first bool) {
    if first { testnetwork.AssertOSNetwork() } else { testnetwork.AssertOSNetwork() }
    _, _ = net.Listen("tcp", "127.0.0.1:0")
}`,
		},
		{
			name:      "caller-only gate",
			wantError: true,
			source: `package owner
import (
    "net"
    "github.com/windshare/windshare/internal/testnetwork"
)
func helper() { _, _ = net.Listen("tcp", "127.0.0.1:0") }
func caller() { testnetwork.AssertOSNetwork(); helper() }`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := writeFixtureModule(t, map[string]string{
				"owner/owner.go":      test.production,
				"owner/owner_test.go": test.source,
			})
			result, err := analyzeRoot(root)
			if err != nil {
				t.Fatalf("analyze fixture: %v", err)
			}
			gotError := len(result.violations) != 0
			if gotError != test.wantError {
				t.Fatalf("violations = %v, want error %v", result.violations, test.wantError)
			}
			if test.wantDetail != "" && !strings.Contains(strings.Join(result.violations, "\n"), test.wantDetail) {
				t.Fatalf("violations = %v, want detail %q", result.violations, test.wantDetail)
			}
			if !result.classified["owner"] {
				t.Fatalf("classified packages = %v, want owner", result.classified)
			}
		})
	}
}

func TestTransparentValueFlowTerminalSemantics(t *testing.T) {
	tests := []struct {
		name       string
		production string
		source     string
		wantError  bool
		wantDetail string
	}{
		{
			name:       "invoke then panic remains resource bearing",
			wantError:  true,
			wantDetail: "InvokeThenPanic",
			production: `package owner
import "net"
type Open func()
func Socket() { listener, _ := net.Listen("tcp", "127.0.0.1:0"); if listener != nil { _ = listener.Close() } }
func InvokeThenPanic(open Open) { open(); panic("after resource effect") }`,
			source: `package owner
func unsafeNoReturn() { InvokeThenPanic(Socket) }`,
		},
		{
			name: "dominating gate authorizes invoke then panic",
			production: `package owner
import "net"
type Open func()
func Socket() { listener, _ := net.Listen("tcp", "127.0.0.1:0"); if listener != nil { _ = listener.Close() } }
func InvokeThenPanic(open Open) { open(); panic("after resource effect") }`,
			source: `package owner
import "github.com/windshare/windshare/internal/testnetwork"
func gatedNoReturn() { testnetwork.AssertOSNetwork(); InvokeThenPanic(Socket) }`,
		},
		{
			name:       "invoke then process exit remains resource bearing",
			wantError:  true,
			wantDetail: "InvokeThenExit",
			production: `package owner
import (
    "net"
    "os"
)
type Open func()
func Socket() { listener, _ := net.Listen("tcp", "127.0.0.1:0"); if listener != nil { _ = listener.Close() } }
func InvokeThenExit(open Open) { open(); os.Exit(2) }`,
			source: `package owner
func unsafeProcessExit() { InvokeThenExit(Socket) }`,
		},
		{
			name:       "invoke then goroutine exit remains resource bearing",
			wantError:  true,
			wantDetail: "InvokeThenGoexit",
			production: `package owner
import (
    "net"
    "runtime"
)
type Open func()
func Socket() { listener, _ := net.Listen("tcp", "127.0.0.1:0"); if listener != nil { _ = listener.Close() } }
func InvokeThenGoexit(open Open) { open(); runtime.Goexit() }`,
			source: `package owner
func unsafeGoexit() { InvokeThenGoexit(Socket) }`,
		},
		{
			name:       "invoke then no return wrapper remains resource bearing",
			wantError:  true,
			wantDetail: "InvokeThenStop",
			production: `package owner
import (
    "net"
    "os"
)
type Open func()
func Socket() { listener, _ := net.Listen("tcp", "127.0.0.1:0"); if listener != nil { _ = listener.Close() } }
func stopProcess() { os.Exit(2) }
func InvokeThenStop(open Open) { open(); stopProcess() }`,
			source: `package owner
func unsafeNoReturnWrapper() { InvokeThenStop(Socket) }`,
		},
		{
			name: "process exit before effect is not transparent",
			production: `package owner
import (
    "net"
    "os"
)
type Open func()
func Socket() { listener, _ := net.Listen("tcp", "127.0.0.1:0"); if listener != nil { _ = listener.Close() } }
func ExitBeforeInvoke(open Open) { os.Exit(2); open() }`,
			source: `package owner
import (
    "net"
    "github.com/windshare/windshare/internal/testnetwork"
)
func classified() { testnetwork.AssertOSNetwork(); _, _ = net.Listen("tcp", "127.0.0.1:0") }
func safeProcessExit() { ExitBeforeInvoke(Socket) }`,
		},
		{
			name: "goroutine exit before effect is not transparent",
			production: `package owner
import (
    "net"
    "runtime"
)
type Open func()
func Socket() { listener, _ := net.Listen("tcp", "127.0.0.1:0"); if listener != nil { _ = listener.Close() } }
func GoexitBeforeInvoke(open Open) { runtime.Goexit(); open() }`,
			source: `package owner
import (
    "net"
    "github.com/windshare/windshare/internal/testnetwork"
)
func classified() { testnetwork.AssertOSNetwork(); _, _ = net.Listen("tcp", "127.0.0.1:0") }
func safeGoexit() { GoexitBeforeInvoke(Socket) }`,
		},
		{
			name: "no return wrapper before effect is not transparent",
			production: `package owner
import (
    "net"
    "os"
)
type Open func()
func Socket() { listener, _ := net.Listen("tcp", "127.0.0.1:0"); if listener != nil { _ = listener.Close() } }
func stopProcess() { os.Exit(2) }
func StopBeforeInvoke(open Open) { stopProcess(); open() }`,
			source: `package owner
import (
    "net"
    "github.com/windshare/windshare/internal/testnetwork"
)
func classified() { testnetwork.AssertOSNetwork(); _, _ = net.Listen("tcp", "127.0.0.1:0") }
func safeNoReturnWrapper() { StopBeforeInvoke(Socket) }`,
		},
		{
			name:       "conditional panic after effect remains resource bearing",
			wantError:  true,
			wantDetail: "InvokeThenMaybePanic",
			production: `package owner
import "net"
type Open func()
func Socket() { listener, _ := net.Listen("tcp", "127.0.0.1:0"); if listener != nil { _ = listener.Close() } }
func InvokeThenMaybePanic(open Open, stop bool) { open(); if stop { panic("after resource effect") } }`,
			source: `package owner
func unsafeConditionalPanic(stop bool) { InvokeThenMaybePanic(Socket, stop) }`,
		},
		{
			name:       "infinite branch after effect remains resource bearing",
			wantError:  true,
			wantDetail: "InvokeThenMaybeLoop",
			production: `package owner
import "net"
type Open func()
func Socket() { listener, _ := net.Listen("tcp", "127.0.0.1:0"); if listener != nil { _ = listener.Close() } }
func InvokeThenMaybeLoop(open Open, spin bool) { open(); if spin { for {} } }`,
			source: `package owner
func unsafeConditionalLoop(spin bool) { InvokeThenMaybeLoop(Socket, spin) }`,
		},
		{
			name:       "dead end after effect remains resource bearing",
			wantError:  true,
			wantDetail: "InvokeThenDeadEnd",
			production: `package owner
import "net"
type Open func()
func Socket() { listener, _ := net.Listen("tcp", "127.0.0.1:0"); if listener != nil { _ = listener.Close() } }
func InvokeThenDeadEnd(open Open) { open(); select {} }`,
			source: `package owner
func unsafeDeadEnd() { InvokeThenDeadEnd(Socket) }`,
		},
		{
			name: "infinite branch before effect is not transparent",
			production: `package owner
import "net"
type Open func()
func Socket() { listener, _ := net.Listen("tcp", "127.0.0.1:0"); if listener != nil { _ = listener.Close() } }
func LoopOrInvoke(open Open, spin bool) { if spin { for {} }; open() }`,
			source: `package owner
import (
    "net"
    "github.com/windshare/windshare/internal/testnetwork"
)
func classified() { testnetwork.AssertOSNetwork(); _, _ = net.Listen("tcp", "127.0.0.1:0") }
func boundedLoopBranch() { LoopOrInvoke(Socket, false) }`,
		},
		{
			name: "dead end before effect is not transparent",
			production: `package owner
import "net"
type Open func()
func Socket() { listener, _ := net.Listen("tcp", "127.0.0.1:0"); if listener != nil { _ = listener.Close() } }
func DeadEndOrInvoke(open Open, block bool) { if block { select {} }; open() }`,
			source: `package owner
import (
    "net"
    "github.com/windshare/windshare/internal/testnetwork"
)
func classified() { testnetwork.AssertOSNetwork(); _, _ = net.Listen("tcp", "127.0.0.1:0") }
func boundedDeadEndBranch() { DeadEndOrInvoke(Socket, false) }`,
		},
		{
			name:       "recovered panic after effect remains resource bearing",
			wantError:  true,
			wantDetail: "InvokeThenRecover",
			production: `package owner
import "net"
type Open func()
func Socket() { listener, _ := net.Listen("tcp", "127.0.0.1:0"); if listener != nil { _ = listener.Close() } }
func InvokeThenRecover(open Open) {
    defer func() { _ = recover() }()
    open()
    panic("after resource effect")
}`,
			source: `package owner
func unsafeRecoveredPanic() { InvokeThenRecover(Socket) }`,
		},
		{
			name:       "deferred effect runs across return panic recovery and goexit",
			wantError:  true,
			wantDetail: "DeferAcrossTerminals",
			production: `package owner
import (
    "net"
    "runtime"
)
type Open func()
func Socket() { listener, _ := net.Listen("tcp", "127.0.0.1:0"); if listener != nil { _ = listener.Close() } }
func DeferAcrossTerminals(open Open, mode int) {
    defer open()
    defer func() { _ = recover() }()
    if mode == 0 { return }
    if mode == 1 { panic("run deferred effect") }
    runtime.Goexit()
}`,
			source: `package owner
func unsafeDeferredEffect(mode int) { DeferAcrossTerminals(Socket, mode) }`,
		},
		{
			name: "deferred effect is not scheduled execution",
			production: `package owner
import (
    "net"
    "os"
)
type Open func()
func Socket() { listener, _ := net.Listen("tcp", "127.0.0.1:0"); if listener != nil { _ = listener.Close() } }
func DeferBeforeTerminal(open Open, exit bool) {
    defer open()
    if exit { os.Exit(2) }
    for {}
}
func LaterDeferStopsEffect(open Open) { defer open(); defer os.Exit(2) }`,
			source: `package owner
import (
    "net"
    "github.com/windshare/windshare/internal/testnetwork"
)
func classified() { testnetwork.AssertOSNetwork(); _, _ = net.Listen("tcp", "127.0.0.1:0") }
func safeDeferredTerminal(exit bool) { DeferBeforeTerminal(Socket, exit) }
func safeDeferredOrder() { LaterDeferStopsEffect(Socket) }`,
		},
		{
			name: "recovered panic before effect is not transparent",
			production: `package owner
import "net"
type Open func()
func Socket() { listener, _ := net.Listen("tcp", "127.0.0.1:0"); if listener != nil { _ = listener.Close() } }
func RecoverBeforeInvoke(open Open) {
    defer func() { _ = recover() }()
    panic("before resource effect")
    open()
}`,
			source: `package owner
import (
    "net"
    "github.com/windshare/windshare/internal/testnetwork"
)
func classified() { testnetwork.AssertOSNetwork(); _, _ = net.Listen("tcp", "127.0.0.1:0") }
func safeRecoveredPanic() { RecoverBeforeInvoke(Socket) }`,
		},
		{
			name: "panic before effect is not transparent",
			production: `package owner
import "net"
type Open func()
func Socket() { listener, _ := net.Listen("tcp", "127.0.0.1:0"); if listener != nil { _ = listener.Close() } }
func PanicBeforeInvoke(open Open) { panic("before resource effect"); open() }`,
			source: `package owner
import (
    "net"
    "github.com/windshare/windshare/internal/testnetwork"
)
func classified() { testnetwork.AssertOSNetwork(); _, _ = net.Listen("tcp", "127.0.0.1:0") }
func safePanic() { PanicBeforeInvoke(Socket) }`,
		},
		{
			name:       "gated entry cannot bless no return sibling",
			wantError:  true,
			wantDetail: "TestUnsafeNoReturn",
			production: `package owner
import "net"
type Open func()
func Socket() { listener, _ := net.Listen("tcp", "127.0.0.1:0"); if listener != nil { _ = listener.Close() } }
func InvokeThenPanic(open Open) { open(); panic("after resource effect") }`,
			source: `package owner
import (
    "testing"
    "github.com/windshare/windshare/internal/testnetwork"
)
func TestSafeNoReturn(t *testing.T) { testnetwork.AssertOSNetwork(); TestUnsafeNoReturn(t) }
func TestUnsafeNoReturn(*testing.T) { InvokeThenPanic(Socket) }`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := writeFixtureModule(t, map[string]string{
				"owner/owner.go":      test.production,
				"owner/owner_test.go": test.source,
			})
			result, err := analyzeRoot(root)
			if err != nil {
				t.Fatalf("analyze fixture: %v", err)
			}
			gotError := len(result.violations) != 0
			if gotError != test.wantError {
				t.Fatalf("violations = %v, want error %v", result.violations, test.wantError)
			}
			if test.wantDetail != "" && !strings.Contains(strings.Join(result.violations, "\n"), test.wantDetail) {
				t.Fatalf("violations = %v, want detail %q", result.violations, test.wantDetail)
			}
			if !result.classified["owner"] {
				t.Fatalf("classified packages = %v, want owner", result.classified)
			}
		})
	}
}

func TestRecoverOutcomeTransformers(t *testing.T) {
	root := writeFixtureModule(t, map[string]string{
		"owner/owner.go": `package owner
import (
    "net"
    "runtime"
)
type Open func()
type Recovery func()
func Socket() { listener, _ := net.Listen("tcp", "127.0.0.1:0"); if listener != nil { _ = listener.Close() } }
func directRecover() { _ = recover() }
func noRecover() {}
func ownPanicRecovered() { defer directRecover(); panic("own panic") }
func derivedOwnPanicRecovered() { ownPanicRecovered() }
func conditionalRecovery(enabled bool) {
    defer func() { if enabled { _ = recover() } }()
    panic("conditionally recovered")
}
func chooseRecovery(enabled bool) Recovery {
    if enabled { return directRecover }
    return noRecover
}
func ambiguousRecoveryTarget(enabled bool) {
    defer chooseRecovery(enabled)()
    panic("ambiguously recovered")
}
func panicWithoutRecovery() { defer noRecover(); panic("not recovered") }
func nestedRecoverHelper() { _ = recover() }
func nestedHelperRecovery() {
    defer func() { nestedRecoverHelper() }()
    panic("nested helper cannot recover")
}
var recoverClosure = func() { _ = recover() }
func normallyInvokedRecoverClosure() {
    defer func() { recoverClosure() }()
    panic("ordinary closure call cannot recover")
}
func recoverThenRepanic() {
    defer func() { _ = recover(); panic("replacement panic") }()
    panic("original panic")
}
func panicRecoveredByOlderDefer() {
    defer directRecover()
    defer func() { panic("newer panic") }()
}
func recoverRunsBeforeOlderPanic() {
    defer func() { panic("older panic") }()
    defer directRecover()
}
func recoveredNilPanic() { defer directRecover(); panic(nil) }
func recoverCannotStopGoexit() { defer directRecover(); runtime.Goexit() }
func recoveredTransientPanicCannotStopGoexit() {
    defer directRecover()
    defer func() { panic("panic during Goexit") }()
    runtime.Goexit()
}
func EffectAfterOwnRecovery(open Open) { ownPanicRecovered(); open() }
func EffectAfterDerivedRecovery(open Open) { derivedOwnPanicRecovered(); open() }
func EffectAfterConditionalRecovery(open Open, enabled bool) { conditionalRecovery(enabled); open() }
func EffectAfterAmbiguousRecovery(open Open, enabled bool) { ambiguousRecoveryTarget(enabled); open() }
func EffectAfterNoRecovery(open Open) { panicWithoutRecovery(); open() }
func EffectAfterNestedHelperRecovery(open Open) { nestedHelperRecovery(); open() }
func EffectAfterNormalRecoverClosure(open Open) { normallyInvokedRecoverClosure(); open() }
func EffectAfterRecoverRepanic(open Open) { recoverThenRepanic(); open() }
func EffectAfterLIFORecovery(open Open) { panicRecoveredByOlderDefer(); open() }
func EffectAfterReverseLIFO(open Open) { recoverRunsBeforeOlderPanic(); open() }
func EffectAfterNilRecovery(open Open) { recoveredNilPanic(); open() }
func EffectAfterGoexit(open Open) { recoverCannotStopGoexit(); open() }
func EffectAfterRecoveredGoexitPanic(open Open) { recoveredTransientPanicCannotStopGoexit(); open() }
func EffectBeforeRepanic(open Open) { open(); recoverThenRepanic() }
func EffectBeforeGoexit(open Open) { open(); recoverCannotStopGoexit() }`,
		"owner/owner_test.go": `package owner
func unsafeAfterOwnRecovery() { EffectAfterOwnRecovery(Socket) }
func unsafeAfterDerivedRecovery() { EffectAfterDerivedRecovery(Socket) }
func unsafeAfterLIFORecovery() { EffectAfterLIFORecovery(Socket) }
func unsafeAfterNilRecovery() { EffectAfterNilRecovery(Socket) }
func unsafeEffectBeforeRepanic() { EffectBeforeRepanic(Socket) }
func unsafeEffectBeforeGoexit() { EffectBeforeGoexit(Socket) }
func safeAfterConditionalRecovery(enabled bool) { EffectAfterConditionalRecovery(Socket, enabled) }
func safeAfterAmbiguousRecovery(enabled bool) { EffectAfterAmbiguousRecovery(Socket, enabled) }
func safeAfterNoRecovery() { EffectAfterNoRecovery(Socket) }
func safeAfterNestedHelperRecovery() { EffectAfterNestedHelperRecovery(Socket) }
func safeAfterNormalRecoverClosure() { EffectAfterNormalRecoverClosure(Socket) }
func safeAfterRecoverRepanic() { EffectAfterRecoverRepanic(Socket) }
func safeAfterReverseLIFO() { EffectAfterReverseLIFO(Socket) }
func safeAfterGoexit() { EffectAfterGoexit(Socket) }
func safeAfterRecoveredGoexitPanic() { EffectAfterRecoveredGoexitPanic(Socket) }`,
	})
	result, err := analyzeRoot(root)
	if err != nil {
		t.Fatalf("analyze fixture: %v", err)
	}
	details := strings.Join(result.violations, "\n")
	for _, name := range []string{
		"unsafeAfterOwnRecovery",
		"unsafeAfterDerivedRecovery",
		"unsafeAfterLIFORecovery",
		"unsafeAfterNilRecovery",
		"unsafeEffectBeforeRepanic",
		"unsafeEffectBeforeGoexit",
	} {
		if !strings.Contains(details, name) {
			t.Errorf("violations = %v, want recovered-continuation entry %q", result.violations, name)
		}
	}
	for _, name := range []string{
		"safeAfterConditionalRecovery",
		"safeAfterAmbiguousRecovery",
		"safeAfterNoRecovery",
		"safeAfterNestedHelperRecovery",
		"safeAfterNormalRecoverClosure",
		"safeAfterRecoverRepanic",
		"safeAfterReverseLIFO",
		"safeAfterGoexit",
		"safeAfterRecoveredGoexitPanic",
	} {
		if strings.Contains(details, name) {
			t.Errorf("violations = %v, non-returning recovery lookalike %q must fail closed", result.violations, name)
		}
	}
	if !result.classified["owner"] {
		t.Fatalf("classified packages = %v, want owner", result.classified)
	}
}

func TestDeferredUnwindOrdering(t *testing.T) {
	root := writeFixtureModule(t, map[string]string{
		"owner/owner.go": `package owner
import (
    "net"
    "os"
    "runtime"
)
type Open func()
type Tail func()
func Socket() { listener, _ := net.Listen("tcp", "127.0.0.1:0"); if listener != nil { _ = listener.Close() } }
func panicTail() { panic("unwind continues") }
func derivedPanicTail() { panicTail() }
func goexitTail() { runtime.Goexit() }
func derivedGoexitTail() { goexitTail() }
func exitTail() { os.Exit(2) }
func blockTail() { select {} }
func recoveredPanicTail() {
    defer func() { _ = recover() }()
    panic("recovered nested panic")
}
func recoverRepanicTail() {
    defer func() { _ = recover(); panic("replacement panic") }()
    panic("original panic")
}
func nestedPanicTail() { defer derivedPanicTail() }
func chooseTail(exit bool) Tail {
    if exit { return exitTail }
    return derivedPanicTail
}
func DeferThenDirectPanic(open Open) { defer open(); defer panic("tail panic") }
func DeferThenDerivedPanic(open Open) { defer open(); defer derivedPanicTail() }
func DeferThenDirectGoexit(open Open) { defer open(); defer runtime.Goexit() }
func DeferThenDerivedGoexit(open Open) { defer open(); defer derivedGoexitTail() }
func DeferThenRecover(open Open) {
    defer open()
    defer func() { _ = recover() }()
    panic("recovered outer panic")
}
func DeferThenRecoverRepanic(open Open) { defer open(); defer recoverRepanicTail() }
func DeferThenRecoveredNestedPanic(open Open) { defer open(); defer recoveredPanicTail() }
func DeferThenNestedPanic(open Open) { defer open(); defer nestedPanicTail() }
func DeferThenMixedUnwind(open Open) { defer open(); defer derivedPanicTail(); defer derivedGoexitTail() }
func DeferThenCallDerivedPanic(open Open) { defer open(); derivedPanicTail() }
func DeferThenCallDerivedGoexit(open Open) { defer open(); derivedGoexitTail() }
func DeferThenCallRecoveredPanic(open Open) { defer open(); recoveredPanicTail() }
func ExitThenDefer(open Open) { defer exitTail(); defer open() }
func BlockThenDefer(open Open) { defer blockTail(); defer open() }
func DeferThenExit(open Open) { defer open(); defer exitTail() }
func DeferThenBlock(open Open) { defer open(); defer blockTail() }
func DeferThenCallExit(open Open) { defer open(); exitTail() }
func DeferThenCallBlock(open Open) { defer open(); blockTail() }
func DeferThenAmbiguousTail(open Open, exit bool) { defer open(); defer chooseTail(exit)() }`,
		"owner/owner_test.go": `package owner
func unsafeDirectPanic() { DeferThenDirectPanic(Socket) }
func unsafeDerivedPanic() { DeferThenDerivedPanic(Socket) }
func unsafeDirectGoexit() { DeferThenDirectGoexit(Socket) }
func unsafeDerivedGoexit() { DeferThenDerivedGoexit(Socket) }
func unsafeRecover() { DeferThenRecover(Socket) }
func unsafeRecoverRepanic() { DeferThenRecoverRepanic(Socket) }
func unsafeRecoveredNestedPanic() { DeferThenRecoveredNestedPanic(Socket) }
func unsafeNestedPanic() { DeferThenNestedPanic(Socket) }
func unsafeMixedUnwind() { DeferThenMixedUnwind(Socket) }
func unsafeCallDerivedPanic() { DeferThenCallDerivedPanic(Socket) }
func unsafeCallDerivedGoexit() { DeferThenCallDerivedGoexit(Socket) }
func unsafeCallRecoveredPanic() { DeferThenCallRecoveredPanic(Socket) }
func unsafeEffectBeforeExit() { ExitThenDefer(Socket) }
func unsafeEffectBeforeBlock() { BlockThenDefer(Socket) }
func safeExitBeforeEffect() { DeferThenExit(Socket) }
func safeBlockBeforeEffect() { DeferThenBlock(Socket) }
func safeCallExitBeforeEffect() { DeferThenCallExit(Socket) }
func safeCallBlockBeforeEffect() { DeferThenCallBlock(Socket) }
func safeAmbiguousTail(exit bool) { DeferThenAmbiguousTail(Socket, exit) }`,
	})
	result, err := analyzeRoot(root)
	if err != nil {
		t.Fatalf("analyze fixture: %v", err)
	}
	details := strings.Join(result.violations, "\n")
	for _, name := range []string{
		"unsafeDirectPanic",
		"unsafeDerivedPanic",
		"unsafeDirectGoexit",
		"unsafeDerivedGoexit",
		"unsafeRecover",
		"unsafeRecoverRepanic",
		"unsafeRecoveredNestedPanic",
		"unsafeNestedPanic",
		"unsafeMixedUnwind",
		"unsafeCallDerivedPanic",
		"unsafeCallDerivedGoexit",
		"unsafeCallRecoveredPanic",
		"unsafeEffectBeforeExit",
		"unsafeEffectBeforeBlock",
	} {
		if !strings.Contains(details, name) {
			t.Errorf("violations = %v, want deferred unwind entry %q", result.violations, name)
		}
	}
	for _, name := range []string{
		"safeExitBeforeEffect",
		"safeBlockBeforeEffect",
		"safeCallExitBeforeEffect",
		"safeCallBlockBeforeEffect",
		"safeAmbiguousTail",
	} {
		if strings.Contains(details, name) {
			t.Errorf("violations = %v, suppressing defer entry %q must remain non-transparent", result.violations, name)
		}
	}
	if !result.classified["owner"] {
		t.Fatalf("classified packages = %v, want owner", result.classified)
	}
}

func writeFixtureModule(t *testing.T, fixtureFiles map[string]string) string {
	t.Helper()
	root := t.TempDir()
	files := map[string]string{
		"go.mod": "module github.com/windshare/windshare\n\ngo 1.26.5\n",
		"internal/testnetwork/gate.go": `package testnetwork
func RequireOSNetwork(any) {}
func AssertOSNetwork() {}
`,
	}
	for relative, content := range fixtureFiles {
		if strings.TrimSpace(content) != "" {
			files[relative] = content
		}
	}
	for relative, content := range files {
		path := filepath.Join(root, filepath.FromSlash(relative))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(strings.TrimSpace(content)+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}
