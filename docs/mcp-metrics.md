# Speedtest metrics

The MCP tools use the same units and definitions as the dashboard API.

| Metric | Unit | Meaning |
|---|---|---|
| Download | Mbps | Measured download throughput. |
| Upload | Mbps | Measured upload throughput. |
| Ping | milliseconds | Round-trip latency reported by the speed-test engine. |
| Jitter | milliseconds | Variation in the engine's ping measurements. |
| Loss | percent | Packet loss reported by the engine. |
| Download/upload latency | milliseconds | Latency measured during the corresponding loaded transfer. |
| Expected speed | Mbps | Owner-configured expected download or upload speed for that node. |
| Success rate | percent | `ok` and `degraded` tests divided by all tests in the interval. |
| Stability | percent | `clamp(100 - 220 × population standard deviation / mean, 0, 100)` for successful download values; unavailable when there is no positive mean. |

Summary counts include failed tests. Summary metric samples omit failed tests
and omit individual null measurements. Trend, hour-of-day, heatmap and
distribution statistics use `ok` tests only. A missing measurement is null,
never an invented zero. The combined dashboard total is a display calculation,
not a persisted network or load-balancer entity.
