"""Build diagnostic overlays from the frozen baseline; production files remain untouched."""
import hashlib
import json
import os
from pathlib import Path
import shutil
import subprocess

ROOT = Path(__file__).resolve().parents[4]
BUILD = ROOT / ".tmp" / os.environ.get("WS_ADAPTIVE_BUILD", "webrtc-bottleneck-20261024-baseline")
SNAPSHOT = BUILD / "snapshot"
HERE = Path(__file__).parent
FLAVOR = os.environ.get("WS_BOTTLENECK_PROFILE_FLAVOR", "")
SUFFIX = "-" + FLAVOR if FLAVOR else ""
OVERLAYS = BUILD / ("profile-overlays" + SUFFIX)
OVERLAYS.mkdir(exist_ok=True)
TARGET = BUILD / ("binaries-profile" + SUFFIX)
TARGET.mkdir(exist_ok=True)
replacements = {}

def overlay(relative, text):
    target = OVERLAYS / relative.replace("/", "_")
    target.write_text(text, encoding="utf-8")
    replacements[str(SNAPSHOT / relative)] = str(target)

sampler = (HERE.parent / "preflight-20261023/native-profile.go.txt").read_text()
sampler = sampler.replace('active = true', 'runtime.SetBlockProfileRate(1000000); runtime.SetMutexProfileFraction(100); active = true')
sampler = sampler.replace('pprof.StopCPUProfile()\n                cpu.Close()', '''pprof.StopCPUProfile()
                for _, name := range []string{"block", "mutex", "allocs", "goroutine"} {
                    profile, err := os.Create(filepath.Join(directory, "sender-" + name + ".pprof"))
                    if err == nil { pprof.Lookup(name).WriteTo(profile, 0); profile.Close() }
                }
                cpu.Close()''')
overlay("cmd/wind/z_bottleneck_profile.go", sampler)
connection = (SNAPSHOT / "transport/webrtc/provider/connection.go").read_text()
anchor = "connection.PeerConnection = pc"
assert connection.count(anchor) == 1
overlay("transport/webrtc/provider/connection.go", connection.replace(anchor, anchor + "\n researchObserveSCTP(pc)"))
overlay("transport/webrtc/provider/z_bottleneck_profile.go", r'''package provider

import (
    "encoding/json"
    "fmt"
    "os"
    "path/filepath"
    "sync/atomic"
    "time"

    pion "github.com/pion/webrtc/v4"
    "golang.org/x/sys/windows"
)
var researchPeerID atomic.Uint64

func researchCPUSeconds() map[string]float64 {
    var creation, exit, kernel, user windows.Filetime
    _ = windows.GetProcessTimes(windows.CurrentProcess(), &creation, &exit, &kernel, &user)
    seconds := func(v windows.Filetime) float64 { return float64(uint64(v.HighDateTime)<<32|uint64(v.LowDateTime))/1e7 }
    return map[string]float64{"kernel": seconds(kernel), "user": seconds(user)}
}
func researchObserveSCTP(pc *pion.PeerConnection) {
    name := os.Getenv("WS_RESEARCH_PION_STATS")
    if name == "" { return }
    id := researchPeerID.Add(1)
    go func() {
        output, err := os.Create(filepath.Join(filepath.Dir(name), fmt.Sprintf("native-stats-%d.ndjson", id)))
        if err != nil { return }
        defer output.Close()
        encoder := json.NewEncoder(output)
        ticks := time.NewTicker(100*time.Millisecond)
        defer ticks.Stop()
        for range ticks.C {
            if pc.ConnectionState() == pion.PeerConnectionStateClosed { return }
            transport := pc.SCTP()
            if transport == nil { continue }
            pair, _ := transport.Transport().ICETransport().GetSelectedCandidatePair()
            _ = encoder.Encode(map[string]any{"time": time.Now().UnixMilli(), "id": id, "cpu": researchCPUSeconds(),
                "state": pc.ConnectionState().String(), "sctp": transport.Stats(), "queues": transport.ResearchQueues(),
                "buffered": transport.BufferedAmount(), "pair": pair})
        }
    }()
}
''')
overlay("third_party/pion/sctp/z_bottleneck_profile.go", r'''package sctp

func (a *Association) ResearchQueues() map[string]any {
    a.lock.Lock()
    defer a.lock.Unlock()
    return map[string]any{
        "pendingBytes": a.pendingQueue.getNumBytes(), "inflightBytes": a.inflightQueue.getNumBytes(),
        "packetsSent": a.stats.getNumPacketsSent(), "packetsReceived": a.stats.getNumPacketsReceived(),
        "fastRetransmissions": a.stats.getNumFastRetrans(), "t3Timeouts": a.stats.getNumT3Timeouts(),
        "ackTimeouts": a.stats.getNumAckTimeouts(),
    }
}
''')
overlay("third_party/pion/webrtc/z_bottleneck_profile.go", r'''package webrtc

func (r *SCTPTransport) ResearchQueues() map[string]any {
    association := r.association()
    if association == nil { return nil }
    return association.ResearchQueues()
}
''')
if FLAVOR == "wall":
    association = (SNAPSHOT / "third_party/pion/sctp/association.go").read_text()
    association = association.replace("type Association struct {", "type Association struct {\n researchWriteCount atomic.Uint64; researchWriteNs atomic.Uint64; researchWriteSlow atomic.Uint64")
    anchor = "_, err := a.netConn.Write(raw)"
    assert association.count(anchor) == 1
    association = association.replace(anchor, """researchStarted := time.Now()
                _, err := a.netConn.Write(raw)
                researchElapsed := time.Since(researchStarted)
                a.researchWriteCount.Add(1); a.researchWriteNs.Add(uint64(researchElapsed))
                if researchElapsed >= time.Millisecond { a.researchWriteSlow.Add(1) }""")
    overlay("third_party/pion/sctp/association.go", association)
    queue_path = Path(replacements[str(SNAPSHOT / "third_party/pion/sctp/z_bottleneck_profile.go")])
    queue_path.write_text(queue_path.read_text().replace('"pendingBytes":', '"writeCount": a.researchWriteCount.Load(), "writeNs": a.researchWriteNs.Load(), "writeSlow": a.researchWriteSlow.Load(), "pendingBytes":'))
    addr = (SNAPSHOT / "third_party/pion/ice/addr.go").read_text()
    anchor = "return c.conn.WriteToUDPAddrPort(b, addr)"
    assert addr.count(anchor) == 1
    overlay("third_party/pion/ice/addr.go", addr.replace(anchor, "return researchUDPWrite(c.conn, b, addr)"))
    overlay("third_party/pion/ice/z_bottleneck_profile.go", r'''package ice

import ("net"; "net/netip"; "sync/atomic"; "time")
var researchUDPCount atomic.Uint64
var researchUDPTime atomic.Uint64
var researchUDPSlow atomic.Uint64
func researchUDPWrite(conn *net.UDPConn, b []byte, addr netip.AddrPort) (int, error) {
    started := time.Now()
    n, err := conn.WriteToUDPAddrPort(b, addr)
    elapsed := time.Since(started)
    researchUDPCount.Add(1); researchUDPTime.Add(uint64(elapsed))
    if elapsed >= time.Millisecond { researchUDPSlow.Add(1) }
    return n, err
}
func ResearchUDPTiming() map[string]uint64 {
    return map[string]uint64{"writes": researchUDPCount.Load(), "nanoseconds": researchUDPTime.Load(), "slowWrites": researchUDPSlow.Load()}
}
''')
    provider_path = Path(replacements[str(SNAPSHOT / "transport/webrtc/provider/z_bottleneck_profile.go")])
    provider = provider_path.read_text().replace('pion "github.com/pion/webrtc/v4"', 'pion "github.com/pion/webrtc/v4"\n "github.com/pion/ice/v4"')
    provider_path.write_text(provider.replace('"buffered": transport.BufferedAmount()', '"udp": ice.ResearchUDPTiming(), "buffered": transport.BufferedAmount()'))

path = BUILD / ("bottleneck-profile-overlay" + SUFFIX + ".json")
path.write_text(json.dumps({"Replace": replacements}, indent=2))
subprocess.run(["go", "build", "-overlay=" + str(path), "-o", str(TARGET / "wind.exe"), "./cmd/wind"],
    cwd=SNAPSHOT, env=dict(os.environ, GOWORK="off", GOTOOLCHAIN="local"), check=True)
shutil.copyfile(BUILD / "binaries/relay-native.exe", TARGET / "relay-native.exe")
identity = {name: hashlib.sha256((TARGET / name).read_bytes()).hexdigest() for name in ["wind.exe", "relay-native.exe"]}
identity["overlays"] = {str(Path(v).relative_to(BUILD)): hashlib.sha256(Path(v).read_bytes()).hexdigest() for v in replacements.values()}
(BUILD / ("binaries-profile" + SUFFIX + ".json")).write_text(json.dumps(identity, indent=2) + "\n")
print(json.dumps(identity))
