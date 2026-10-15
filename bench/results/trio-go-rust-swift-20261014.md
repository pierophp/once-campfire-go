# Go, Rust and Swift in the same container runner (2026-10-14)

Docker images for the three ports, 16 concurrent clients, 4 CPUs for the server (0-3) and 4 for the
load generator (4-7), 8 s per route, 3 repetitions with alternating order. Median requests/sec; every
response was HTTP 200 with zero errors.

| Route | Go before | Go after (`4adec6c`) | Rust | Swift | Go/Rust | Go/Swift |
|---|---:|---:|---:|---:|---:|---:|
| Room page | 41,611 | 53,487 | 45,480 | 66,709 | 1.18 | 0.80 |
| Messages page | 41,884 | 56,289 | 42,352 | 66,530 | 1.33 | 0.85 |
| Sidebar | 42,974 | 55,615 | 49,579 | 73,476 | 1.12 | 0.76 |
| Search | 43,232 | 56,900 | 48,124 | 78,173 | 1.18 | 0.73 |
| Post a message | 2,719 | 2,479 | 2,785 | 3,663 | 0.89 | 0.68 |

Peak cgroup memory: Go 127 MB, Rust 144 MB, Swift 174 MB. Read p99 for Go fell from ~1.6 ms to ~1.25 ms.

`post_message` is within run-to-run noise: Go ranged 2.4k-2.8k in both rounds, with p50 3.6 ms
(Rust 5.2 ms) but a p99 tail of 21-50 ms (Rust ~11 ms). Raw runs: `trio-go-rust-swift-20261014-before/`
and `trio-go-rust-swift-20261014-after/`. These Docker figures are not comparable with the README
table, which comes from the shared verification harness.
