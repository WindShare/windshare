"""Render the retained evidence; requires matplotlib, with no access to raw private artifacts."""
import json
from pathlib import Path
import sys

HERE = Path(__file__).parent
ROOT = HERE.parents[3]
local_packages = ROOT / ".tmp/webrtc-bottleneck-20261024-baseline/plot-packages"
if local_packages.exists():
    sys.path.insert(0, str(local_packages))
import matplotlib
matplotlib.use("Agg")
import matplotlib.pyplot as plt
import numpy as np

results = json.loads((HERE / "results.json").read_text())
runs = {run["label"]: run for run in results["runs"]}
plt.rcParams.update({"font.family": "DejaVu Sans", "font.size": 10,
    "axes.spines.top": False, "axes.spines.right": False})
fig = plt.figure(figsize=(12, 8.3), facecolor="#ffffff")
grid = fig.add_gridspec(3, 2, height_ratios=[1.2, 1, .65], hspace=.53, wspace=.23)
colors = ["#285a9f", "#19846f", "#a26928", "#8b53a5"]
names = ["Default\n(n=7)", "Fixed 3 PCs\n(n=3)", "No success\ncooldown (n=3)", "Longer settling\n+ no cooldown (n=3)"]
groups = ["1GiB-adaptive", "1GiB-fixed-three", "1GiB-success-no-cooldown", "1GiB-recovery-window"]
for column, (metric, title) in enumerate([
    ("clickMBps", "Complete 1 GiB transfer"),
    ("middle20to80MBps", "Fixed middle 20%-80% of output bytes"),
]):
    ax = fig.add_subplot(grid[0, column])
    for index, group in enumerate(groups):
        values = [runs[label]["rounds"][0][metric] for label in results["groups"][group]["labels"]]
        xs = index + np.linspace(-.13, .13, len(values))
        ax.scatter(xs, values, color=colors[index], s=32, zorder=3)
        median = results["groups"][group][metric]["median"]
        ax.plot([index-.24, index+.24], [median, median], color=colors[index], linewidth=2)
        ax.text(index+.26, median+.25, f"{median:.2f}", ha="left", fontsize=9, color=colors[index])
    ax.axhline(30, color="#64748b", linestyle="--", linewidth=1)
    ax.set_xticks(range(4), names, fontsize=8.5)
    ax.set_ylim(23, 36)
    ax.set_ylabel("Useful MB/s")
    ax.set_title(title, loc="left", weight="bold", fontsize=11)
    ax.grid(axis="y", color="#e8edf2")
    ax.set_axisbelow(True)

ax_rate = fig.add_subplot(grid[1, :])
ax_paths = fig.add_subplot(grid[2, :], sharex=ax_rate)
examples = [
    ("confirmation-5-large-original-3", "Default: typical", colors[0]),
    ("confirmation-6-large-original-4", "Default: early retirement", "#b44b4d"),
    ("confirmation-8-large-fixed-three-2", "Fixed 3 PCs", colors[1]),
]
for label, name, color in examples:
    row = runs[label]["rounds"][0]
    intervals = row["fiveSecondOutputIntervals"]
    xs = [point["fromSeconds"] + 2.5 for point in intervals]
    ys = [point["MBps"] for point in intervals]
    ax_rate.plot(xs, ys, marker="o", markersize=4, color=color, label=name, linewidth=1.7)
    changes = row["peerChanges"]
    seconds = [point["seconds"] for point in changes] + [row["elapsedSeconds"]]
    counts = [point["peers"] for point in changes] + [changes[-1]["peers"]]
    ax_paths.step(seconds, counts, where="post", color=color, linewidth=1.7)

ax_rate.axhline(30, color="#64748b", linestyle="--", linewidth=1)
ax_rate.set_title("Selected trajectories: 5-second output intervals and admitted connections", loc="left", weight="bold", fontsize=11)
ax_rate.set_ylabel("Useful MB/s")
ax_rate.set_ylim(10, 45)
ax_rate.grid(axis="y", color="#e8edf2")
ax_rate.legend(loc="upper right", frameon=False, ncol=3, fontsize=9)
ax_paths.set_ylabel("Connections")
ax_paths.set_xlabel("Seconds from transfer measurement start")
ax_paths.set_yticks([0, 1, 2, 3])
ax_paths.set_ylim(-.1, 3.3)
ax_paths.set_xlim(0, 44)
ax_paths.grid(axis="y", color="#e8edf2")
ax_paths.annotate("Retired before a second scheduler probe; ~15 s cooldown",
    xy=(14, 1), xytext=(17, 1.55), fontsize=9, color="#9b3c3e",
    arrowprops={"arrowstyle": "-", "color": "#9b3c3e"})
fig.suptitle("30 MB/s is attainable; automatic growth remains inconsistent", x=.07, y=.985, ha="left", weight="bold", fontsize=16)
fig.text(.07, .945, "Windows -> Linux Chrome | physical Wi-Fi LAN | read + seal + authenticate + OPFS | decimal MB/s", fontsize=10, color="#536475")
fig.subplots_adjust(top=.89, bottom=.09, left=.07, right=.98)
fig.text(.07, .025, "Dots: every non-profiled 1 GiB run in each arm. Bars and labels: medians. Curves: three named examples, not best-window scores.", fontsize=9, color="#536475")
fig.savefig(HERE / "throughput.png", dpi=170)
svg = HERE / "throughput.svg"
fig.savefig(svg)
# Matplotlib leaves spaces inside multiline path data; normalize only line ends
# so regenerated evidence satisfies the repository whitespace gate.
svg.write_text("\n".join(line.rstrip() for line in svg.read_text().splitlines()) + "\n", newline="\n")
print("Rendered throughput.png and throughput.svg")
