package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"time"
)

const warmupMessages = 8

var variant = "upstream"

type options struct {
	scenario     string
	drops        int
	messages     int
	size         int
	interval     time.Duration
	delay        time.Duration
	timeout      time.Duration
	interleaving bool
	listen       string
	remote       string
	trace        string
}

func main() {
	var o options
	flag.StringVar(&o.scenario, "scenario", "loss", "loss, tail, reorder, ambiguous")
	flag.IntVar(&o.drops, "drops", 2, "consecutive target DATA transmissions to drop")
	flag.IntVar(&o.messages, "messages", 320, "messages after warmup")
	flag.IntVar(&o.size, "size", 256, "bytes per message")
	flag.DurationVar(&o.interval, "interval", 4*time.Millisecond, "spacing of application writes")
	flag.DurationVar(&o.delay, "delay", 10*time.Millisecond, "one-way network delay per endpoint")
	flag.DurationVar(&o.timeout, "timeout", 8*time.Second, "whole experiment deadline")
	flag.BoolVar(&o.interleaving, "interleaving", true, "negotiate I-DATA")
	flag.StringVar(&o.listen, "listen", "", "run the echo receiver at this UDP address")
	flag.StringVar(&o.remote, "remote", "", "use a separate echo receiver instead of local loopback")
	flag.StringVar(&o.trace, "trace", "trace.jsonl", "structured packet and Pion trace")
	flag.Parse()
	if o.scenario == "tail" {
		o.messages = 1
	}
	if o.scenario == "ambiguous" {
		o.messages = 3
		o.interval = max(o.interval, o.delay+2*time.Millisecond)
	}
	if o.messages < 1 || o.size < 4 || o.size > 65536 || o.delay <= 0 || o.interval <= 0 || o.drops < 0 {
		fmt.Fprintln(os.Stderr, "invalid experiment parameters")
		os.Exit(2)
	}
	switch o.scenario {
	case "loss", "tail", "reorder", "ambiguous":
	default:
		fmt.Fprintln(os.Stderr, "unknown scenario")
		os.Exit(2)
	}
	if err := os.MkdirAll(filepath.Dir(o.trace), 0755); err != nil {
		panic(err)
	}
	out, err := os.Create(o.trace)
	if err != nil {
		panic(err)
	}
	r := &recorder{start: time.Now(), output: out}
	ctx, cancel := context.WithTimeout(context.Background(), o.timeout)
	r.event("config", map[string]any{"variant": variant, "go": runtime.Version(), "os": runtime.GOOS, "options": o.fields(), "dependency": dependency()})
	if o.listen != "" {
		err = serve(ctx, o, r)
	} else {
		err = experiment(ctx, o, r)
	}
	if err != nil {
		r.event("failure", map[string]any{"error": err.Error()})
		fmt.Fprintln(os.Stderr, err)
	}
	cancel()
	if closeErr := out.Close(); closeErr != nil {
		panic(closeErr)
	}
	if err != nil {
		os.Exit(1)
	}
}
func dependency() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "unknown"
	}
	for _, dep := range info.Deps {
		if dep.Path == "github.com/pion/sctp" {
			if dep.Replace != nil {
				return dep.Version + " => " + dep.Replace.Path
			}
			return dep.Version
		}
	}
	return "unknown"
}
func (o options) fields() map[string]any {
	return map[string]any{"scenario": o.scenario, "drops": o.drops, "messages": o.messages, "size": o.size, "interval_ms": o.interval.Milliseconds(), "delay_ms": o.delay.Milliseconds(), "interleaving": o.interleaving, "remote": o.remote, "listen": o.listen}
}
func experiment(ctx context.Context, o options, r *recorder) error {
	var remote *net.UDPAddr
	var err error
	var receiverDone chan error
	if o.remote == "" {
		receiver, err := newWire(ctx, "127.0.0.1:0", nil, o.delay, r, "receiver")
		if err != nil {
			return err
		}
		defer receiver.Close()
		remote = receiver.LocalAddr().(*net.UDPAddr)
		receiverDone = make(chan error, 1)
		go func() { receiverDone <- echo(ctx, o, r, receiver) }()
	} else {
		remote, err = net.ResolveUDPAddr("udp4", o.remote)
		if err != nil {
			return err
		}
	}
	sender, err := newWire(ctx, "0.0.0.0:0", remote, o.delay, r, "sender")
	if err != nil {
		return err
	}
	defer sender.Close()
	f := &faults{record: r, scenario: o.scenario, drops: o.drops, delay: o.delay, chunks: map[uint32]*transmission{}}
	sender.faults = f
	summary, closeSender, err := send(ctx, o, r, sender, f)
	defer closeSender()
	if err != nil {
		return err
	}
	if receiverDone != nil {
		select {
		case err = <-receiverDone:
			if err != nil {
				return err
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	summary["variant"], summary["os"], summary["go"] = variant, runtime.GOOS, runtime.Version()
	summary["complete"], summary["options"] = true, o.fields()
	r.event("summary", summary)
	return json.NewEncoder(os.Stdout).Encode(summary)
}
