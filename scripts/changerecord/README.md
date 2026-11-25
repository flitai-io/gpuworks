# Change-record gate

`make check-change-record` checks the committed feature-branch diff. Its trigger
list is `scripts/change-record-rules.txt`; tests are excluded. A contract change
requires an added or updated source document under `docs/`. Deleting a document
or changing only a translation does not satisfy it. Renames check both paths.

For a decision change, local preflight accepts an added or updated `plan.md` on
the matching branch of the independent record repository. Its YAML frontmatter
must contain non-empty `decision` and `rejected` strings. Historical plans that
were not changed on the branch do not count.

The GitLab MR pipeline always verifies an open companion MR on the same source
branch. It reads changed plans from that MR's source project at its immutable
head revision; a local checkout cannot bypass this check. Missing records,
credentials, denied access, invalid responses and truncated diffs fail the gate.
Default-branch, scheduled and release pipelines do not re-require an open MR
after it may have been merged.

A GitHub pull request run (`GITHUB_EVENT_NAME=pull_request`) is a merge gate
for contract changes: it diffs the pull request against `origin/$GITHUB_BASE_REF`
and fails when that base is missing (the checkout needs `fetch-depth: 0`).
Decision records are not verified there, because the companion MR is read
through the GitLab API; the run prints the decision paths it did not check and
passes. Other GitHub runs (push, manual, scheduled) are not the gate.

The first CI job runs the gate's CLI/HTTP regression tests and the check before
UI, documentation builds or other tests start. The CI graph check rejects jobs
that bypass this dependency. `make test` and `make test-unit` also wait for the
gate before starting their expensive prerequisites, including under `make -j`.

The gate's CI job sets `GIT_DEPTH: "0"` so the MR diff base remains available
even on branches longer than the runner's default shallow history. A missing
base causes `git diff` to fail before any documentation checks run.

## Credentials

GitLab 14's built-in job token cannot read MR changes. Set the masked
`CHANGE_RECORD_ARTIFACTS_TOKEN` variable in the product project, scoped to the
`change-record` environment. Use a project access token with only `read_api` scope in the record project.
Select Reporter access when the server supports it. GitLab 14.0 creates its
project-token bot as Maintainer and cannot lower that role through this API;
the token must still have only the read-only API scope. Renew it before its configured
expiry; do not use a write-capable CI token or commit credentials. `GITLAB_TOKEN`
is also accepted for an explicit local remote check. Requests have a 20-second
timeout. Local preflight can use a matching independent checkout without network.

`CHANGE_RECORD_BASE` and `CHANGE_RECORD_BRANCH` select a local comparison;
GitLab MR base/source variables take priority. `CI_API_V4_URL` and
`CHANGE_RECORD_ARTIFACTS_PROJECT` select the GitLab API and record project.
