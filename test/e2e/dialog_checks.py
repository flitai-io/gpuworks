"""Dialog checklist checks for section 12.

Callers pass the dialog the product actually opened. These helpers do not
decide which dialog is under test, and they do not mark a manifest covered.
"""
from __future__ import annotations

from playwright.sync_api import expect

from platform_token_read_revoke import button, check, modal_geometry


VIEWPORT = {"width": 1440, "height": 900}


def _inside(inner, outer):
    return (inner["top"] >= outer["top"] - 0.5 and inner["left"] >= outer["left"] - 0.5
            and inner["right"] <= outer["right"] + 0.5 and inner["bottom"] <= outer["bottom"] + 0.5
            and inner["bottom"] > inner["top"] and inner["right"] > inner["left"])


def _overlaps(a, b):
    return not (a["bottom"] <= b["top"] + 0.5 or b["bottom"] <= a["top"] + 0.5
                or a["right"] <= b["left"] + 0.5 or b["right"] <= a["left"] + 0.5)


def regions_stacked(ordered, box):
    """Ordered regions sit inside the container, top to bottom, and do not overlap."""
    if not isinstance(box, dict) or any(not isinstance(region, dict) for region in ordered):
        return False
    if any(not _inside(region, box) for region in ordered):
        return False
    for earlier, later in zip(ordered, ordered[1:]):
        if earlier["bottom"] > later["top"] + 0.5:
            return False
    return all(not _overlaps(left, right)
               for index, left in enumerate(ordered) for right in ordered[index + 1:])


def boxes_stacked(title, body, footer, box):
    """Title, body and footer sit inside the dialog and do not overlap."""
    return regions_stacked((title, body, footer), box)


def in_viewport(region, size):
    if not isinstance(region, dict) or not isinstance(size, dict):
        return False
    return (region["top"] >= -0.5 and region["left"] >= -0.5
            and region["right"] <= size["width"] + 0.5 and region["bottom"] <= size["height"] + 0.5
            and region["bottom"] > region["top"] and region["right"] > region["left"])


def _regions(dialog):
    return dialog.evaluate("""element => {
        const rect = (node) => {
            if (!node) return null;
            const box = node.getBoundingClientRect();
            return {top: box.top, left: box.left, right: box.right, bottom: box.bottom};
        };
        return {
            title: rect(element.querySelector(".ant-modal-title")),
            body: rect(element.querySelector(".ant-modal-body")),
            footer: rect(element.querySelector(".ant-modal-footer")),
            box: rect(element),
        };
    }""")


def assert_viewport(page):
    check(page.viewport_size == VIEWPORT, f"dialog layout measured at {page.viewport_size}, not 1440x900")


def _focus_index(dialog):
    return dialog.evaluate("""element => {
        const active = document.activeElement;
        if (!active || !element.contains(active)) return -1;
        const nodes = [...element.querySelectorAll("button, a[href], input, select, textarea, [tabindex]")];
        return nodes.indexOf(active);
    }""")


def _drawer_focus(dialog):
    # rc-drawer focuses the outer .ant-drawer panel, not the role=dialog node.
    # Its two aria-hidden sentinels sit in that panel, outside role=dialog.
    return dialog.evaluate("""element => {
        const panel = element.closest('.ant-drawer');
        const active = document.activeElement;
        const inside = !!(active && element.contains(active));
        const onPanel = !!(active && active === panel);
        const sentinel = !!(active && panel && panel.contains(active) && !inside && !onPanel
            && active.tagName === 'DIV' && active.getAttribute('aria-hidden') === 'true'
            && !active.getAttribute('role'));
        const nodes = [...element.querySelectorAll('button, a[href], input, select, textarea, [tabindex]')];
        return {
            inside, onPanel, sentinel,
            inDrawer: inside || onPanel || sentinel,
            index: inside ? nodes.indexOf(active) : -1,
            tag: active ? active.tagName : null,
            className: active ? String(active.className).slice(0, 160) : null,
            label: active ? active.getAttribute('aria-label') : null,
        };
    }""")



def _drawer_focusables(dialog):
    """How many tab stops the drawer itself publishes, for sizing a Tab cycle."""
    return dialog.evaluate("""(d) => d.querySelectorAll(
        'a[href], button:not([disabled]), input:not([disabled]), select:not([disabled]), '
        + 'textarea:not([disabled]), [tabindex]:not([tabindex="-1"])').length""")

def _cycle_drawer(page, dialog, key):
    """One rc-drawer sentinel is part of the trap. The page behind it is not."""
    seen = []
    bridges = 0
    # Enough presses to come back to the first control however many the drawer
    # holds. A fixed 48 stopped being enough once the replica editor grew: the
    # cycle is still the assertion, only the budget is derived, and a drawer whose
    # trap is genuinely broken still fails because the first control never repeats.
    budget = max(48, _drawer_focusables(dialog) * 2 + 8)
    for _ in range(budget):
        page.keyboard.press(key)
        focus = _drawer_focus(dialog)
        check(focus["inDrawer"], f"{key} reached background content: {focus}")
        if focus["sentinel"] or focus["onPanel"]:
            bridges += 1
            check(bridges <= 1, f"{key} left the drawer controls: {focus}")
            continue
        bridges = 0
        if focus["index"] >= 0 and focus["index"] in seen:
            check(len(seen) >= 2, f"{key} did not move between drawer controls; visited {seen}")
            return len(seen)
        if focus["index"] >= 0:
            seen.append(focus["index"])
    check(False, f"{key} did not cycle inside the drawer; visited {seen}")
    return 0


def drawer_regions(dialog):
    """The drawer's header, title, body and header actions as viewport boxes."""
    return dialog.evaluate("""element => {
        const rect = (node) => {
            if (!node) return null;
            const box = node.getBoundingClientRect();
            return {top: Math.round(box.top), left: Math.round(box.left),
                    right: Math.round(box.right), bottom: Math.round(box.bottom)};
        };
        return {
            box: rect(element),
            header: rect(element.querySelector('.ant-drawer-header')),
            title: rect(element.querySelector('.ant-drawer-title')),
            body: rect(element.querySelector('.ant-drawer-body')),
            extra: rect(element.querySelector('.ant-drawer-extra')),
        };
    }""")


def check_drawer_stacked(regions, viewport):
    """12.2: the drawer's parts stay inside the viewport and do not overlap each other."""
    width, height = viewport["width"], viewport["height"]
    for key in ("header", "title", "body"):
        region = regions.get(key)
        check(region is not None, f"drawer has no {key} region: {regions}")
        check(region["top"] >= 0 and region["left"] >= 0
              and region["right"] <= width and region["bottom"] <= height,
              f"drawer {key} leaves the viewport: {region}")
    check(regions["body"]["top"] >= regions["header"]["bottom"] - 1,
          f"drawer body overlaps its header: {regions}")
    check(regions["title"]["bottom"] <= regions["header"]["bottom"] + 1,
          f"drawer title spills out of its header: {regions}")
    extra = regions.get("extra")
    if extra is not None:
        check(extra["right"] <= width and extra["top"] >= 0,
              f"drawer actions leave the viewport: {extra}")
        check(extra["left"] >= regions["title"]["right"] - 1,
              f"drawer actions overlap the title: {regions}")


def assert_drawer_identity(page, dialog, name, first_step, absent):
    """Drawer identity. This is not a modal: no title/body/footer stack.

    name is the accessible name opened from the caller's real page control.
    first_step is the first business region of this drawer. absent is a region
    that belongs to a different component and must not be open.
    """
    assert_viewport(page)
    expect(page.get_by_role("dialog")).to_have_count(1)
    expect(dialog).to_be_visible()
    expect(dialog).to_be_in_viewport(ratio=1)
    kind = dialog.evaluate("""element => ({
        className: String(element.className),
        ariaModal: element.getAttribute('aria-modal'),
        inDrawer: !!element.closest('.ant-drawer'),
        inModal: !!element.closest('.ant-modal')
    })""")
    check(kind["inDrawer"] and not kind["inModal"] and "ant-drawer-content" in kind["className"],
          f"opened component is not a drawer: {kind}")
    check(kind["ariaModal"] == "true", f"drawer is not modal: {kind}")
    title = dialog.locator(".ant-drawer-title")
    expect(title).to_be_visible()
    check(title.inner_text() == name, f"drawer title {title.inner_text()!r} is not {name!r}")
    expect(dialog.locator(".ant-drawer-close")).to_be_visible()
    expect(dialog.locator(".ant-modal-close")).to_have_count(0)
    expect(first_step).to_be_visible()
    expect(absent).to_have_count(0)
    # 12.2 for a drawer: it has no title/body/footer stack, but its header, body and header
    # actions still have to fit the viewport without overlapping each other. Measured here so
    # every drawer opened through this helper gets the same check.
    regions = drawer_regions(dialog)
    check_drawer_stacked(regions, page.viewport_size)
    return {"layer": "ui", "drawer_shell": name, "role": "dialog", "component": kind["className"],
            "aria_modal": kind["ariaModal"], "first_step_visible": True, "other_component_count": 0,
            "viewport": f"{page.viewport_size['width']}x{page.viewport_size['height']}",
            "drawer_regions": regions}


def assert_yaml_drawer_focus(page, dialog, save, editor):
    """12.3 for the YAML drawer. Tab inside the editor indents, so it is not pressed there."""
    opened = _drawer_focus(dialog)
    check(opened["onPanel"] or opened["inside"], f"default focus is outside the drawer: {opened}")
    save.focus()
    page.keyboard.press("Shift+Tab")
    shifted = _drawer_focus(dialog)
    check(shifted["inDrawer"], f"Shift+Tab reached the page: {shifted}")
    save.focus()
    page.keyboard.press("Tab")
    expect(editor).to_be_focused()
    tabbed = _drawer_focus(dialog)
    check(tabbed["inside"], f"Tab from Save left the drawer: {tabbed}")
    check(dialog.get_attribute("aria-modal") == "true", "YAML drawer is not modal")
    return {"layer": "ui", "default_focus": "panel" if opened["onPanel"] else "dialog",
            "shift_tab_in_drawer": True, "tab_focus": "editor", "background_focus_blocked": True,
            "aria_modal": "true"}


def assert_drawer_shell(page, dialog, name, first_step, absent):
    """Drawer identity and focus. Focus is a separate cycle from identity."""
    evidence = assert_drawer_identity(page, dialog, name, first_step, absent)
    page.wait_for_function("""element => {
        const panel = element.closest('.ant-drawer');
        const active = document.activeElement;
        return !!(active && panel && (active === panel || element.contains(active)));
    }""", arg=dialog.element_handle(), timeout=5000)
    opened = _drawer_focus(dialog)
    check(opened["onPanel"] or opened["inside"], f"default focus is outside the drawer: {opened}")
    tab_stops = _cycle_drawer(page, dialog, "Tab")
    shift_stops = _cycle_drawer(page, dialog, "Shift+Tab")
    evidence.update({"default_focus": "panel" if opened["onPanel"] else "dialog",
                     "default_focus_index": opened["index"], "tab_stops": tab_stops,
                     "shift_tab_stops": shift_stops, "background_focus_blocked": True})
    return evidence


def assert_dialog_shell(page, dialog, marker, footer):
    """12.1 identity, 12.2 viewport and non-overlap, 12.3 focus trap."""
    assert_viewport(page)
    expect(dialog).to_be_visible()
    expect(dialog).to_have_count(1)
    expect(marker).to_be_visible()
    modal_geometry(dialog, footer)
    regions = _regions(dialog)
    check(boxes_stacked(regions["title"], regions["body"], regions["footer"], regions["box"]),
          f"dialog title, body and footer overlap or leave the dialog: {regions}")
    page.wait_for_function("element => element.contains(document.activeElement)",
                           arg=dialog.element_handle(), timeout=5000)
    check(_focus_index(dialog) >= 0, "default focus is outside the dialog")
    # The opening sentinel is focused once and is not part of the later Tab cycle.
    for key in ("Tab", "Shift+Tab"):
        seen = []
        cycled = False
        for _ in range(24):
            page.keyboard.press(key)
            current = _focus_index(dialog)
            check(current >= 0, f"{key} moved focus out of the dialog")
            if current in seen:
                check(len(seen) >= 2, f"{key} did not move between dialog controls; visited {seen}")
                cycled = True
                break
            seen.append(current)
        check(cycled, f"{key} did not cycle inside the dialog; visited {seen}")


def assert_nested_dismissals(dialog, parent, reopen, dismissals, mutation_count, focus_trigger=None):
    """Close the child and leave the parent. mutation_count stays unchanged."""
    baseline = mutation_count()
    for label, dismiss in dismissals:
        dismiss(dialog)
        expect(dialog).to_be_hidden()
        expect(parent).to_be_visible()
        expect(dialog.get_by_role("button").first).to_be_hidden()
        if focus_trigger is not None:
            expect(focus_trigger).to_be_focused()
        check(mutation_count() == baseline, f"{label} sent a mutation")
        dialog = reopen()
        expect(dialog).to_be_visible()
        expect(parent).to_be_visible()
    return dialog


def cancel_button(label):
    return lambda dialog: button(dialog, label).click()


def escape(dialog):
    # The dialog panel is not focusable. Pressing Escape there keeps focus in
    # the parent drawer, and the drawer handles that key by closing itself.
    dialog.locator(".ant-modal-close").focus()
    dialog.page.keyboard.press("Escape")


def close_button(dialog):
    dialog.locator(".ant-modal-close").click()


def mask(dialog):
    # rc-dialog closes when the click target is the wrap, which covers the mask.
    dialog.locator("xpath=ancestor::div[contains(@class,'ant-modal-wrap')]").click(position={"x": 8, "y": 8})
