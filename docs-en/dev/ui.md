# UI development

Vite builds `ui/` into `internal/server/webui/dist`, embedded in the Go server.
The console uses React 19, Tailwind 4 and the Zeron design system alongside
antd 5 while pages migrate one by one. The shell, form layer, shared components
(page header, resource table, detail drawer), `/batches` and `/dashboard` are on
Zeron; other pages still render antd inside them. New code uses Zeron and
`ui-kit/`. Preserve APIs, permissions, routes and server-owned rules.

## Directories and public APIs

- `ui/src/components/ui/`, managed `lib/` and `hooks/`: CLI-owned Zeron sources;
  provenance and transformed hashes live in `ui/.zeron/install-state.json`.
- `ui/src/ui-kit/`: project compositions: SideDrawer, NumberInput, FileUpload,
  ConfirmPopover, Spinner, Progress, Segmented, Timeline and Result.
- `ui-kit/form/`: react-hook-form controllers connected to Zeron Field; caller
  schemas use zod and server dictionary limits. `formSubmit` focuses and scrolls
  to the first error. Set `shouldFocusError: false` to use this ordering.
- `ui-kit/feedback/`: imperative message/confirm bridge and a single FeedbackHost.
  Migrate callers deliberately; this is not the entire antd imperative API.
- `components/PageHeader`, `ResourceTable`, `detail/DetailDrawer`, `EmptyState`:
  shared page structure. ResourceTable uses Zeron Table and useDataTable because
  the pinned DataTable renderer lacks row props and resizable header slots.
  Its temporary antd **types** adapt existing callers; query and selection state
  remain owned by callers. Do not add unsupported legacy options silently.
- `ui/src/pages/`: business compositions; `theme/`: Zeron-derived antd/ECharts
  values; `i18n/locales/{zh,en}/`: text, with `ui/i18n/glossary.json` terminology.

## Pinned source and synchronization

Zeron source: https://github.com/Carlosfengv/zeron-ui at
`1e036c58957a752bbd3ed61866f4616118a7dc28` (CLI 0.2.0-beta.18). The pin lives
only in `ui/src/theme/zeron/source.json` (`commit`, `repository`).
Do not modify managed sources. Product adaptations belong in `ui-kit/` or pages.

Install a component (git, Node >= 20.18.1 and npm needed; first run fetches):

```bash
scripts/zeron-registry.sh add <names...> --dry-run   # preview files and dependencies
scripts/zeron-registry.sh add <names...>             # install into ui/
scripts/zeron-registry.sh check                      # CLI doctor + managed file hashes
```

The script fetches the pinned commit into
`${XDG_CACHE_HOME:-~/.cache}/gpuworks/zeron/<commit>` (git's own
`http.https://github.com.proxy` is honoured; `ZERON_GIT_URL=<clone>` fetches
from an existing clone), verifies the commit, installs the CLI's dependencies,
and builds the Registry from that revision's committed `public/r` with
`registryDependencies` URLs rebased to `http://127.0.0.1:5211/r` and a
`manifest.json` of source and rebased hashes. `add` and `check` run a temporary
server on 5211 (or reuse one already serving the same revision) and stop it on
exit; `scripts/zeron-registry.sh serve` keeps one running. Keep port 5211:
install-state records the Registry base URL.

1. Check out the approved revision separately and read its swap-to-zeronui and
   zeron-page-builder skills. Review dependency closure and public APIs.
2. Install with `scripts/zeron-registry.sh add <names>` (dry run first).
   Resolve conflicts before installation; retain install-state provenance.
   Upgrading Zeron means a new pin in `source.json`, a reinstall and step 3,
   not editing managed files.
3. The CLI owns static `:root` and `.dark` tokens in `ui/src/app.css`. Run
   `node scripts/generate-zeron-tokens.mjs` to regenerate only TypeScript
   integration values. Never import upstream `tokens.css` or use `light-dark()`.
4. Run the theme consistency test (`ui/tests/zeronTheme.test.ts`) and the checks below.

Chrome 121 / Firefox 123 remain the browser floor. Verify the pinned production
CSS in both; do not infer compatibility from a modern browser. Hidden dark
preview sets both the HTML theme attribute and `dark` class. Charts already on
ECharts use Zeron chart-1…7 colors until they move to Zeron charts (see
"Choosing a component"). antd icons use `ui-kit/antdIcon` for Zeron icon slots.

## Verification

Per change, in `ui/`: `npx tsc -b`, `npx eslint .`, `npm run build` and the
related unit tests; from the root: `node scripts/i18n-check.mjs` and
`./scripts/check_architecture.sh`. CI runs the full suite.

**Unit tests** (`ui/tests/*.test.ts(x)`, React Testing Library under jsdom; run
by `npm test` in `ui/`, `make test` and CI's `core-ui-test` on every pipeline).
Each shared building block has its own test file: everything in `ui-kit/`,
`app/`, `components/detail/`, `components/resourceTable/`, and `PageHeader` /
`EmptyState`. The test imports it, or names its `ui/src/...` path when it drives
it through its real host (the sidebar through `App`). A page test that happens
to render one does not count; `ui_untested_shared_files` in `scripts/arch-constraints.txt` (max 0)
fails the architecture check and names any that has none. Change a building
block and its test in the same commit; add one with its test.

A test states behaviour the way a user meets it: by visible text, role,
accessible name and keyboard (Tab, Enter, Escape, focus returning to the
opener), and covers loading, empty, error and success. Do not snapshot, mock
Zeron or Base UI internals, or wait on fixed sleeps. Measure with
`npm run test:coverage` (`c8 --all`); a shared building block aims at 90% of
lines and 80% of branches, and a branch left untested is one the UI cannot
reach. Sizes, spacing, colours and overlay stacking are not unit-tested: jsdom
does no layout. The browser check below owns them.

When a test changes its expectation, the commit says why; a reviewer checks
that the behaviour change was intended rather than a test bent to pass.

**antd inventory** (`scripts/ui_antd_inventory.mjs`). How much antd is left, per
component, file and menu page; `ui/antd-inventory.json` is the committed
snapshot. `--check` (in CI's core-ui) fails when any file uses more antd than
the snapshot records, or a new file starts using it; after migrating, refresh
the snapshot with `--write` and lower `ui_antd_import_files` in
`scripts/arch-constraints.txt` in the same change. `--affected <files>` /
`--base <ref>` names the pages a change reaches (`["*"]` for shared files).

**Browser check** (`ui/tests/browser/ui-check.mjs`, run by
`scripts/ui_check_ci.sh` and the `ui-check` CI job). Walks a disposable
experience environment: every menu page, the first row's detail drawer and each
of its tabs, and each dialog the page header opens, at 1440 and 1024 in zh and
en. It fails on covered or overlapping text, controls stretched past their
content, controls an invisible layer blocks, sideways scrolling and console
errors. On a merge request it walks only the pages the change reaches, at 1024
only, and photographs the target branch's UI for the states that look different; the
report (`ui-check-report/index.html`, a job artifact) is for a reviewer to
confirm intended changes. Locally:

```sh
python3 -m venv "$HOME/.venvs/gpuworks-ui-check"
export UI_BROWSER_PYTHON="$HOME/.venvs/gpuworks-ui-check/bin/python"
"$UI_BROWSER_PYTHON" -m pip install -r ui/tests/browser/requirements.txt
scripts/ui_check_ci.sh                                    # every page
UI_CHECK_BASE_REF=origin/master UI_CHECK_COMPARE=1 scripts/ui_check_ci.sh
```

Local checks require Go, Node, Python Playwright and Chrome or Chromium.
Create the virtual environment and install its dependencies once. In later shells,
set `UI_BROWSER_PYTHON` to that interpreter. The template checks and page walk
share a browser from `PATH`, falling back to the installed stable Chrome channel.
Set `CHROME_PATH` to an absolute executable path to select a specific browser.

The check rebuilds the UI when sources are newer than the build, and refuses to
start when its ports (8899, 8900) are taken. A new page, drawer, tab or header dialog is covered without registering
it; flows behind other buttons (row menus, multi-step forms) are not walked
yet and still need a person or a dedicated E2E case.

**Bundle size** (`scripts/check_ui_bundle_size.sh`, after `make ui` and in
core-ui): warns, never fails, when the compressed UI outgrows
`ui_bundle_gzip_kb`.

Chrome 121 / Firefox 123 are the browser floor: check a change to shared styles
in both. Keep authentication in local processes; never log credentials or
authentication responses.

## Migration close-out checklist

1. When antd imports reach zero, add the `ui_raw_color` and `ui_raw_radius`
   counting rules to `scripts/arch-constraints.txt`; `check_architecture.sh`
   prints a reminder.
2. Remove the antd dependency, the React 19 compatibility patch, the temporary
   legacy interface adapters and unused `.ant-*` styles.
3. Open the dark theme entry only after every menu page passes acceptance in
   both languages, both themes, by keyboard and on the browser floor.
4. `ui/antd-inventory.json` is empty, and `scripts/ui_antd_inventory.mjs --check`
   and the browser check both pass.

## Page look

Confirmed 2026-11-06. Check every migrated page against this, by screenshot.

| Part | Rule |
| --- | --- |
| Frame | Top bar, side menu and frame share `surface-base`; the page is a white panel (radius 12, hairline `border`) 12px clear of the right and bottom edges (`--app-frame-gap`). No rule between menu and page. |
| Side menu | 200px (56 folded), folds to icons below 1280px; items 32px, 36px apart; group names `fg-subtle`; collapse button pinned at the bottom under a rule, its icon in the menu icons' column. |
| Icons | Shell icons come from `ui-kit/consoleIcons.ts` (HugeIcons, the set Zeron draws with): 16px in the menu and across the top bar, stroke 1.5. Top bar text 14px. No antd icons in the shell. |
| Page header | `PageHeader` only. Title 18/600 (Zeron `text-title`) behind the 4x18 blue bar; description 14px, one line. Breadcrumb only on pages below a menu page (`parents`), 12px above the title: menu section / linked parents / current page. Parent links use the brand color; the current title is non-clickable and marked aria-current="page". Rich titles supply a plain `breadcrumbTitle` so status badges and controls are not duplicated. Page-wide controls (`toolbar`) 12px under the title row, flush left. |
| Spacing | 16px from the header to the content and between top-level blocks; 24px between overview sections. Pages that fill the viewport subtract `--app-header-height`, `--app-frame-gap` and the panel borders. |
| Blocks | Section title 16/600 above a `Container` tray (radius 16, `rounded-2xl`); white panels inside at radius 12 (`rounded-xl`), panel title 14/600. |
| Controls | Zeron component defaults; do not restyle buttons or inputs per page. |
| Tables | `ResourceTable`: a sortable title is Zeron's `DataTableColumnHeader` (up/down mark, menu Ascending / Descending / Clear); a filterable column shows a funnel (`ui-kit/ColumnFilter`, blue while applied), the only place column filters are set; filters in effect are listed between the toolbar and the table as "column: values" chips, each clearable, plus "clear filters". The toolbar holds search and page-specific controls only. Its refresh and column-settings buttons, and the row expand chevron, are part of `ResourceTable` and use `ui-kit/consoleIcons` at 16px; pages do not add their own. Pinned header cells keep the header grey. |
| Table text | A header row is as tall as the shortest body row of its table's density (one line of text); rows with buttons or tags may be taller, the header does not follow them (`src/theme/tableDimensions.css`). Cells 14px regular (medium-size Zeron table); a second line or a tag is 12px. Names are not bold; a name link starts flush with its title. Text links in table cells use the brand color and underline on hover; shared product CSS covers Zeron and legacy antd tables, excluding button links and disabled controls. Titles are 14/600 and never overlap: an unresized column is at least as wide as its title and marks, a narrowed one cuts the title with an ellipsis. |
| Overlays | While antd and Zeron coexist, both libraries' overlays sit on one 1000 layer (`src/theme/layers.css`): the one opened last is on top. Right-side drawers of either library open beside the AI assistant. |
| Status words | Show a state as Zeron `Badge` with `status`, not as coloured text: dark success text reads as black. |

## Vendor marks

Accelerator vendor marks (NVIDIA, AMD, Ascend, ...) live in
`ui/public/vendor-logos/` and are mapped by `VENDOR_ASSETS` in
`ui/src/components/vendorAssets.ts`. A mark is a horizontal wordmark on a
transparent canvas: SVG (preferred) or PNG with an alpha channel, with no
padding box, plate or background colour of its own.

The `VENDOR_LOGO_INK` entry records the original ink tone. Light artwork
such as Kunlunxin's white wordmark uses no background plate in either theme.
The light theme darkens the ink with a CSS filter. Dark artwork keeps its
original colours and uses a light plate on the dark theme. Draw
a mark only with `VendorMarkImage` from `ui/src/components/VendorLogo.tsx`
(`VendorLogo` and the dashboard's hardware panel both do): it wraps the image
in the ink class, and the caller keeps its own sizing on the image. A plated
mark gains 4px of padding left and right and 2px above and below, exposed as
`--vendor-mark-pad-x` and `--vendor-mark-pad-y` for a box that must fit it.

The test-only package `internal/uiassets` enforces this in plain
`go test ./...`. `TestVendorLogosHaveTransparentBackgrounds` allows only
`.svg` and `.png`. An SVG needs a viewBox and has no background style, no
embedded `<image>` or `<foreignObject>`, and no filled rect, circle, ellipse,
polygon, polyline or straight-edged rectangular path covering 90% of the
viewBox; shapes inside `<defs>`, `<clipPath>`, `<mask>`, `<symbol>` and
similar containers are not drawn and are not measured. A PNG has alpha, fully
transparent corners, a border ring at least half transparent, and fewer than
60% of its pixels at alpha 240/255 or above (shipped marks measure 10-31%).
`TestVendorAssetsMatchLogoFiles` requires every file to be mapped and every
mapping to name a file. `TestVendorLogoInkIsDeclared` requires every mapped
mark to declare its ink and every declaration to name a mapped mark.
`TestVendorMarksAreDrawnThroughTheComponent` fails any `.ts`/`.tsx` file
under `ui/src` (tests aside) other than `vendorAssets.ts` and `VendorLogo.tsx`
that names `VENDOR_ASSETS` or a `/vendor-logos/` path.

`TestVendorLogoInkReadsOnItsSurfaces` measures the original artwork with WCAG 2
relative luminance and contrast. It checks dark ink against
`--vendor-logo-light` and pale ink against the dark theme’s `--surface-raised`.
It also applies the light-theme filter to pale ink and checks the result
against `--vendor-logo-light`. The UI check job runs the Chromium regression
for the rendered Kunlunxin and NVIDIA marks in both themes. A PNG's ink is every pixel at alpha 128/255 or
more, weighted by alpha and composited over the surface; an SVG's ink is each
drawn, filled shape's colour, weighted by the area of the shape's bounding
box (for a path, the box around its command end points; curve control points
are ignored). The measured tone is whichever reference surface most of the ink weight
reads better on, and it must match the declaration. At least half the ink
weight must reach 3:1 (the colour accents keep Kunlunxin and NVIDIA near
60%); no SVG fill may fall below 1.5:1, and at most 2% of a PNG's ink may.
The test logs each mark's numbers; run
`go test ./internal/uiassets/ -v -run TestVendorLogoInk` to see them.

The SVG checks read attributes, not a rendered image: they do not apply
`transform`, follow `<use>`, read fills set by `<style>` classes, or measure
strokes, and bounding boxes overstate thin or hollow shapes. A filled
`<text>` or `<use>`, a gradient or a `currentColor` fill fails the ink check
because it cannot be measured. Review new marks by eye in both themes too.

## Copy on the page

Confirmed 2026-11-13. A page states facts and what to do next. How a number
is computed, what it does not mean and where it comes from belong in a
`HelpTip` (ⓘ beside the label, `ui-kit/HelpTip`) or in the docs, not in grey
lines under the content. Check new text against this before adding it; an
agent writing UI text checks it too.

| Rule | Detail |
| --- | --- |
| One helper line | A field, a value or a block gets at most one line of helper text, 40 Chinese characters or 80 English ones. More goes into a `HelpTip`; past 200 Chinese characters, link the docs instead. |
| Messages | An error, a warning or a reason names the problem and the next step in at most 80 Chinese characters and two sentences. How the platform got there belongs in the docs. |
| Never inline | Formulas, aggregation and sampling rules, data sources, internal names (endpoints, packages, file paths the user does not type) and caveats such as "not guaranteed", "does not mean", "for reference only" go into a `HelpTip`. |
| No stacked caveats | At most one caveat per block, and none when the label already says it: "estimated" needs no "not guaranteed"; a fact shown on the block is not restated as a caveat. |
| Facts as values | Related numbers share one line, `label value · label value`, not one sentence each. |
| Warnings act | A warning (`Notice`, amber text) names the problem and the next step in one sentence; text with no next step is not a warning. |
| Empty states | A title, at most one sentence, and the action. |
| No chained clauses | A string joins at most two clauses with `；`; a `：` that opens such a chain counts as one more. Three clauses are three sentences written to pass the sentence count: cut, or split into the parts below. |

How the text is written, checked by the reader, not by a budget:

| Rule | Detail |
| --- | --- |
| What the user decides with | A page says what the control does and what the user must do. Placement order, retry timing and what a restart does inside the platform go into the docs. |
| No unasked reassurance | "Does not lose a replica", "changes nothing" and the like are cut. A consequence is written only when the action destroys or interrupts something. |
| Product words | `rank`, `wrapper`, `runner`, `server_dial`, package and file names the user does not type stay off the page. Commands and config keys the user must type are written as they are typed. |
| Chinese as Chinese | Chinese text is written for a Chinese reader, not translated clause by clause: 无法访问, not 够不到; 正被使用, not 有东西在用; 平台的, not 我们的; 仍没有节点接入, not 已等了一阵. |
| The user's side | A title or summary says what the user sees or cannot do, not what the server did or how it files the case: "4 Agents cannot connect: their node credential does not match this server", not "refused for a credential this server does not know"; "14 nodes run an Agent version that differs from the server", not "Agent target 56cb84fa" over "Different build (14)". A bare value is not a title. |
| Charts explain themselves | What a colour, a line or a shade stands for is a legend entry beside its swatch, or the mark's tooltip. No sentence names a colour ("the black line", "red = error"): the name is wrong in the other theme and unreadable without colour. How the chart is computed is the `HelpTip` beside its title. |

### Problems and what to do

A message about something wrong has up to four parts, always in this order.
Each part is its own sentence or its own element; a part that has nothing to
say is left out, not filled.

| Part | Says | Form |
| --- | --- | --- |
| Summary | What is wrong, in terms the user can see on the page | One sentence, no cause and no advice. Key `summary`, or the code itself for a one-string message |
| Impact | What does not work because of it | One sentence, only when the summary does not already say it. Key `impact` |
| Fix | What the user does | An imperative sentence. A command the user types is a copyable block built by the code or sent by the server, never text inside the sentence. Several ways out or several steps are a list, one key each under `fix` or `step` |
| After | What the fix interrupts or how long it takes to show | One sentence, only when the fix stops or restarts something. Key `after` |

Which form a message takes follows from where it is shown:

| Shown as | Form |
| --- | --- |
| One line: a toast, an API error (`apiError.*`), a scheduling reason, a table cell | One string, at most two sentences: the summary, then the fix. No command longer than a flag or a file name |
| A `Notice`, a drawer section, a dialog | Separate keys. The summary is the title; impact, fix and after are the body; the action is a button or a copyable block |

Use separate keys whenever one of these holds, whatever the surface:

- the fix is a command the user types;
- the fix has more than one step, or more than one way out;
- the same fix answers several summaries (write it once and reuse the key);
- the component already has a title and a body.

A fix is on the page. A `HelpTip` holds causes and method; a user who never
hovers still reads what to do. A fix written only in a `HelpTip`, or a command
inside a sentence, is the fault this section exists to prevent.

A notice that counts things offers the action on them. Its button does the
fix (opens the batch dialog, the form, the drawer); "Dismiss" alone is not an
action. A count that filters the list is a link named for what it does ("View
these nodes"), not a number that happens to be clickable.

Parts shown together are read together: text a component joins from several
keys counts as one message, and its total follows "One helper line" and
"Messages" above. Joining five sentences into one paragraph is the same fault
as writing them in one key.

Existing shapes to copy: `nodes:doctor.finding.<code>.{summary,impact}`,
`nodes:gpuIsolation.{summary,reason,fix}`, `nodes:slurm.{unavailable,step}`.

A locale key whose name ends in `Help` holds `HelpTip` content and is shown
only there; text shown on the page uses any other name. A `Help` key is
never a form item's `extra` or the text of a paragraph.

The `ui_copy_*` budgets in `scripts/arch-constraints.txt` (max, ratchet down)
count what breaks these rules; `node scripts/ui_copy_audit.mjs list` names each
one. A new string over a limit is cut or moved into the docs, not covered by a
raised budget, a renamed key or a split key.

| Budget | Counts |
| --- | --- |
| `ui_copy_long_strings` | Chinese strings over 120 characters (200 for `Help` keys) |
| `ui_copy_caveat_strings` | Non-`Help` strings carrying a caveat phrase |
| `ui_copy_helper_strings` | Non-`Help` page strings over 40 Chinese characters |
| `ui_copy_message_strings` | Error, warning and reason strings over 80 Chinese characters |
| `ui_copy_sentences_strings` | Non-`Help` strings of more than two sentences |
| `ui_copy_clauses_strings` | Non-`Help` strings chaining clauses with `；` past "No chained clauses" |
| `ui_copy_command_strings` | Strings, `Help` keys included, with a command the user types inside a sentence (a key named `*Command` holds a block's text) |
| `ui_copy_en_strings` | English strings over 240 characters (400 for `Help` keys) |
| `ui_copy_inline_help_refs` | Places that put a `Help` key on the page instead of in a `HelpTip` |

### Reviewing copy

The budgets count length and shape. They cannot tell whether a sentence helps,
and text can pass every one of them and still leave the user without a clue:
the three strings that prompted this section did. A maintainer reviewing a
change that adds or edits text under `ui/src/i18n/locales/` reads each string
where it is shown (the merge request carries a screenshot of a new notice,
chart or dialog, in both languages) and sends the change back when one of
these holds. Text written by an agent gets the same reading.

| Ask | Send back when |
| --- | --- |
| Would a user who has never seen the code know what is wrong, or what the number means? | The summary is in the server's words (refused, booked, reconciled, a build class, an internal state name), or the title is a bare value. See "The user's side". |
| Is the next step on the page, and can it be taken there? | The fix is only in a `HelpTip` or the docs; the only button is "Dismiss"; a filtering link reads as a label; a command sits inside a sentence. |
| Does the chart need a sentence to be read? | A sentence names a colour, a line or a shade, or says how the chart is drawn, outside a legend and a `HelpTip`. |
| Was a budget passed by shape? | Clauses chained with `；` or `：`; a key renamed `*Help` or `*Command` for the allowance; one message split into keys that are shown together; a raised budget. |
| Does the count match what it counts? | The title says "N Agents" over a list of N hosts, or a total that the rows below do not add up to. |
| Do both languages say the same thing, each as its own reader would write it? | One language carries a fact, a caveat or an action the other lacks; see "Chinese as Chinese". |

The rewrite belongs to the same merge request: "copy to be polished later" is
not a follow-up. Shapes to copy for a notice with its fix and action:
`nodes:rejects.{summary,causeHelp,fix}` with `RejectedAgentsNotice`, and
`nodes:fleet.{mismatch,fact,upgradeAll}` with `AgentFleetNotice`; for a chart,
`gpus:composition` with `GPUMemoryComposition`.

## Choosing a component

Zeron first: a new or replaced component comes from Zeron, installed with
`scripts/zeron-registry.sh add <names>` in the change that first needs it, not
ahead of use. Compose in `ui-kit/` only what Zeron does not have, and ask the
product owner before doing so, or when a Zeron component costs bundle size or
speed (measure first).

| Need | Use |
| --- | --- |
| Page frame, header, list, detail drawer | `PageHeader`, `ResourceTable`, `DetailDrawer` (tabs, visited tabs kept) |
| Facts in a drawer ("label: value") | Zeron `detail-list` / `info-item`, replacing antd `Descriptions` |
| Overview blocks | `Container` tray with white panels (Page look, Blocks); Zeron `card` for a standalone card |
| Charts | Zeron charts (`bar-chart`, `line-chart`, `area-chart`, `pie-chart`, `heatmap-chart`, …). Check bundle size (`ui_bundle_gzip_kb`) and, for large data such as the scheduling heatmap or long series, speed against the ECharts version before switching |
| Status | Zeron `Badge` with `status` |
| Explanation beside a label or value | `ui-kit/HelpTip` (see "Copy on the page") |
| Confirm, toast, empty, progress, upload, number, segmented | `ui-kit/` compositions (`ConfirmPopover`, `feedback`, `EmptyState`, `Progress`, `FileUpload`, `NumberInput`, `Segmented`) |

The registry is pinned (`ui/src/theme/zeron/source.json`); upstream changes do
not reach the console until the pin moves. Move it when a needed component or
fix exists only upstream, in its own change: new pin, reinstall, then the full
checks above (unit tests, browser check with comparison).

## Product typography defaults

Console body text is 14px (`--font-size-body`) with the shared body line height.
Both `body` and portal dialog roots inherit these product defaults from
`styles.css`; bare text in a dialog must not fall back to the browser's 16px
default. Keep Zeron managed sources unchanged. Use `text-body` for ordinary
content, `text-label` for 12px supporting text, and `text-title` for 18px page
or dialog titles. A list toolbar title uses body size and semibold weight.
Do not rely on an ancestor inside the page to size a portal, or use an arbitrary
font-size on each new dialog.

After changing typography or popup composition, run the Chromium geometry
checks at 1280 and 1920 widths, including both themes.
`make test-ui-template-catalog-layout` checks the chip editor's computed body
size and title size, the filter header's hit targets, and the gap between
monitor signal cards and the service toolbar. Build the UI before this target;
React unit tests alone do not verify computed sizes or overlaps.

Organization avatars keep official images when available. Missing or failed images use white initials on stable, organization-keyed saturated blue, cyan, amber, green and violet backgrounds. The deeper artwork shades are defined by --organization-avatar-1 through -5, shared by light and dark themes with at least 4.5:1 contrast. Do not mix gray into these colors or switch initials to black. Do not reassign colors on refresh or locale changes.


### Resource navigation checks

Resource names that open a drawer use the native link button variant (`variant="link"` for Zeron, `type="link"` for legacy antd). Names that navigate use actual links. Do not use ghost buttons for resource names, and do not make inert names look clickable. Icon/action buttons and disabled controls keep their own styles.

`tests/tableNameAffordance.test.ts` runs in the normal UI test gate and scans name-column renderers across product pages. `ui-check` also runs `tests/browser/table-link-rules.mjs` against every visited page/drawer, including full-page details reached from a list: visible table text links and clickable resource names must resolve to the theme color, including nested text. Its unit tests exercise failures and exclusions; real computed colors require the browser job. When changing shared navigation, test breadcrumbs through the current page and click the parent link; use heading roles when the resource name also appears in the breadcrumb.

<!-- translation-source-sha256: 2f7226da7b426ecd3d1ea5fc1e020126bc9b0c877c2822777495277663eb857b # UI development -->
<!-- translation-source-sha256: 13c5c07cffc3a9231089d41ecf45a179558c616141e1c57672f4acf10a11c2c6 ## Directories and public APIs -->
<!-- translation-source-sha256: e1a59e9f1ae0e2db365d11425279f53bad0d4520718a02e31139ee920be91443 ## Pinned source and synchronization -->
<!-- translation-source-sha256: 4e6ecdfae8ebd5c05e8c7ff1dd34de61dc226b9b89faa8094a0d4a2c60f0c359 ## Verification -->
<!-- translation-source-sha256: c7b61b2ede38b04e51393df4490c765efbc0d8ea7a17d80b16538e355c3e829e ## Migration close-out checklist -->
<!-- translation-source-sha256: 1318afa013ade9be7b3751ffcdccc06b442a512cef5b728790ea7adc728bd6d9 ## Page look -->
<!-- translation-source-sha256: 4b30836bdd43985503aafd5807e275e3b328f812a2a3038781bd808b5bbeeae8 ## Vendor marks -->
<!-- translation-source-sha256: fddaafffae7e564494407df1c976f2beec4c1c8827cc55a1fc620c0d00eafb0c ## Copy on the page -->
<!-- translation-source-sha256: 4f94d2593a004f64b16607597a4142b04e86ca85affe710e12aabd04fffe4721 ### Problems and what to do -->
<!-- translation-source-sha256: a212225b624adfa8421d2175bc828d4203339d58411493c706eeb3c8afd57cd2 ### Reviewing copy -->
<!-- translation-source-sha256: af116f9d28a2e9db00b4cb72e4bafbe953ab909047dcc0b330bdea227c632e95 ## Choosing a component -->
<!-- translation-source-sha256: 8093bb1dc2312ac300409380b97205019cb94c8e41e2a04c4a6037a91ba0eb58 ## Product typography defaults -->
<!-- translation-source-sha256: aa7be0b3ecb8d403615075408c31dedc3c018b9e001abc389b09f655fed2429a ### Resource navigation checks -->
