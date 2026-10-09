# only-dep-bump

A lightweight GitHub Action that labels pull requests that are **only a Go dependency bump**.

It follows the dependency-bump patterns of the [Chainlink repository](https://github.com/smartcontractkit/chainlink): pure Go bumps (e.g. `deps: bump chainlink-common to latest main`) touch nothing but `go.mod`/`go.sum` files, including in multi-module monorepos, while the Go version itself is pinned in the `go`/`toolchain` directives of `go.mod` (and `.tool-versions`).

## When a PR gets the label

The `only-dependency-bump` label is added when **all** of these hold:

1. Every changed file is a `go.mod` or `go.sum`, at any directory depth (multi-module repositories are supported).
2. At least one `require`/`replace` dependency version **increases** (semver comparison, including pseudo-versions like `v0.11.2-0.20261007…`).
3. No dependency version decreases — any **downgrade disqualifies**.
4. The Go version is untouched: any change to the `go` or `toolchain` directives in `go.mod` — **upgrade or downgrade** — disqualifies.
5. No other structural `go.mod` changes: module path, `exclude`/`retract`/`godebug`/`tool`/`ignore` directives, non-matching `replace` targets, added or deleted `go.mod` files, or an unparseable `go.mod`.

In every other case — any other file changed (`.go`, `.tool-versions`, workflows, docs…), a downgrade, or a Go version change — the label is **removed immediately** on the latest commit.

Draft PRs are never labeled (and any existing label is removed while a PR is in draft). The PR is re-evaluated at every commit once it is ready for review.

## Usage

```yaml
name: only-dep-bump
on:
  pull_request:
    types: [opened, synchronize, reopened, ready_for_review, converted_to_draft]
permissions:
  contents: read
  pull-requests: write
jobs:
  label:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v7
      - uses: 007vasy/only-dep-bump@v1
        with:
          token: ${{ github.token }}
          pr-number: ${{ github.event.pull_request.number }}
```

The `concurrency` group should be keyed on the PR number (see [.github/workflows/label.yml](.github/workflows/label.yml)) so that rapid pushes always leave the newest evaluation in charge.

### Inputs

| Input | Description | Default |
| --- | --- | --- |
| `token` | Token with `pull-requests: write` (e.g. `github.token`) | — *(required)* |
| `pr-number` | Pull request number to evaluate | — *(required)* |
| `label` | Label to apply | `only-dependency-bump` |
| `repo` | Repository (`owner/name`) of the pull request | current repository |

### Outputs

| Output | Description |
| --- | --- |
| `decision` | `true` when the label was applied |
| `reasons` | semicolon-separated disqualifiers |
| `upgrades` | semicolon-separated dependency upgrades found |

## How it works

The action (a single Go program, no checkout of the target repo needed) uses the GitHub REST API to:

1. Fetch the PR; if it is a draft, remove the label (if present) and stop.
2. List the changed files; anything that is not `go.mod`/`go.sum` disqualifies immediately.
3. Fetch every changed `go.mod` at the merge base and at the PR head and parse both with [`golang.org/x/mod/modfile`](https://pkg.go.dev/golang.org/x/mod/modfile).
4. Semver-compare every `require`/`replace` version (via `golang.org/x/mod/semver`) and check the `go`/`toolchain` and structural directives.
5. Add or remove the label via the REST API.

## Development

```sh
go test ./...   # unit tests for the decision logic
go vet ./...
```

`nested/` is a second Go module used to demonstrate multi-module bumps, Chainlink-style. `.tool-versions` pins the local Go version (asdf), like the Chainlink repository.
