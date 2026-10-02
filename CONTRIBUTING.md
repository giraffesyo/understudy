# Contributing

## Branches

- `canary` is the only long-lived branch (and the repository's default):
  work lands there, and releases are cut from it. Each push to `canary`
  makes [release-please](https://github.com/googleapis/release-please) open
  or update a release PR; merging the release PR tags and creates the
  release, and GoReleaser then builds, signs and uploads its artifacts and
  container image (see [Releases](README.md#releases)).

## Commit messages: Conventional Commits

release-please computes the next version and writes
[CHANGELOG.md](CHANGELOG.md) from the commit subjects on `canary`, so they
follow [Conventional Commits](https://www.conventionalcommits.org/):

```
<type>[(<scope>)][!]: <summary>

[body]

[footers, e.g. BREAKING CHANGE: <what breaks and how to migrate>]
```

The scope is the area the commit touches, the same word the history's
`area: summary` subjects started with: `template`, `pyre`, `yaml`,
`modules`, `e2e`, `cli`, ... For example:

```
feat(template): dig lookup on a port of dnspython
fix(template): dict keys that are not strings stay themselves
perf(yaml): scan plain scalars without backtracking
test(e2e): golden coverage for include_vars first_found
docs: README install section
```

| Type       | Use for                                    | CHANGELOG section        | Version bump (pre-1.0)        |
|------------|--------------------------------------------|--------------------------|-------------------------------|
| `feat`     | new behavior users see                     | Features                 | minor (0.1.0 -> 0.2.0)        |
| `fix`      | a bug fix (incl. fidelity fixes)           | Bug Fixes                | patch (0.1.0 -> 0.1.1)        |
| `perf`     | a speedup                                  | Performance Improvements | patch                         |
| `revert`   | reverting an earlier commit                | Reverts                  | patch                         |
| `docs`     | documentation only                         | Documentation            | patch                         |
| `build`    | Makefile, GoReleaser/Dockerfile, toolchain | Build System             | patch                         |
| `refactor` | code change with no behavior change        | (hidden)                 | none on its own               |
| `test`     | tests, golden corpus, harnesses            | (hidden)                 | none on its own               |
| `ci`       | workflows, CI configuration                | (hidden)                 | none on its own               |
| `chore`    | anything else                              | (hidden)                 | none on its own               |
| `style`    | formatting                                 | (hidden)                 | none on its own               |

A `!` after the type or scope (`feat(cli)!: ...`), or a `BREAKING CHANGE:`
footer, marks a breaking change; before 1.0 that bumps the minor version
(`bump-minor-pre-major`), afterwards the major. Dependabot's PRs use
`fix(deps)` for Go modules (they ship in the binary), `ci(deps)` for
actions and `test(deps)` for the golden suite's ansible-core.

The **Conventional Commits** workflow checks every PR's title (what a
squash merge commits) and each non-merge commit in it (what a merge commit
brings along). Check a branch locally before pushing:

```sh
.github/scripts/conventional-commits.sh range origin/canary..HEAD
.github/scripts/conventional-commits.sh title "fix(template): ..."
```

Merge commits are not checked and do not appear in the changelog.

## Before sending a change

```sh
make build test                 # unit + local end-to-end tests
gofmt -l .                      # must print nothing
go vet ./...
make test-golden                # with ansible-core installed (see README)
make test-e2e                   # with Docker
```

Changes to `.goreleaser.yaml` or the `Dockerfile`: run
`goreleaser check` and `goreleaser release --snapshot --clean --skip=sign`
(GoReleaser v2, the version pinned in `.github/workflows/release.yml`; the
images need Docker with buildx), then `make clean` or `rm -rf dist`.

CI also runs `go mod verify`, staticcheck, govulncheck, the race detector,
the cross-compile check, `goreleaser check` with a snapshot release build,
CodeQL, and the golden and Docker suites; see
[.github/workflows/ci.yml](.github/workflows/ci.yml) for the pinned tool
versions (staticcheck and govulncheck run with `go run module@version`
and stay out of `go.mod`). The module takes no third-party Go
dependencies: the standard library and `golang.org/x` only.
