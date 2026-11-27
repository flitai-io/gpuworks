# Contribution Guide

## Development Workflow

1. Create a narrowly scoped branch from the target branch; do not include unrelated changes.
2. Confirm the contract from the code, tests, and current ADRs before changing the implementation and tests at the same layer.
3. Split files that exceed 600 lines; separate DTOs from store entities, and explicitly publish events after the transaction commits.
4. Run the pre-commit checks required by [`CLAUDE.md`](../../CLAUDE.md), then submit an MR and state which branches of behavior remain uncovered and the status of external acceptance testing.

## Documentation Locations

- Put tutorials in `docs/tutorials/`: teach the main workflow from scratch without exploring underlying principles.
- Put how-to guides in `docs/how-to/`: solve one task without repeating introductory teaching.
- Put precise fields, matrices, and protocols in `docs/reference/`: do not write narrative operating procedures.
- Put design rationale in `docs/explanation/`; document irreversible tradeoffs separately in `docs/adr/NNNN-*.md`.
- Put development procedures in `docs/dev/`; put internal living ledgers in `docs/dev/ledgers/`.
- Put one-off plans, acceptance records, audits, and real-hardware execution records in `artifacts/`: use `work/<YYYY-MM-DD>-<topic>/` while work is in progress, then move
  the entire topic directory to `archive/<YYYY-MM-DD>-<topic>/` when it is complete. Put independently completed research in `research/<YYYY-MM-DD>-<topic>/`;
  record status only in the plan file's own YAML frontmatter. See `artifacts/README.md` for the index.

When adding, moving, or deleting an article, update the [documentation index](../README.md). Follow its existing categories and ordering source rather than defining a separate order.
Explain changes to categories, paths, or article types, and check existing links.

Keep the Chinese originals in `docs/` and the English translations at the same relative paths in `docs-en/`; leave code, API fields, and CLI output unchanged. Current documentation must not be tied to M numbers or depend on
relative paths outside the repository. Do not add private artifacts references to content for public readers; put design rationale in `docs/` or ADRs. The root
[`CONTEXT.md`](../../CONTEXT.md) is the sole complete definition of terminology; the user glossary is only a subset.

English translations retain the original sections, steps, restrictions, and examples; do not replace full content with summaries. Prefer terminology from the GPUWorks English UI;
translate descriptions and image alt text without changing commands, paths, parameters, or sample values. You may reuse existing Chinese screenshots, but the English text must state the screenshot language.
After a source document changes, review its translation, then run `python3 docs-site/check_translations.py --write docs/<path>.md` to record the SHA-256 of each reviewed source section at the end of the English page; do not refresh hashes without checking the translation.
Run `python3 docs-site/check_translations.py` to check for stale translations and code block changes; also use `--require-complete` for site-wide English acceptance checks.
The hash at the end of an English page and `internal/server/aihelper/apidocs_gen.go` are both derived content. When a merge or rebase conflicts there, do not merge them by hand: resolve the conflicts in the Chinese and English text first, then run `make api-docs` (for other pages, the `--write` command above) to regenerate them, and `git add` the results.

## Writing Conventions

Organize content by its purpose: tutorials and how-to guides help readers complete tasks, explanations clarify concepts and mechanisms, and references make information easy to look up.
Manual editing and automated maintenance follow the same writing conventions. Below, “Required” denotes acceptance requirements; “Recommended” denotes approaches you may adapt to the page's content.

### Content Structure

How-to guides and tutorials must explain the intended outcome, applicable scenarios, and prerequisites before the procedure so readers can first decide whether the guide applies and what they need to prepare.
You may use the following opening template. It is an information checklist: choose paragraphs, lists, or tables to fit the content, without copying it word for word:

```markdown
# Article Title

Describe the task and the intended result in one sentence.

**Use cases**: Explain which needs or problems this article addresses, and state any scope limitations.

## Prerequisites

- Required permissions and nodes, models, or services that are already prepared.
- Version, hardware, and network conditions that determine whether you can start; list only those actually required by this article.
```

If the title or introduction already explains the outcome and scenario, do not add redundant labels. Existing equivalent sections such as “Before You Begin” or “Preparation” may remain.
If a prerequisite fits in one sentence, put it directly in the opening. Use a list for multiple independent prerequisites, or a table when readers need to compare configurations item by item; do not create an empty section when there are no additional prerequisites.
Place shared prerequisites first. Put conditions needed by only one procedure at the start of that procedure instead of making them requirements for the entire article.
Each document type must cover the following content, without repeating information from the opening or copying every section title.

| Type | Required Coverage |
|---|---|
| Tutorial | A clear outcome, prerequisites, a continuous sequence of actions, and final verification |
| How-to guide | Applicable scenarios, necessary prerequisites, steps to complete a specific task, and result checks; explain how to handle known blockers |
| Reference | Scope, and terms, fields, or capabilities organized by topic; explicitly state defaults and restrictions where they exist, with examples where needed |
| Troubleshooting guide (a type of how-to guide) | Symptoms, checks, remedies selected according to check results, and verification of recovery |

Build a tutorial around one clear outcome. Use concrete, reproducible examples, follow a sequence of steps through the main workflow, and end by verifying that the outcome is achieved.
Organize how-to sections and steps around the actions readers need to perform, rather than backend modules or implementation processes. When several procedures are available,
first explain when to choose each, then give their key actions and completion criteria separately. Keep parallel methods for the same task within the article's scope in the main text; link to specialized guides for detailed parameters or preparation.
A recommendation for one method must be grounded in the article's goal or product behavior. It must not lead you to omit key steps for other in-scope methods or replace them with links alone.

Keep information that affects method selection, execution, or result assessment in the main workflow of tutorials and how-to guides, including necessary prerequisites, service interruption impacts, and deletion scope;
place explanations immediately after the relevant action. Metric definitions and measurement rules, state meanings and transition conditions, the behavior of options once applied, and the results and limitations of automatic handling
are information users need to understand the product and assess outcomes. Explain them in the relevant main text; do not reduce them to names, conclusions, or reference links alone.

After retaining essential explanations, you may link details such as internal modules, data structures, complete algorithms, and protocol implementations to existing references or explanations. Confirm that each target actually covers the content before linking.
If no existing explanation is available, implementation details useful for deeper troubleshooting may go in a supplement at the end or a collapsible section; required meanings, conditions, actions, risks, and assessment criteria must not be moved into collapsible content.

Examples in references explain fields or protocols; do not expand them into narrative operating procedures.
Keep API route lists in tables grouped by resource so readers can find an API first. For simple APIs, “method, path, purpose, authentication” is sufficient;
add the parameters and return content needed for the call in the description. Refer to the compact table in the [LiteLLM Model Management API](https://docs.litellm.ai/docs/proxy/model_management#automation-api)
and the short description of the [llama.cpp model loading API](https://github.com/ggml-org/llama.cpp/blob/master/tools/server/README.md#post-modelsload-load-a-model) for examples.

- Scale the information to the API's complexity; do not mechanically attach parameter tables, complete response field tables, curl examples, and error code tables to every route.
  State where required inputs belong. Retain defaults, allowed ranges, null semantics, and special permissions whenever they affect the call.
- Add examples only when the request shape or result is not intuitive. Use the smallest valid request and a short response; explicitly mark omitted fields, and do not present an illustration as a complete structure or a record of an actual test.
- For return values, prioritize status, metric definitions, asynchronous results, and easily misunderstood fields. Do not repeat explanations for each field whose name is already clear,
  or repeatedly expand the same object in list, detail, and action responses.
- Define authentication, pagination, and common errors centrally; each API adds only its differences. Put longer but necessary explanations in short sections under the same resource and link to them from the table;
  reference algorithm or protocol details only when existing documentation actually covers them. Brevity must not remove deletion scope, service interruptions, state changes, restrictions, or the information needed to handle errors.
- Keep package names, internal execution steps, and change history that callers do not need out of route descriptions; place maintenance notes at the end.

Do not make exception paths, alternatives, resource cleanup, or supplemental mechanisms mandatory sections; explain them as needed when relevant to the current goal, without adding empty sections.

### Steps and Examples

**Required:** Write the main workflow of tutorials and guides with procedures in the actual order of execution. For key steps, state where to act, what to do, and how to confirm completion.
For UI actions, name the entry point and button; for commands, state the machine, terminal, and necessary working directory.

- Within the article's stated scope, explain the triggers and handling for exception paths and alternatives that affect whether an operation can continue, which method to choose, or how to assess the result.
  Place short branches immediately after the relevant step, and explain conditions that affect whether readers can begin before execution. Longer preparation or exception handling may link to existing how-to or troubleshooting guides;
  retain key steps for parallel methods of performing the current task as described in “Content Structure” above.
  When the primary method is unavailable under an applicable condition, provide a verified alternative and explain when to use it. You need not cover every situation outside the article's scope.
- State the permissions, hardware, models, versions, and network conditions actually needed for the scenario; explain where to obtain required placeholder values and where to replace them.
- Use the same resource names and parameters for creation, configuration, invocation, and verification. When cleanup instructions are needed, match the creation method and state which objects are deleted and which are retained.
- Give each step one intermediate goal. Put optional actions after the relevant main step, and state conditions that affect whether readers can continue before execution.
  Describe automatic system actions as explanations or expected results of the step, rather than as steps users must perform.
- State the preparation needed for commands, and label code blocks with their language. Separate commands from output and omit terminal prompts. Identify the key output to check,
  without requiring a match for variable values such as random IDs or timestamps. If readers must check results before continuing, separate the commands before and after that check and state the condition for proceeding;
  show alternative commands separately and identify which step they replace so readers do not assume they must run all of them.
- Read the code before describing specific behavior such as error codes, flags, defaults, or ports; base flag tables in references on `--help` output.
- Make the final verification match the tutorial's goal. For example, a tutorial on deploying and calling a model must verify a successful response, rather than only checking that the instance is “Running”.
- Do not present unverified combinations as known to work, or label illustrative input and output as actual verification results.

### Screenshots and Flowcharts

**Required:** User tutorials include illustrations that help readers follow steps and check results. For UI operations, screenshots cover the entry point, key configuration, and completed result.
Adjacent actions may share a screenshot; a screenshot for every click is not required.

Illustrate other document types as their content requires: add screenshots to how-to and troubleshooting guides when entry points are difficult to locate, configuration choices are easy to get wrong, or results need visual recognition;
explanations and references do not require illustrations, and need no extra images when the text is already clear. Use flowcharts to clarify relationships or processes, not as a mandatory feature of every document.

- Store screenshots in `docs/assets/screenshots/` and reference them with `![Description](../assets/screenshots/xxx.png)`. Use the Chinese UI,
  a resolution of at least 1440×900, and avoid exposing real addresses, tokens, or hostnames. The published site uses `docs-site/stage_docs.py` to copy images and rewrite their paths for the site.
- Place each image immediately after the corresponding step. Its caption or alt text explains what to check; the text must still specify paths, buttons, inputs, and expected results instead of replacing steps with “follow the screenshot”.
- Use consistent versions, scenarios, and resource names in screenshots and text. Configuration images show key values after the example has been filled in, and completion images show the state actually reached.
- Capture screenshots from the actual UI; do not pass off prototypes, fabricated states, or other scenarios as operation results.
- Provide commands, error messages, and key output as copyable text; CLI procedures do not require terminal screenshots.
- If a screenshot is required but unavailable, list what remains to be added in the review record; do not use placeholder images to imply completion.
- Use ```mermaid code blocks for flowcharts (both GitHub and the published site can render them). Enclose node labels containing parentheses or colons in double quotes.

### Style and Layout

**Required:** Start steps with actions such as “Open”, “Select”, “Enter”, “Click”, or “Check”; match UI names to the actual labels.
Bold buttons, menus, and field labels. Use code formatting for commands, paths, parameters, and API fields. Briefly explain essential terms on first use,
consistent with existing definitions. Use numbered lists for operation sequences; choose a format for field mappings and conditions from the table below.

**Recommended:** Choose a form that suits the content:

| Content Characteristics | Recommended Format |
|---|---|
| Brief background, purpose, or a single condition | Short paragraph |
| Multiple independent prerequisites or notes | Bulleted list |
| Actions that must be performed in order | Numbered steps |
| A workflow with several distinct stages | Stage subheadings, with the appropriate steps under each |
| Multiple fields to fill in individually, or multiple options to compare along the same dimensions | Table |
| Commands, configuration, requests, and output | Code blocks labeled with their language; separate commands from output |
| A short conditional branch within a step | An “If …, then …” explanation immediately after that step |
| A longer alternative workflow that can stand alone | Separate sections for parallel methods of the current task; specialized guide links for preparation or exception handling |

Prefer the simplest form that conveys the content clearly. Do not cram sequential actions into tables or split a condition that fits in one sentence into multiple sections.
Retain an existing format when it is clear and complete; do not reorganize or rewrite it just to impose a uniform template.
Allocate table widths according to column content, keeping name and metric columns legible. Allow horizontal scrolling on narrow screens rather than forcing everything into one screen by wrapping every character, shrinking text, or deleting explanations.
Keep each paragraph focused on one point, and reserve callouts for important conditions that affect operations.
Avoid phrases such as “very simple” or “just” in place of necessary steps. Do not impose uniform targets for word count, screenshot count, or section count.

### Updates and Acceptance

**Required:** When editing existing documentation, make decisions in the following order:

1. **Confirm the content scope.** Retain tasks, methods, prerequisites, compatibility restrictions, and result criteria from the original that remain valid.
   If a feature change or explicit scope adjustment requires additions or deletions, explain the basis; do not interpret “streamlining” as reducing applicability or choosing methods for readers.
2. **Assess the extent of changes needed.** Retain clear, accurate text. Fix layout problems through layout alone; change the relevant parts when content is incorrect, missing, or unclear.
   If a how-to guide mainly discusses internal implementation, or explanations interrupt the procedure, reorganize the relevant text around the actions while retaining accurate commands, necessary branches, product logic, and result criteria. Do not expand unrelated content.
3. **Remove redundancy.** Delete repeated conclusions, merge repeated explanations, and reference or supplement internal implementation details as described in “Content Structure”.
   Do not shorten content by deleting still-valid usage, prerequisites, or success criteria, or turn automatic backend behavior into steps users must perform.
4. **Choose the presentation.** Once the content is settled, choose paragraphs, steps, sections, or tables according to “Style and Layout”. Formatting serves the content; do not rewrite the entire article for a uniform template.

Do not repeatedly rewrite completed documents with no new issues simply because the rules become more detailed. This principle also applies to automated maintenance.

**Required:** When behavior changes, check affected summaries, steps, tables, examples, screenshots, and limitations together and replace obsolete explanations;
do not merely append a new rule at the end while retaining outdated conclusions earlier. Preserve structures that remain correct and unrelated content.

- Keep one complete definition of each fact, and refer to it from other pages through necessary summaries and links. When updating a definition, check whether referring pages also need changes.
- Run `make check-doc-links` (already included in `make test-unit`) to validate relative links and `#anchors`, and reject `[[wiki]]` references under `docs/`.
  Links under `docs-en/` are checked against the anchors the site assigns: an English page's headings keep the Chinese page's anchors (`preserve_heading_ids` in `docs-site/stage_docs.py`), so a link names the Chinese heading's anchor, `#认证` rather than `#authentication`.
  Also check image references and example syntax. Check separately that repository files exist and that published-site paths are valid. For presentation changes, check the build and preview, including name columns, wrapping, and horizontal scrolling in narrow-screen tables.
- Review content against the original before editing to assess accuracy, scope completeness, and reading burden. For tutorials and how-to guides, check each of the following:
  whether the opening provides enough information to determine applicability, the intended outcome, and prerequisites; whether any necessary preparation is missing or a condition for one path has been expanded to the whole article;
  whether still-valid usage has an entry point and key steps; whether prerequisites and compatibility restrictions are retained; whether scope adjustments have a basis; whether a default path has been imposed without justification;
  whether unnecessary actions or navigation have been added, whether backend behavior has become user steps, and whether readers can prepare inputs, perform the procedure continuously, and determine success.

  For content deleted, collapsed, or replaced with links, check item by item that original metric definitions, state meanings, automatic behavior, and restrictions remain sufficiently explained in the main text. Checking only whether steps are retained is not enough.
  Assess required branches by their triggering conditions and handling, rather than by the presence of fixed exception, alternative, or cleanup sections;
  factual correctness, reduced word count, formatting changes, or a passing build alone do not establish that a revision is acceptable.

  For explanations, check that concepts and reasoning are clear; for references, check accuracy and ease of lookup. Check that any illustrations and examples agree with the text.
  For API lists, also check that methods, paths, and authentication remain consistent with the original, that parameters and restrictions remain discoverable, and that references cover the information moved out;
  simple APIs must not become tutorials, and complex explanations must not be crammed into table cells. GET route descriptions in `api.md` also generate the AI assistant's API catalog.
  After changing them, run `make api-docs` to synchronize the catalog and run consistency tests. Route summaries retain the purpose, required query parameters and valid values, and key fields for locating the API, rather than merely saying “see link”.
  `search_api` does not read linked content, and its returned summaries are truncated. When shortening GET descriptions, also verify that typical fields retrieve the corresponding API and that values required to call it still appear in the summaries actually returned.
- Verify the relevant workflow when adding or substantially changing an operational path. Wording-only changes do not mechanically require rerunning every environment; this does not replace the repository's required pre-commit checks.
  Record versions, environments, results, and uncovered areas in the MR or existing acceptance record, and store evidence according to repository conventions.
- Record code checks, page inspections, and actual operations separately. Do not describe passing static checks or builds as passing real operation tests, or mark unverified areas as having completed acceptance testing.

## ADRs

Before adding an ADR, confirm that the issue is a long-term architectural tradeoff. Use the current highest number in `docs/adr/` plus one, record the date,
status, context, decision, consequences, and supersession relationships, and update `docs/adr/README.md`. Link implementation documentation to the ADR;
do not copy the full decision into multiple guides.

<!-- translation-source-sha256: 13ceddc133eeeebb1ee4e00fb7a72628e57da3a3e68153fbedcfa120a6a39198 # Contribution Guide -->
<!-- translation-source-sha256: 956930905dd52c9343a6383f6bf7ef29839c04c3068abe8252102cf5753374f0 ## Development Workflow -->
<!-- translation-source-sha256: 60b57fe84985aaae16f7f527ddd8c2a625a9f8a16e2517b74ac0830d1f1f11f4 ## Documentation Locations -->
<!-- translation-source-sha256: 37e2573e7e41bf5c0a843de622d09509701243b77774747000ffe46378be54a8 ## Writing Conventions -->
<!-- translation-source-sha256: 44c1635d382e2dd5a4befaffcd9ba9d4eeb5feed65d1bd68d1a7b480e8fe5dc3 ### Content Structure -->
<!-- translation-source-sha256: 1dab26b020dd412bfa8a4c04ad872cee1a573c1b44817c13912f28f1a59507a0 ### Steps and Examples -->
<!-- translation-source-sha256: e17cae7ca53ddb457c4327048918c86dd0514e1d35ddfbb679943e0185382eb4 ### Screenshots and Flowcharts -->
<!-- translation-source-sha256: b7ac90ee96175ebe50f50f5b5e4b27d68eb34f634f3c620266330677aad37268 ### Style and Layout -->
<!-- translation-source-sha256: 067603fe1d7ca5f9e61a70f0b672c48d58b3af1890bf46ab20decb1def9bf609 ### Updates and Acceptance -->
<!-- translation-source-sha256: 3c68ce975e086f7e908b848f686e503cf42aa33d4a441d6139fc67c61157f14d ## ADRs -->
<!-- translation-localized-code-block: 6ac2188471df1d13d3f555100a0be8446e1e7e797f8484dd9a8478a88ab5e2ff b5c81a401976b96af4b52f6d68e1178451469f2e31988ec96e9ef86319c56f5b Reviewed English writing-template prose: present-tense wording only; heading levels, placeholders, prerequisites and executable examples are unchanged. -->
