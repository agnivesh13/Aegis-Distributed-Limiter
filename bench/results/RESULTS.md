# Results

Generated from `summary.csv` by `go run ./cmd/bench -table` on 2026-11-16. Each cell is the median of the point's runs, with the lowest and highest run in brackets where they differ.

How to read the columns:

- **Over-admission %:** requests admitted beyond what one exact limiter admits for the same requests, as a share of that ideal.
- **False rejects %:** requests rejected that the exact limiter would have admitted.
- **Peak in one period:** the most requests admitted for one key in any span of one period, divided by the limit. The sliding window counter itself allows about 1.05 (see S0).
- **Decision p99 (µs):** time inside the limiter, from the gateways' histogram. Buckets grow by 1.5×, so the value is known only to within its bucket; 10 means "in the first bucket".
- **Client p99 (ms):** whole-request time seen by the load generator.
- **Errors %:** requests that got neither a 2xx nor a 429.
- **Late sends %:** requests that left the load generator more than 5 ms behind schedule. Above 1%, the machine could not sustain the run.

Conditions: one laptop (12th Gen Intel Core i7-12700H), Docker, 1 CPU per gateway, 2 for the load generator. Defaults unless the point says otherwise: 3 gateways, sliding window, 100 requests per second per key, 10 keys, 2,000 requests per second offered, gossip every 100 ms with fanout 2, 30 s measured after a 5 s warm-up. Rows are in the order the points were first run.

Not in this table: six runs at sizes the laptop could not sustain, kept in `summary-oversized.csv`.

| Scenario | Point | Backend | Gateways | Runs | Over-admission % | False rejects % | Peak in one period | Decision p99 (µs) | Client p99 (ms) | Errors % | Late sends % |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| S0 | n1 | memory | 1 | 3 | 0.00 (0.00 to 0.01) | 0.02 (0.00 to 0.03) | 1.05 | 10 | 1.65 (1.64 to 1.65) | 0.00 | 0.01 (0.00 to 0.02) |
| S1 | default | redis | 3 | 3 | 0.00 | 0.02 (0.01 to 0.03) | 1.05 (1.04 to 1.06) | 395 (384 to 432) | 1.92 (1.88 to 1.92) | 0.00 | 0.00 (0.00 to 0.01) |
| S1 | default | gossip | 3 | 3 | 4.45 (3.71 to 4.61) | 0.00 | 1.19 (1.17 to 1.21) | 12 (11 to 12) | 1.71 (1.70 to 1.78) | 0.00 | 0.00 (0.00 to 0.01) |
| S4 | i10ms-n3 | gossip | 3 | 3 | 0.54 (0.49 to 0.54) | 0.00 | 1.06 (1.06 to 1.07) | 11 (10 to 11) | 1.75 (1.70 to 1.82) | 0.00 | 0.01 (0.00 to 0.01) |
| S4 | i50ms-n3 | gossip | 3 | 3 | 1.99 (1.71 to 2.20) | 0.00 | 1.10 (1.10 to 1.12) | 11 (11 to 12) | 1.75 (1.72 to 1.83) | 0.00 | 0.01 (0.00 to 0.01) |
| S4 | i100ms-n3 | gossip | 3 | 3 | 3.86 (3.34 to 4.33) | 0.00 | 1.16 (1.15 to 1.24) | 11 | 1.75 (1.71 to 1.78) | 0.00 | 0.00 (0.00 to 0.01) |
| S4 | i500ms-n3 | gossip | 3 | 3 | 20.12 (19.12 to 22.48) | 0.00 | 1.48 (1.46 to 1.54) | 12 | 1.72 (1.71 to 1.75) | 0.00 | 0.00 (0.00 to 0.01) |
| S6 | delay0ms | redis | 3 | 3 | 0.00 | 0.01 (0.00 to 0.02) | 1.04 (1.04 to 1.05) | 738 (714 to 792) | 1.96 (1.95 to 2.08) | 0.00 | 0.00 (0.00 to 0.01) |
| S6 | delay1ms | redis | 3 | 3 | 0.00 | 0.03 (0.02 to 0.03) | 1.05 (1.04 to 1.05) | 2889 (2886 to 2889) | 3.92 (3.89 to 3.96) | 0.00 | 0.01 (0.00 to 0.01) |
| S6 | delay5ms | redis | 3 | 3 | 0.01 (0.00 to 0.01) | 0.00 (0.00 to 0.01) | 1.05 (1.04 to 1.06) | 6554 (6551 to 6554) | 7.81 (7.74 to 7.82) | 0.00 | 0.00 |
| S2 | n1 | redis | 1 | 3 | 0.00 | 0.01 (0.00 to 0.01) | 1.05 | 371 (367 to 525) | 1.80 (1.78 to 2.08) | 0.00 | 0.00 (0.00 to 0.08) |
| S2 | n1 | gossip | 1 | 3 | 0.00 (0.00 to 0.01) | 0.00 (0.00 to 0.02) | 1.04 (1.04 to 1.06) | 10 | 1.62 (1.60 to 1.62) | 0.00 | 0.00 |
| S2 | n3 | redis | 3 | 3 | 0.00 | 0.01 (0.00 to 0.01) | 1.05 | 377 (376 to 377) | 1.86 (1.85 to 1.88) | 0.00 | 0.00 |
| S2 | n3 | gossip | 3 | 3 | 3.74 (3.28 to 3.81) | 0.00 | 1.17 (1.15 to 1.17) | 11 | 1.67 (1.66 to 1.67) | 0.00 | 0.00 |
| S2 | n5 | redis | 5 | 3 | 0.00 (0.00 to 0.02) | 0.00 (0.00 to 0.01) | 1.05 (1.04 to 1.06) | 382 (377 to 383) | 1.89 (1.87 to 1.90) | 0.00 | 0.00 |
| S2 | n5 | gossip | 5 | 3 | 7.07 (6.68 to 7.81) | 0.00 | 1.28 (1.23 to 1.30) | 12 (11 to 12) | 1.69 (1.68 to 1.71) | 0.00 | 0.00 |
| S3 | load0.5x | redis | 3 | 3 | 0.00 | 0.00 | 0.74 (0.74 to 0.79) | 379 (370 to 384) | 1.99 (1.99 to 2.02) | 0.00 | 0.01 (0.00 to 0.01) |
| S3 | load0.5x | gossip | 3 | 3 | 0.00 | 0.00 | 0.74 (0.74 to 0.79) | 12 | 1.75 (1.71 to 1.77) | 0.00 | 0.01 (0.01 to 0.03) |
| S3 | load1x | redis | 3 | 3 | 0.00 | 0.01 (0.01 to 0.02) | 1.13 (1.12 to 1.16) | 369 (360 to 708) | 1.95 (1.91 to 2.66) | 0.00 | 0.00 (0.00 to 0.01) |
| S3 | load1x | gossip | 3 | 3 | 1.41 (1.16 to 1.44) | 0.00 | 1.16 (1.14 to 1.19) | 10 | 2.22 (2.14 to 2.40) | 0.00 | 0.00 |
| S3 | load2x | redis | 3 | 3 | 0.00 (0.00 to 0.01) | 0.00 (0.00 to 0.03) | 1.05 (1.04 to 1.06) | 836 (801 to 2751) | 2.73 (2.63 to 18.47) | 0.00 | 0.00 (0.00 to 0.17) |
| S3 | load2x | gossip | 3 | 3 | 3.83 (3.12 to 3.90) | 0.00 | 1.18 (1.17 to 1.20) | 10 | 2.17 (2.06 to 2.48) | 0.00 | 0.00 |
| S5 | f1-n5 | gossip | 5 | 3 | 12.79 (12.56 to 13.34) | 0.00 | 1.42 (1.40 to 1.45) | 15 (15 to 27) | 2.79 (2.71 to 4.18) | 0.00 | 0.00 (0.00 to 0.25) |
| S5 | f2-n5 | gossip | 5 | 3 | 6.86 (6.67 to 7.19) | 0.00 | 1.24 (1.24 to 1.26) | 10 | 2.18 (2.17 to 2.41) | 0.00 | 0.00 |
| S5 | f4-n5 | gossip | 5 | 3 | 4.97 (4.09 to 5.12) | 0.00 | 1.20 (1.19 to 1.21) | 10 | 2.17 (2.15 to 2.18) | 0.00 | 0.00 |
| S8 | zipf1000 | redis | 3 | 3 | 0.00 | 0.00 (0.00 to 0.01) | 1.09 (1.07 to 1.10) | 842 (836 to 860) | 2.83 (2.79 to 3.05) | 0.00 | 0.00 (0.00 to 0.07) |
| S8 | zipf1000 | gossip | 3 | 3 | 0.59 (0.55 to 0.68) | 0.00 | 1.20 (1.15 to 1.22) | 10 | 2.19 (2.16 to 2.29) | 0.00 | 0.00 |
| S9 | r2000 | memory | 1 | 1 | 0.00 | 0.01 | 1.05 | 10 | 2.00 | 0.00 | 0.06 |
| S9 | r2000 | redis | 3 | 1 | 0.00 | 0.01 | 1.06 | 641 | 2.27 | 0.00 | 0.00 |
| S9 | r2000 | gossip | 3 | 1 | 2.88 | 0.00 | 1.20 | 10 | 2.04 | 0.00 | 0.00 |
| S9 | r4000 | memory | 1 | 1 | 0.00 | 0.05 | 1.03 | 10 | 27.64 | 0.00 | 0.34 |
| S9 | r4000 | redis | 3 | 1 | 0.00 | 0.00 | 1.02 | 1444 | 14.36 | 0.00 | 0.27 |
| S9 | r4000 | gossip | 3 | 1 | 4.96 | 0.00 | 1.28 | 10 | 1.75 | 0.00 | 0.00 |
| S9 | r8000 | memory | 1 | 1 | 0.00 | 0.01 | 1.07 | 10 | 50.22 | 0.00 | 0.56 |
| S9 | r8000 | gossip | 3 | 1 | 6.84 | 0.00 | 1.21 | 10 | 27.69 | 0.00 | 0.99 |
| S9 | r16000 | memory | 1 | 1 | 0.00 | 63.19 | 2.53 | 10 | 328787.22 | 94.37 | 114.56 |
| S9 | r16000 | gossip | 3 | 1 | 0.00 | 18.63 | 3.77 | 10 | 123589.50 | 74.95 | 113.95 |
| F1 | kill-at-10s | redis | 3 | 3 | 0.00 (0.00 to 0.57) | 0.37 (0.00 to 0.47) | 1.28 (1.11 to 1.65) | 2382 (1061 to 19229) | 5275.80 (5018.65 to 5982.85) | 27.11 (27.00 to 27.18) | 8.87 (3.57 to 9.92) |
| F1 | kill-at-10s | gossip | 3 | 3 | 1.57 (1.43 to 2.65) | 0.00 | 1.16 (1.14 to 1.24) | 10 | 5005.90 (5002.46 to 5020.19) | 27.23 (27.20 to 27.27) | 1.91 (1.33 to 2.68) |
| F2 | fail-open | redis | 3 | 3 | 30.60 (30.24 to 32.47) | 0.00 | 2.40 (2.40 to 2.51) | 74078 (74049 to 74165) | 63.34 (63.03 to 69.19) | 0.00 | 0.19 (0.17 to 0.22) |
| F2 | fail-closed | redis | 3 | 3 | 0.00 | 26.42 (20.58 to 27.32) | 1.20 (1.17 to 1.54) | 73920 (73832 to 73939) | 53.78 (52.54 to 54.22) | 27.67 (24.31 to 27.95) | 0.17 (0.12 to 0.20) |
| F3 | split-2-2 | gossip | 4 | 3 | 33.44 (33.02 to 33.85) | 0.00 | 2.15 (2.14 to 2.17) | 10 | 2.02 (2.01 to 2.06) | 0.00 | 0.00 |
| S4 | i10ms-n5 | gossip | 5 | 3 | 0.82 (0.81 to 0.87) | 0.00 | 1.08 (1.07 to 1.08) | 10 | 2.13 (2.13 to 2.16) | 0.00 | 0.00 |
| S4 | i50ms-n5 | gossip | 5 | 3 | 3.53 (3.29 to 3.68) | 0.00 | 1.16 (1.15 to 1.18) | 10 | 2.10 (2.08 to 2.10) | 0.00 | 0.00 |
| S4 | i100ms-n5 | gossip | 5 | 3 | 6.30 (6.10 to 6.91) | 0.00 | 1.27 (1.25 to 1.29) | 10 | 2.06 (2.06 to 2.07) | 0.00 | 0.00 |
| S4 | i500ms-n5 | gossip | 5 | 3 | 32.10 (30.68 to 32.38) | 0.00 | 1.78 (1.71 to 1.84) | 10 | 2.10 (2.02 to 2.10) | 0.00 | 0.00 (0.00 to 0.01) |
| S3 | load6x | redis | 3 | 3 | 0.03 (0.01 to 0.04) | 0.00 | 1.03 | 6561 (4370 to 7195) | 40.87 (32.44 to 41.45) | 0.00 | 0.70 (0.26 to 0.73) |
| S3 | load6x | gossip | 3 | 3 | 5.08 (4.87 to 7.68) | 0.00 | 1.24 (1.18 to 1.34) | 10 | 12.78 (11.81 to 14.25) | 0.00 | 0.60 (0.49 to 0.78) |
| S7 | burst | redis | 3 | 3 | 0.06 (0.00 to 5.90) | 0.00 | 1.52 (1.14 to 1.61) | 1890 (511 to 65642) | 8.15 (1.94 to 83.54) | 0.00 | 0.00 (0.00 to 6.72) |
| S7 | burst | gossip | 3 | 3 | 4.81 (4.19 to 6.38) | 0.00 | 1.45 (1.21 to 1.90) | 10 | 2.01 (1.67 to 5.93) | 0.00 | 0.00 (0.00 to 0.38) |
| S9 | r8000 | redis | 3 | 1 | 0.03 | 0.00 | 1.04 | 7118 | 38.24 | 0.00 | 0.57 |
| S9 | r16000 | redis | 3 | 1 | 0.00 | 57.00 | 7.20 | 112227 | 111765.10 | 95.51 | 115.60 |
