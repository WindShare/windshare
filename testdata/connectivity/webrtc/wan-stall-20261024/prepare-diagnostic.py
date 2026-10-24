"""Frozen Go overlays only. Diagnostic timings are never pooled with baseline."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import shutil
import subprocess

ROOT = Path(__file__).resolve().parents[4]
HERE = Path(__file__).parent


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--base", required=True)
    parser.add_argument("--name", required=True)
    parser.add_argument("--interface-filter", action="store_true")
    parser.add_argument("--socket-buffer", action="store_true")
    args = parser.parse_args()
    base = ROOT / ".tmp" / args.base
    snapshot = base / "snapshot"
    build = ROOT / ".tmp" / args.name
    build.mkdir()
    overlays = build / "overlays"
    overlays.mkdir()
    binaries = build / "binaries"
    binaries.mkdir()
    os.link(base / "fixture.bin", build / "fixture.bin")
    shutil.copyfile(base / "binaries/relay-native.exe", binaries / "relay-native.exe")
    replacements = {}

    def overlay(relative, text):
        target = overlays / relative.replace("/", "_")
        target.write_text(text, encoding="utf-8")
        replacements[str(snapshot / relative)] = str(target)

    def replace(relative, anchor, replacement):
        source = (snapshot / relative).read_text()
        assert source.count(anchor) == 1, relative
        overlay(relative, source.replace(anchor, replacement))

    overlay("cmd/wind/z_wan_research.go", (HERE / "process-sampler.go.txt").read_text())
    overlay("transport/webrtc/provider/z_wan_research.go", (HERE / "sctp-sampler.go.txt").read_text())
    replace("transport/webrtc/provider/connection.go",
            "connection.PeerConnection = pc", "connection.PeerConnection = pc\n researchObserveSCTP(pc)")
    overlay("core/transfer/lanes/z_wan_research.go", (HERE / "scheduler-sampler.go.txt").read_text())
    source = (snapshot / "core/transfer/lanes/set.go").read_text()
    assert source.count("return &Set{") == 1 and source.count("}, nil\n}") == 1
    source = source.replace("return &Set{", "result := &Set{").replace("}, nil\n}", "}\n researchObserveSet(result)\n return result, nil\n}")
    overlay("core/transfer/lanes/set.go", source)
    association = (snapshot / "third_party/pion/sctp/association.go").read_text()
    association = association.replace("type Association struct {",
        "type Association struct {\n researchWriteCount atomic.Uint64; researchWriteNs atomic.Uint64; researchWriteSlow atomic.Uint64")
    anchor = "_, err := a.netConn.Write(raw)"
    assert association.count(anchor) == 1
    association = association.replace(anchor, """researchStarted := time.Now()
                _, err := a.netConn.Write(raw)
                researchElapsed := time.Since(researchStarted)
                a.researchWriteCount.Add(1); a.researchWriteNs.Add(uint64(researchElapsed))
                if researchElapsed >= time.Millisecond { a.researchWriteSlow.Add(1) }""")
    overlay("third_party/pion/sctp/association.go", association)
    overlay("third_party/pion/sctp/z_wan_research.go", """package sctp
func (a *Association) ResearchQueues() map[string]any {
    a.lock.Lock()
    defer a.lock.Unlock()
    return map[string]any{
        "pendingBytes": a.pendingQueue.getNumBytes(), "inflightBytes": a.inflightQueue.getNumBytes(),
        "packetsSent": a.stats.getNumPacketsSent(), "packetsReceived": a.stats.getNumPacketsReceived(),
        "fastRetransmissions": a.stats.getNumFastRetrans(), "t3Timeouts": a.stats.getNumT3Timeouts(),
        "ackTimeouts": a.stats.getNumAckTimeouts(), "writeCount": a.researchWriteCount.Load(),
        "writeNs": a.researchWriteNs.Load(), "writeSlow": a.researchWriteSlow.Load(),
    }
}
""")
    overlay("third_party/pion/webrtc/z_wan_research.go", """package webrtc
func (r *SCTPTransport) ResearchQueues() map[string]any {
    association := r.association()
    if association == nil { return nil }
    return association.ResearchQueues()
}
""")
    replace("third_party/pion/ice/addr.go", "return c.conn.WriteToUDPAddrPort(b, addr)",
            "return researchUDPWrite(c.conn, b, addr)")
    overlay("third_party/pion/ice/z_wan_research.go", (HERE / "udp-sampler.go.txt").read_text())
    if args.interface_filter:
        relative = "connectivity/nativepeer/addresses.go"
        text = (snapshot / relative).read_text()
        anchor = "func selectAddresses(addresses []networkstate.Address) []netip.Addr {"
        assert text.count(anchor) == 1
        overlay(relative, text.replace(anchor, anchor + "\n addresses = researchFilterAddresses(addresses)"))
        overlay("connectivity/nativepeer/z_wan_interface.go", """package nativepeer
import ("os"; "github.com/windshare/windshare/connectivity/networkstate")
func researchFilterAddresses(addresses []networkstate.Address) []networkstate.Address {
    name := os.Getenv("WS_WAN_INTERFACE")
    if name == "" { return addresses }
    result := make([]networkstate.Address, 0, len(addresses))
    for _, address := range addresses {
        if address.InterfaceName == name { result = append(result, address) }
    }
    return result
}
""")
    if args.socket_buffer:
        replace("connectivity/socketauthority/authority.go",
                "mux.endpoints = append(mux.endpoints, ice.NewUniversalUDPMuxDefault",
                "researchSocketBuffer(conn)\n\t\tmux.endpoints = append(mux.endpoints, ice.NewUniversalUDPMuxDefault")
        overlay("connectivity/socketauthority/z_wan_buffer_linux.go", (HERE / "socket-buffer_linux.go.txt").read_text())
        overlay("connectivity/socketauthority/z_wan_buffer_windows.go",
                "package socketauthority\nimport \"net\"\nfunc researchSocketBuffer(net.PacketConn) {}\n")
    path = build / "overlay.json"
    path.write_text(json.dumps({"Replace": replacements}, indent=2))
    environment = dict(os.environ, GOWORK="off", GOTOOLCHAIN="local", CGO_ENABLED="0", GOARCH="amd64")
    for platform, name in [("windows", "wind.exe"), ("linux", "wind-linux")]:
        subprocess.run(["go", "build", "-overlay=" + str(path), "-o", str(binaries / name), "./cmd/wind"],
                       cwd=snapshot, env=dict(environment, GOOS=platform), check=True)
    identity = {
        "base": args.base, "baseSourcesSHA256": hashlib.sha256((base / "sources.json").read_bytes()).hexdigest(),
        "diagnostic": True,
        "binaries": {p.name: hashlib.sha256(p.read_bytes()).hexdigest() for p in binaries.iterdir()},
        "overlays": {str(Path(p).relative_to(build)): hashlib.sha256(Path(p).read_bytes()).hexdigest() for p in replacements.values()},
    }
    (build / "identity.json").write_text(json.dumps(identity, indent=2) + "\n")
    print(json.dumps({"event": "diagnostic-built", "name": args.name}), flush=True)


if __name__ == "__main__":
    main()
