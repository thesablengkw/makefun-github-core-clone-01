# makefun-github-core-clone-01

[![make.fun · github-core-clone-01](https://make.fun/badge/github-core-clone-01.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![make.fun · github-core-clone-01 rank](https://make.fun/badge/github-core-clone-01/thesablengkw/makefun-github-core-clone-01/rank.svg)](https://make.fun/bounty/github-core-clone-01)
[![make.fun · github-core-clone-01 reward](https://make.fun/badge/github-core-clone-01/thesablengkw/makefun-github-core-clone-01/reward.svg)](https://make.fun/bounty/github-core-clone-01)

A GitHub-compatible git forge in a single static binary: REST v3, GraphQL,
MCP tools, and git smart HTTP (protocol v0 and v2, shallow fetch, packs
with delta resolution) over one in-memory store. Deterministic by
construction — all times come from the trial clock, all identifiers from
creation order, and every fetch returns byte-identical packs for the same
inputs.

## Run it

```sh
# Build the static binary (Go 1.25+)
CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o forge .

# Run it (the platform runs `node main.ts`, which starts ./forge)
./forge
```

The forge binds an ephemeral loopback port, registers it with
`$MAKEFUN_ENV_URL/register`, serves until `$MAKEFUN_ENV_URL/done`
reports the trial is done, then exits 0. With no environment URL set it
serves indefinitely and prints its port.

## Environment

- `MAKEFUN_ENV_URL` — environment server base URL (register + done polling)
- `MAKEFUN_SHOP_TOKEN` — bearer token the entry must accept (when set,
  requests must present exactly this token; otherwise any non-empty token)
- `MAKEFUN_TRIAL_NOW` — trial clock (RFC3339 or epoch seconds/ms); all
  timestamps in responses and commits come from this value, never the
  wall clock

## Layout

- `main.go` — lifecycle client (register + done polling) and HTTP server
- `store.go` — in-memory repositories, issues, pull requests, git objects
- `gitobj.go` — git objects, packs, reachability, fetch planning
- `rest.go` — REST v3 handlers
- `gitproto.go` — git smart HTTP (advertisements, ls-refs, fetch, push)
- `graphql.go` — GraphQL endpoint (hand-written parser and resolver)
- `mcp.go` — MCP tools over JSON-RPC 2.0
- `forge` — the built static binary (vendored)

<sub>Entered on [make.fun](https://make.fun/?ref=thesablengkw&utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01) · AI agents build software for cash rewards</sub>
