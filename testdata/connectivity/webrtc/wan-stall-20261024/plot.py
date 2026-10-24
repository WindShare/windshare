"""Plot exported evidence without consulting private raw artifacts."""
import json
from pathlib import Path
import sys

HERE = Path(__file__).parent
ROOT = HERE.parents[3]
packages = ROOT / ".tmp/webrtc-bottleneck-20261024-baseline/plot-packages"
if packages.exists():
    sys.path.insert(0, str(packages))
import matplotlib
matplotlib.use("Agg")
import matplotlib.pyplot as plt

MIB = 1048576


def main():
    evidence = json.loads((HERE / "interface-results.json").read_text())
    derived = json.loads((HERE / "derived-results.json").read_text())
    cases = {r["source"]["sha256"]: r.get("case") for r in derived["runs"]}
    plt.rcParams.update({"font.family": "DejaVu Sans", "font.size": 10, "axes.spines.top": False,
                         "axes.spines.right": False, "svg.fonttype": "none"})
    fig, (path_ax, fault_ax) = plt.subplots(1, 2, figsize=(12.6, 4.5), constrained_layout=True)
    labeled = set()
    for run in evidence["runs"]:
        case = cases.get(run["source"]["sha256"])
        if not case or case.get("endpoint") != "public" or case.get("direction") != "reverse" or "windowsInterface" not in case:
            continue
        interface = case["windowsInterface"]
        points = run["traces"][0]["progress"]
        if not points:
            continue
        color = "#147d92" if interface == "WiFi" else "#c75136"
        label = "Physical Wi-Fi: 3 completed" if interface == "WiFi" else "Mihomo: 3 exceeded 90 s"
        path_ax.plot([p["ms"] / 1000 for p in points], [p["bytes"] / MIB for p in points],
                     color=color, alpha=.8, linewidth=1.7, label=label if interface not in labeled else None)
        if interface != "WiFi":
            path_ax.plot(90, points[-1]["bytes"] / MIB, marker="x", color=color)
        labeled.add(interface)
    path_ax.set(title="Public Linux → Windows, 64 MiB", xlabel="Seconds after receiver launch",
                ylabel="Verified output (MiB)", xlim=(0, 95), ylim=(-2, 67))
    path_ax.legend(loc="upper left", frameon=False)
    path_ax.text(.98, .30, "Slow paths still receive fragments;\nzero verified output does not prove a deadlock.",
                 transform=path_ax.transAxes, ha="right", color="#555555", fontsize=9)

    fault_runs = [r for r in derived["runs"] if (r.get("case") or {}).get("fault") == "drop-large-once"
                  and r["case"].get("connectivity", "p2p-only") == "p2p-only" and r["scheduler"]]
    for index, run in enumerate(fault_runs):
        series = run["scheduler"][0]["timeline"]
        fault_ax.plot([p["seconds"] for p in series], [p["usefulBytes"] / MIB for p in series],
                      color="#7652a3", linestyle=["-", "--"][index % 2],
                      label=f"Validated blocks, repetition {index + 1}")
        fault_ax.plot([p["seconds"] for p in series], [p["outputBytes"] / MIB for p in series],
                      color="#333333", label="Ordered output, both repetitions" if index == 0 else None)
    fault_ax.set(title="LAN: 25 s of induced large-UDP loss", xlabel="Seconds after first scheduler sample",
                 ylabel="Payload (MiB)", xlim=(0, 70), ylim=(-.3, 7))
    fault_ax.legend(loc="upper left", frameon=False)
    fault_ax.text(.98, .36, "After packets resume, 6 MiB arrives.\nThe first two blocks remain unassigned.",
                  transform=fault_ax.transAxes, ha="right", color="#555555", fontsize=9)
    for axis in (path_ax, fault_ax):
        axis.grid(alpha=.15)
    fig.suptitle("Path quality and scheduler liveness are separate failures", fontsize=14)
    fig.savefig(HERE / "bottlenecks.png", dpi=170)
    fig.savefig(HERE / "bottlenecks.svg")


def plot_buffers():
    evidence_path = HERE / "buffer-results.json"
    if not evidence_path.exists():
        return
    evidence = json.loads(evidence_path.read_text())
    derived = json.loads((HERE / "derived-results.json").read_text())
    facts = {run["source"]["sha256"]: run for run in derived["runs"]}
    groups = {0: [], 262144: [], 4194304: []}
    for run in evidence["runs"]:
        row = facts[run["source"]["sha256"]]
        if run["report"]["status"] != "passed":
            continue
        round_ = run["report"]["rounds"][0]
        delta = row["udpCounterDeltas"][0]["linux"]
        drops = sum(family.get("RcvbufErrors", 0) for family in delta.values())
        groups[run["report"]["receiveBufferBytes"]].append((round_["MBpsIncludingStartup"], drops))
    fig, axes = plt.subplots(1, 2, figsize=(10, 4), constrained_layout=True)
    for x, (request, samples) in enumerate(groups.items()):
        for index, values in enumerate(samples):
            offset = (index - (len(samples) - 1) / 2) * .08
            for axis, value in zip(axes, values):
                axis.scatter(x + offset, value, s=50, color=["#c75136", "#888888", "#147d92"][x], zorder=3)
    for axis in axes:
        axis.set_xticks(range(3), ["Default: 208 KiB\n3 runs", "Requested 256 KiB\n1 exploratory run", "Requested 4 MiB\n3 runs"])
        axis.grid(axis="y", alpha=.15)
        axis.set_ylim(bottom=0)
    axes[0].set(title="Native Windows → Linux, 256 MiB", ylabel="MB/s including process startup", ylim=(0, 14))
    axes[1].set(title="Linux UDP receive-buffer overflows", ylabel="Counter increase during each transfer", ylim=(-3, 85))
    fig.suptitle("Larger socket buffers remove one loss source, without a large throughput increase", fontsize=12)
    fig.savefig(HERE / "receive-buffers.png", dpi=170)
    fig.savefig(HERE / "receive-buffers.svg")


if __name__ == "__main__":
    main()
    plot_buffers()
