# UI development

- New pages use Zeron components and project compositions in `src/ui-kit/`.
- Use Zeron semantic token variables for colors; do not add literal colors.
- Use token variables for radii; do not add literal radius values.
- Do not add new antd imports. Lower `ui_antd_import_files` after migrating a page.
- Compose page structure from PageHeader, ResourceTable and DetailDrawer.
- Preserve routes, API contracts, permissions and business behavior.
- All UI text belongs in `src/i18n/locales/{zh,en}`; use `i18n/glossary.json`.
- Text follows docs/dev/ui.md "Copy on the page": one helper line per field or block; formulas, caveats, data sources and method notes go into `ui-kit/HelpTip` or the docs, never as extra grey lines. Do not add small text to explain a value.
- Run TypeScript, ESLint, i18n, build and related tests; `scripts/ui_check_ci.sh` checks the pages in a browser.
- After migrating a page: `node scripts/ui_antd_inventory.mjs --write` and lower `ui_antd_import_files`.
- Page frame, titles and radii follow docs/dev/ui.md "Page look"; after swapping components, screenshot every touched page at 1440 and 1024 and compare. If unsure, stop and ask the user.
- Install Zeron components only with `scripts/zeron-registry.sh add <names>`; run its `check` after.
- Prefer Zeron components, charts included (docs/dev/ui.md "Choosing a component"); ask before composing one Zeron lacks.
- Change a shared building block (ui-kit, app shell, PageHeader, ResourceTable, DetailDrawer) and its `tests/<name>.test.tsx` in the same commit; a new one comes with its test (`ui_untested_shared_files`).
- antd and Zeron coexist while pages migrate; check docs/dev/ui.md before assuming a component exists.

See [UI development](../docs/dev/ui.md) for sources, directories and verification.

## Semantic charts and code typography

- Applies to every chart, SVG, capacity bar and timeline, including legacy antd/ECharts views. Status outcomes use `colors.success` / `colors.warning` / `colors.danger` and their CSS equivalents. Never use `chart-N` or ordinal aliases for success/failure: categorical palette order can change independently.
- Queueing, sleeping, idle capacity, external occupancy and selection do not by themselves indicate a warning. Preserve explicit threshold/failure warnings. Models, deployments, memory composition and ordinary metric series remain categorical; keep labels and legends.
- Shared code typography lives in `src/theme/layers.css`: Zeron `--font-size-body` (14px at the default root, matching body text), `--line-height-code`, monospace. Plain `pre` is covered, so newly added command blocks do not inherit a drawer's 16px default. Do not alter the HTML root size to fix local text.
- Log text uses the same 14px body size on a fixed dark surface. Platform events use information blue, warning/error use their semantic foreground tokens; source ANSI colours remain source-defined. Shared log styles live beside the terminal theme.
- xterm uses `src/theme/terminalTheme.ts`: fixed dark surface, explicit ANSI palette, minimum text contrast 4.5. Preserve ANSI hue meanings, including magenta; do not force all shell output to white.
- Regression checks: `tests/semanticVisuals.test.ts` and `node tests/browser/semantic-visuals.mjs /absolute/output/path`. The browser check uses synthetic code/terminal data, real production CSS, light/dark themes, Chinese/English and 1440/768px views. Add new command-area selectors here when introducing an overriding style; screenshots alone are insufficient—assert computed typography and overflow.

- `node tests/browser/log-command-layout.mjs /absolute/output/path` exercises production drawers, instance/service log components, Kubernetes commands, deployment preview/manual editor and AI log evidence with deterministic API data and the production theme providers. It does not validate live SSH or server behavior.
