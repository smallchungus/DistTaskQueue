#!/usr/bin/env python3
import csv
import sys

import matplotlib

matplotlib.use("Agg")
import matplotlib.pyplot as plt

src = sys.argv[1] if len(sys.argv) > 1 else "loadtest.csv"
dst = sys.argv[2] if len(sys.argv) > 2 else "loadtest.png"

t, depth, replicas = [], [], []
with open(src) as f:
    for row in csv.DictReader(f):
        t.append(int(row["elapsed_sec"]))
        depth.append(float(row["queue_depth"]))
        replicas.append(int(row["ready_replicas"]))

fig, (ax1, ax2) = plt.subplots(
    2, 1, sharex=True, figsize=(9, 5.5), height_ratios=[3, 1]
)
ax1.plot(t, depth, color="#2A7DE1", linewidth=1.8)
ax1.set_ylabel("queue:test depth")
ax1.grid(True, alpha=0.25, linewidth=0.5)
ax2.step(t, replicas, where="post", color="#C4532E", linewidth=1.8)
ax2.set_ylabel("ready replicas")
ax2.set_xlabel("seconds since start")
ax2.set_yticks(range(0, 6))
ax2.grid(True, alpha=0.25, linewidth=0.5)
fig.suptitle("Flood: 1000 synthetic jobs vs KEDA scale-out", fontsize=11)
fig.tight_layout()
fig.savefig(dst, dpi=150)
print(f"wrote {dst}")
