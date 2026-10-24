# Releasing attos-go

Rules: commit as the maintainer (no AI trailers), `git push origin main` fast-forward only (fetch and rebase first, never force), tag only from `main` after the gates pass. Do not release the Go, Rust and Java SDKs out of step with a compiler change: readers first (see FORMAT_COMPATIBILITY.md).

## Where the version lives

| Where | Current value |
|---|---|
| Module path | `github.com/Attos-Labs/attos-go` (`go.mod`, `go 1.24.3`) |
| SDK version constant | `Version` in `sync.go` (currently `1.3.0`); it goes into the User-Agent and telemetry |
| Release tag | `v1.3.0` already exists; `v0.1.0` and `v1.2.0` before it |

The Go module version *is* the git tag; there is no version field in `go.mod`. Keep `sync.go` `Version` equal to the tag without the `v`.

## Before tagging: run the gates

```bash
# from a checkout of Attos-Labs/attos-cloud with attos-go, attos-rust and attos-java next to it
scripts/release-gate/run.sh        # every format option combination read back by all three SDKs (must print "release gate PASSED")
```

## SDK test suite

```bash
cd attos-go
GOFLAGS=-buildvcs=false go vet ./... && go test ./... -count=1     # about 1 minute
./run_tests.sh                                                      # the repo's own gate
```

## Release steps

1. Update `CHANGELOG.md` (move "Unreleased" under the new version and date) and set `Version` in `sync.go`.
2. Commit on `main` (fast-forward only), `git push origin main`.
3. Tag and push the tag (annotated):
   ```bash
   git tag -a v1.3.1 -m "attos-go v1.3.1"
   git push origin v1.3.1
   ```
4. Force the Go proxy to fetch it and check:
   ```bash
   GOFLAGS=-mod=mod GOPROXY=https://proxy.golang.org go list -m github.com/Attos-Labs/attos-go@v1.3.1
   ```
5. Smoke test from a scratch module: `go get github.com/Attos-Labs/attos-go@v1.3.1`, build, open a golden file.

**Credentials:** only push access to `Attos-Labs/attos-go` (a GitHub account with write permission). Nothing to publish to: the Go proxy reads the public tag.

**Never** move or delete a published tag; the proxy caches it forever. If a release is bad, ship a new patch version and add a `retract` directive to `go.mod`.

## After the release

* Update `docs/FORMAT_COMPATIBILITY.md` in `Attos-Labs/attos-cloud` (the row for this SDK).
* Update the version shown in the dashboard docs and the site, if it names one.
* Announce which format features this release adds, so operators know when it is safe to compile with them.
