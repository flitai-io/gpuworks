# GPUWorks PR / MR workflow

[PR / MR template](../../.github/PULL_REQUEST_TEMPLATE.md)

Use this guide to prepare, validate, and submit a GitHub Pull Request (PR) or GitLab Merge
Request (MR). Write the description in English or Chinese; one language is enough. For project
requirements, read [CONTRIBUTING.md](../../CONTRIBUTING.md), the
[development rules](../../CLAUDE.md), and the [testing guide](testing.md).

Before starting, prepare a local checkout, access to a fork or writable working branch, and the
[development environment](setup.md). Run the commands below from the repository root.

## 1. Define the problem and scope

Search existing issues, PRs/MRs, and documentation first. For a bug fix, describe the trigger,
actual behaviour, and expected behaviour. Discuss substantial features or architectural changes
with maintainers before implementation. Keep each PR focused on one problem; avoid unrelated
formatting, dependency upgrades, or fixes.

Report vulnerabilities privately through the channels in [SECURITY.md](../../SECURITY.md).
Do not publish exploit details in a public PR.

## 2. Create a working branch

These commands assume `upstream` points to the project repository and `origin` to your fork.
For a direct contribution, use `origin` in place of `upstream`. Replace `TARGET_BRANCH` with the
branch selected for this PR on the hosting platform, and choose a descriptive working branch
such as `fix/agent-reconnect`, `feat/model-filter`, or `docs/node-setup`.

```bash
git status --short
git remote -v
```

Check that the remotes are correct and preserve any uncommitted work before proceeding. Do not
reset or clean away someone else's changes. Then create the branch from the current target:

```bash
git fetch upstream
git switch -c fix/agent-reconnect upstream/TARGET_BRANCH
```

## 3. Implement and validate

Read the relevant code, tests, and ADRs first. A regression test should expose the original
problem and pass with the fix. Cover relevant failure, timeout, cancellation, and boundary
paths. Check wire JSON contracts for API changes, and user interactions, states, and keyboard
operation for UI changes.

Update affected documentation. Chinese pages live under `docs/`, with English translations at
the same relative paths under `docs-en/`. Review both editions and update the source hashes in
`docs-site/translations.json`, following [CONTRIBUTING.md](../../CONTRIBUTING.md#documentation-conventions).

Run the project's pre-submission checks; replace the package path with the packages changed:

```bash
make test
go test -race -timeout 40m ./path/to/changed/package/...
make vet
git diff --check
```

`make test` includes the UI, static checks, and default Go suites, but not L2/L3. Changes to
scheduling, deployment, agent runtime, or container behaviour also require `make test-l2` and
`make test-l3`. For verbose output and an explicit L3 timeout, run:

```bash
make test-l2
make test-l3 GO_TEST_FLAGS='-count=1 -v' L3_TIMEOUT=40m
```

Check the output for skipped cases. Docker being unavailable can leave container tests skipped
with exit code 0; that is not an L3 pass. `L3_TIMEOUT` controls the package timeout in this target;
it does not replace an external job deadline or resource limits.

Use a dedicated test environment and an explicitly selected, authorized container runtime.
A separate Docker socket or data directory does not isolate host networking; do not start a
second daemon in a shared host network namespace. Run L4 hardware or cloud checks with the
required resources and authorization. Use dedicated workers for expensive tests, with memory,
swap, concurrency, and runtime limits appropriate to the host.

Record the tested commit, exact commands, environment, exit codes, skipped cases, and redacted
evidence. Use `PASS`, `FAIL`, `SKIP`, `NOT_RUN`, or `N/A`, explaining any non-pass result.
A timeout is an incomplete run, not a pass. Missing hardware is an unrun check, not an
inapplicable one. Mark a check `N/A` only when it does not apply to the change, and give the
reason; the template does not waive project requirements.

For GPU, image, or model changes, record the GPU model and count, CPU architecture, driver,
vendor stack, container runtime, image reference and digest, and model revision where applicable.
Report image builds, CPU verification, real GPU inference, and Web validation separately.
Evidence for one stage or image digest does not establish that another passed.

## 4. Review the diff and commit

Keep each commit focused and independently reviewable. Explain the problem, why the chosen fix
works, and any relevant compatibility trade-offs in its message. Stage only files belonging to
this change; replace the paths below with the files you actually edited:

```bash
git diff --stat
git diff
git add path/to/file path/to/test
git diff --cached
git diff --cached --check
git var GIT_AUTHOR_IDENT
git var GIT_COMMITTER_IDENT
```

Confirm the staged diff contains no credentials, private connection details, model weights,
large build outputs, or unrelated changes. Check your name and email before committing:

```bash
git commit -s
git log -1 --format=full
```

`-s` adds your `Signed-off-by` trailer; it is not a cryptographic signature. Sign only for
yourself, preserve other contributors' attribution, and check authors and trailers after
amending, rebasing, or applying patches. Recheck the staged diff for every additional commit.

## 5. Open the PR / MR

Push the working branch to your fork or authorized repository:

```bash
git push -u origin fix/agent-reconnect
```

Select the correct target repository, target branch, and working branch. Use Draft while
implementation or validation remains incomplete. GitHub uses the
[single PR template](../../.github/PULL_REQUEST_TEMPLATE.md) by default once it is merged into the
repository's default branch, as described in the [GitHub template documentation](https://docs.github.com/en/communities/using-templates-to-encourage-useful-issues-and-pull-requests/creating-a-pull-request-template-for-your-repository).
The template keeps three Chinese headings: 问题 (Problem), 验证 (Validation), and 说明 (Notes).
Instructions are in HTML comments; there is no checklist. Write the body in Chinese or English.
On GitLab, paste the same template into the MR description.

Use a title that states the outcome, such as `Fix agent reconnection after a temporary connection
failure` or `修复 agent 在临时断线后无法重连的问题`. Lead with the problem and resulting behaviour,
then explain the approach, verification, and limitations. Use `Closes #123` only for a fully
resolved issue on the hosting platform; for partial work, reference the issue and list what
remains. For an external tracker, include its issue key and a link accessible to reviewers.
Fill in the three sections with actual details; write "None" if there are no additional notes.

## 6. Address review and complete the merge

Respond to each review comment with the change made or the reason for retaining the approach.
When scope changes, update the title and description around the final implementation. After new
commits or conflict resolution, rerun affected checks and update the tested commit and evidence.
Coordinate history rewrites with anyone sharing the branch.

Keep failure logs with their associated commits. For a suspected pre-existing failure, compare
against the target branch in the same environment and report both results. A later passing run
does not erase a previous failure, and a failing required check is not a pass.

Before merging, confirm review feedback is addressed, required checks pass, and any remaining
external acceptance has a named next step. An authorized maintainer decides whether the change
is ready and merges it under repository policy. Include required user documentation, upgrade
instructions, and rollback limitations with the change.
