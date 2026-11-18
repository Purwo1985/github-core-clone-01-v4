# github-core-clone-01-v4

[![make.fun score](https://make.fun/badge/score/github-core-clone-01-v4)](https://make.fun/competitions/github-core-clone-01)
[![make.fun rank](https://make.fun/badge/rank/github-core-clone-01-v4)](https://make.fun/competitions/github-core-clone-01)
[![make.fun submission](https://make.fun/badge/submission/github-core-clone-01-v4)](https://make.fun/competitions/github-core-clone-01)

A GitHub-compatible git forge in Go (standard library only, static
binary) with a thin Node launcher: raw HTTP/1.1, zero-alloc hot paths,
git smart HTTP (upload-pack v2, receive-pack v0), REST v3, GraphQL and
MCP. Built for the make.fun `github-core-clone-01` competition.

## Layout

- `main.ts` — launcher: spawns `./forge`, waits for `READY`, registers
  the port with the environment, answers the boot health checks, then
  long-polls `/done` and exits 0.
- `forge` — the static Go binary (vendored source in this repo).
- `*.go` — the forge: `main.go` (entry), `http.go` (raw TCP HTTP/1.1),
  `ident.go` (deterministic scenario identity), `obj.go` (git objects,
  packs, reachability), `store.go` (repos, issues, pull requests),
  `gitproto.go` (git smart HTTP), `rest.go` (REST v3), `gql.go`
  (GraphQL), `mcp.go` (MCP), `json.go` (hand-rolled JSON).

## Build

```sh
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "-s -w" -o forge .
```

## Run

```sh
MAKEFUN_SHOP_TOKEN=shpat_... MAKEFUN_TRIAL_NOW=1735689600 ./forge
# prints: READY port=<ephemeral>
```

## Performance notes

- Raw TCP HTTP/1.1 (no `net/http`): one goroutine per connection, a
  reusable read buffer per connection, one `Write` per response.
- Pre-rendered JSON for repos, issues, comments and pull requests;
  the issue-read hot path is a map lookup plus a buffer copy.
- Pack, ls-refs and advertisement responses are cached per repo
  version, so the 50-way concurrent shallow clone serves the same
  bytes after the first build.
- Fast-forward checks walk at most one parent step on linear history.
- Packs are deflated at `flate.BestSpeed`; object ids are SHA-1 over
  the exact git header, computed once per object.

## License

MIT — see `LICENSE`.
