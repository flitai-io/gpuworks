# UI development

- New pages use Zeron components and project compositions in `src/ui-kit/`.
- Use Zeron semantic token variables for colors; do not add literal colors.
- Use token variables for radii; do not add literal radius values.
- Do not add new antd imports. Lower `ui_antd_import_files` after migrating a page.
- Compose page structure from PageHeader, ResourceTable and DetailDrawer.
- Preserve routes, API contracts, permissions and business behavior.
- All UI text belongs in `src/i18n/locales/{zh,en}`; use `i18n/glossary.json`.
- Run TypeScript, ESLint, i18n, build and related tests; `scripts/ui_check_ci.sh` checks the pages in a browser.
- After migrating a page: `node scripts/ui_antd_inventory.mjs --write` and lower `ui_antd_import_files`.
- Page frame, titles and radii follow docs/dev/ui.md "Page look"; after swapping components, screenshot every touched page at 1440 and 1024 and compare. If unsure, stop and ask the user.
- Install Zeron components only with `scripts/zeron-registry.sh add <names>`; run its `check` after.
- Prefer Zeron components, charts included (docs/dev/ui.md "Choosing a component"); ask before composing one Zeron lacks.
- Change a shared building block (ui-kit, app shell, PageHeader, ResourceTable, DetailDrawer) and its `tests/<name>.test.tsx` in the same commit; a new one comes with its test (`ui_untested_shared_files`).
- antd and Zeron coexist while pages migrate; check docs/dev/ui.md before assuming a component exists.

See [UI development](../docs/dev/ui.md) for sources, directories and verification.
