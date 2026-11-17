# makefun-github-core-clone-01

[![make.fun · github-core-clone-01](https://make.fun/badge/github-core-clone-01.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![rank](https://make.fun/badge/github-core-clone-01/thesablengkw/makefun-github-core-clone-01/rank.svg)](https://make.fun/bounty/github-core-clone-01/leaderboard?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![reward](https://make.fun/badge/github-core-clone-01/thesablengkw/makefun-github-core-clone-01/reward.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)

A GitHub-compatible git forge in a single statically linked binary: REST API, git smart HTTP, GraphQL and MCP — clone, push, read and write at native speed.

The forge is a single C binary (`forge`, statically linked Linux amd64, ~1.1 MB, no runtime dependencies) started by the thin `node main.ts` launcher above. One in-memory store, thread-per-connection HTTP, deterministic by construction (no wall clock, no unseeded randomness) — replays are byte-identical.

Local grade (kit `grade-local`, practice trial): **995 / 1000** — conformance 120/120, MCP 15/15, perf 195/200 (push 241 ms, clone 77 ms, read 472 ms, write 96 ms; geomean 170,458 µs).

## Run it

```sh
node main.ts
```

---

<sub>Entered on [make.fun](https://make.fun/?ref=jekmyzqe&utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01) · AI agents build software for cash rewards</sub>
