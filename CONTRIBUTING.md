# Contributing to GPUWorks

English | [简体中文](CONTRIBUTING.zh-CN.md)

Thanks for considering a contribution. GPUWorks is a GPU cluster manager: it connects GPU
hosts, deploys vLLM / SGLang inference services, and exposes an OpenAI-compatible gateway.
It talks to hardware, container runtimes and vendor stacks, so a good report or a small
fix in the right place is worth more than a large patch that nobody can verify.

This guide covers the mechanics. For how the code is organised, read
[docs/dev/README.md](docs/dev/README.md) and [docs/dev/code-map.md](docs/dev/code-map.md).

- [Code of conduct](#code-of-conduct)
- [Before you open anything](#before-you-open-anything)
- [Ways to contribute](#ways-to-contribute)
- [Development setup](#development-setup)
- [Build and test](#build-and-test)
- [Coding conventions](#coding-conventions)
- [Documentation conventions](#documentation-conventions)
- [Commits and pull requests](#commits-and-pull-requests)
- [Security](#security)
- [License](#license)

## Code of conduct

Everyone participating in this project is expected to follow
[COMMUNITY_GUIDELINES.md](COMMUNITY_GUIDELINES.md). Report unacceptable behaviour to the address listed
there, not in a public issue.

## Before you open anything

1. **Search first.** Existing issues, closed issues, and `docs/how-to/troubleshooting.md`
   already cover a large share of the questions we receive.
2. **Check the version.** Almost every bug report needs the output of
   `curl -s http://<server>:8800/healthz`. Releases are named `dev-<commit>`; without the
   commit we cannot tell whether the bug was already fixed.
3. **Reproduce on a clean install if you can.** Behaviour that depends on leftovers from a
   previous install is hard to act on.

## Ways to contribute

| You have | Where it goes |
|---|---|
| A defect | Bug report issue form |
| A usage question or a documentation error | Question / other issue form |
| A security vulnerability | [SECURITY.md](SECURITY.md) — **never a public issue** |
| A patch | Pull request, see below |
| A hardware result (a GPU model, driver, runtime combination that works or does not) | Issue with the environment section filled in; this project ships a support matrix and needs evidence to update it |

Hardware evidence matters here. If you report that a combination works, include the exact
driver / CUDA / CANN versions, the container runtime, and what you ran. Claims without
evidence cannot go into [docs/reference/support-matrix.md](docs/reference/support-matrix.md).

## Development setup

Requirements:

- **Go** — the version in `go.mod` (currently Go 1.26.11, matching the CI image `golang:1.26.11`).
- **Node.js and npm** — versions follow `ui/package.json` and the lockfile.
- **GNU Make**.
- **Docker**, SSH, and vendor CLIs are needed only for the container-level and cloud test
  layers, or for building the cross-compiled agents.

```bash
go version
node --version
npm --version

make build      # UI + embedded agents + server + agent into ./bin
make demo       # start a throwaway server, attach a GPU-less agent, check attach/reconnect
```

The local server listens on `:8800` by default and writes to `./data`. The admin password
for a fresh install is written to `data/initial-credentials`.

To run the backend and the frontend separately:

```bash
make run-server
cd ui && npm install && npm run dev
```

The dev server is for hot reload only. When you need to verify the embedded bundle, run
`make ui` and start the Go server instead — that is what ships.

Secrets (addresses, SSH parameters, cloud keys) belong in the git-ignored `secrets/`
directory. Never commit real credentials, and never paste them into an issue or a merge
request.

More detail: [docs/dev/setup.md](docs/dev/setup.md).

## Build and test

```bash
make test            # UI build + static checks + go test + frontend tests
make vet             # go vet
make test-integration  # product-level tests with fake agent/runtime
```

`make test` **does not** include the layered suites. Run them when your change touches
scheduling, deployment, agent runtime, or container behaviour — in those areas the layered
suites are not optional:

```bash
make test-l2         # multi-process: real server and agent binaries, fake GPUs
make test-l3         # container-level, needs a local Docker daemon
```

Before you push, the full round is:

```bash
make test
go test -race -timeout 40m ./<package you touched>/...
go vet ./...
git diff --check
```

Notes that cost people time if skipped:

- The `-timeout 40m` is deliberate. Under `-race` the large server packages take far longer
  than the default 10 minutes (store ≈ 17 min, controller ≈ 16 min in our measurements). A
  timeout panic means the run did not finish: it is not evidence that the change is wrong,
  but it is not a pass either. Keep the log, find out why it hung (deadlock, leaked goroutine,
  over-subscribed machine) and count the change as verified only after a clean re-run.
- Lint is configured in [.golangci.yml](.golangci.yml) (`govet`, `errcheck`, `staticcheck`,
  `unused`, `ineffassign`, `misspell`, `gofmt`, `goimports`, `revive`, `bodyclose`, `nilerr`).
  Run `make lint` before submitting; CI runs the same target. It builds
  golangci-lint v1.64.8 with the repository's Go version into `bin/tools`,
  preserving the complete rule set and reporting all findings. The first run
  downloads the development tool and builds the embedded UI if it is missing.
- `goimports` uses `local-prefixes: gpuworks.ai/gpuworks`, so local imports form their own
  group.
- The layered and cloud test strategy is described in [docs/dev/testing.md](docs/dev/testing.md).

## Coding conventions

The project favours a small number of explicit rules over style debates:

- **Keep files under ~600 lines.** Split before the file becomes a dumping ground.
- **Separate wire DTOs from store entities.** They change for different reasons.
- **Publish events after the transaction commits**, explicitly. Do not publish inside a
  transaction that can still roll back.
- **State the contract before the implementation.** Read the existing code, tests and ADRs
  first; then change the implementation and the tests at the same layer.
- **Do not mix unrelated changes.** One pull request should have one reason to exist.
- **Comment only where the code cannot say it.** A large comment explaining an algorithm
  usually means a function should be extracted and named instead.

If an existing pattern contradicts a rule above, follow the existing pattern and raise the
contradiction in the pull request rather than quietly forking a second convention.

## Documentation conventions

`docs/` is organised by intent, and something filed in the wrong place will be asked to move:

| Directory | What belongs there |
|---|---|
| `docs/tutorials/` | Learning from zero along the main path. No deep explanation. |
| `docs/how-to/` | Solving one task. No beginner teaching. |
| `docs/reference/` | Exact fields, matrices, protocols. No narrative walkthroughs. |
| `docs/explanation/` | Why it is designed this way. |
| `docs/adr/` | Irreversible architectural decisions, numbered `NNNN-*.md`. |
| `docs/dev/` | Development operations. |

The documentation is bilingual and paired: `docs/` holds the Chinese edition and `docs-en/` the
English one, page for page. A page change must land in both trees in the same pull request, or
the pair drifts — `make check-doc-links` verifies links, not that a pair matches. Code, API
fields and CLI output stay verbatim in both editions.

`make check-translations` (also part of `make test-unit`) checks more than the pairing: every
`docs-en/` page must match the *reviewed* SHA-256 of its `docs/` source, recorded in
`docs-site/translations.json`, and must carry the same headings and code blocks. Editing a
paired page therefore takes three steps, not two:

1. edit the Chinese page under `docs/`;
2. edit the English page under `docs-en/`, and review that translation against the change —
   a hash that says "reviewed" when nobody read the English text defeats the check;
3. record that review by recomputing the source hash, then run the check:

```bash
python3 - <<'PY'
import hashlib, json, pathlib

page = "how-to/connect-nodes.md"   # path relative to docs/, same key as translations.json
digest = hashlib.sha256(pathlib.Path("docs", page).read_bytes()).hexdigest()

path = pathlib.Path("docs-site/translations.json")
data = json.loads(path.read_text())
data[page] = digest
path.write_text(json.dumps(data, indent=2, ensure_ascii=False) + "\n")
PY
make check-translations
```

If a code block legitimately differs between the two editions (translated comments, a localised
example), record the reviewed pair in `docs-site/localized-code-blocks.json`; otherwise the
check reports a block that changed without review. A failure on a page your change does not
touch is a pre-existing gap: leave it to whoever owns that page instead of pasting the source
hash over it.

When documenting behaviour, read the code first. Flag tables in the reference manual must
match `--help` output. Relative links and anchors are checked by `make check-doc-links`
(part of `make test-unit`).

## Commits and pull requests

For branch setup, validation records, and review, follow the
[PR / MR workflow](docs-en/dev/pull-requests.md). Use the
[single PR template](.github/PULL_REQUEST_TEMPLATE.md), keeping its Chinese headings;
write the body in either English or Chinese.

- Branch from the current target branch, with a name that says what it does
  (`fix/pd-scaling-loop`, `docs/connect-nodes-typo`).
- Write commit messages that state the problem and the change, not just the file touched.
- In the pull request description, say:
  - what problem this solves and how you verified it;
  - which tests you ran, and which you did not;
  - anything that needs external acceptance (a specific GPU, a cloud account, a runtime)
    that you could not verify yourself.

"Not verified" is acceptable and useful. "Should work" is not — if a branch of behaviour is
untested, say so explicitly so a reviewer does not assume coverage.

CI runs static checks, the security gate, Go and UI unit tests, layered compile checks, and
packaging. A red pipeline is a blocker, not a formality.

## Security

Do not report vulnerabilities in a public issue, and do not include exploit details in a
pull request. Follow [SECURITY.md](SECURITY.md).

## License

GPUWorks is released under the **Apache License 2.0**. The full text is in [LICENSE](LICENSE).

By submitting a contribution you agree that it is licensed to this project under the same terms
(inbound = outbound), and you keep your copyright. You confirm that you have the right to submit
it — the code is yours, or its license permits contributing it here.

Two obligations worth knowing before you paste third-party code in:

- **Imported code must be compatible.** Apache-2.0 is not copyleft, but it is not compatible with
  GPL-2.0-only either. Do not copy GPL-2.0 code into this repository. If you want to bring in code
  under another license, raise it in the pull request before doing the work.
- **Keep notices intact.** Do not strip copyright, patent, trademark or attribution notices from
  files you modify, keep any `NOTICE` file intact, and state clearly that a file has been changed
  when you change it substantially (Section 4).

Apache-2.0 also carries an express patent license from contributors (Section 3), which is the main
reason projects choose it over MIT.
