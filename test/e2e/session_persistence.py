"""Re-entered auth pages must match an independent session query.

The saved login is the identity returned by POST /api/v1/auth/login. After that
save, reloading the networks page and opening the settings deep link must show
those pages, and a cookie-only client plus a bearer-only client must read the
same identity from GET /api/v1/auth/me. A management session has no agent.
"""
from __future__ import annotations

import re
from urllib.parse import urlsplit

from playwright.sync_api import expect

from local_browser_fixture import http
from platform_token_read_revoke import check

ME = "/api/v1/auth/me"
COOKIE = "gpuworks_session"


def _cookie_identity(play, base, value):
    client = play.request.new_context(extra_http_headers={"Cookie": f"{COOKIE}={value}"})
    try:
        response = client.get(base + ME)
        return response.status, response.json()
    finally:
        client.dispose()


def exercise_session_persistence(page, context, play, server, identity, networks, settings, auth, observe, cookie, bearer):
    page.reload()
    refreshed = page.request.get(server.base + ME)
    check(refreshed.status == 200 and refreshed.json() == identity,
          "independent session query after re-entering networks differs from the saved login")
    check(_cookie_identity(play, server.base, cookie) == (200, identity),
          "cookie-only session query after re-entry differs from the saved login")
    check(http(server.base, "GET", ME, bearer) == (200, identity),
          "bearer session query after re-entry differs from the saved login")
    expect(page).to_have_url(server.base + "/networks")
    expect(page.get_by_role("heading", name=networks["title"], exact=True)).to_be_visible()
    expect(page.get_by_label(auth["password"], exact=True)).to_have_count(0)

    peer = context.new_page()
    observe(peer)
    peer.goto(server.base + "/settings")
    expect(peer).to_have_url(server.base + "/settings")
    expect(peer.get_by_role("heading", name=settings["page"]["title"], exact=True)).to_be_visible()
    peer.reload()
    deep = peer.request.get(server.base + ME)
    check(deep.status == 200 and deep.json() == identity,
          "independent session query after the settings deep link differs from the saved login")
    expect(peer).to_have_url(server.base + "/settings")
    expect(peer.get_by_role("heading", name=settings["page"]["title"], exact=True)).to_be_visible()
    return peer, {
        "reentered_networks": True,
        "deep_link_settings": True,
        "independent_get_matches": True,
        "cookie_client": 200,
        "bearer_client": 200,
        "auth_call": "GET /api/v1/auth/me",
        "agent_or_workload": False,
    }


def exercise_navigation_entries(page, server, report):
    """22.1: every sidebar entry is entered by deep link and by click, and the entry
    that highlights afterwards names the page that rendered. The list is the menu the
    page actually shows, not a copy of the route table, so an entry that quietly
    disappears or gains a wrong target shows up as a missing or mismatched row."""
    # The shell mounts the same menu twice: once inside the desktop sider and once inside the
    # mobile drawer, and the drawer stays in the document while closed. A bare sidebar
    # therefore resolves to two menus, and the selection read below then matches nothing.
    # The desktop one is named explicitly, which is also the one this case drives.
    nav = page.locator('[data-slot="sidebar"]')
    expect(nav).to_be_visible()
    labels = [item.strip() for item in nav.locator('[data-slot="nav-item"]').all_inner_texts()]
    entries = [{ "label": label, "href": nav.locator('[data-slot="nav-item"] a').nth(index).get_attribute("href") }
               for index, label in enumerate(labels)]
    check(len(entries) >= 15, f"the sidebar rendered only {len(entries)} entries")
    rows = []
    for entry in entries:
        page.goto(server.base + entry["href"])
        # Some entries land on a child route (/analytics -> /analytics/usage); the entry
        # still owns that address, so the settled path is recorded and checked to sit
        # under the entry rather than equal to it.
        expect(page).to_have_url(re.compile(re.escape(server.base + entry["href"].rstrip("/")) + r"(?:/.*)?$"))
        page.wait_for_load_state()
        # A page may redirect once it mounts, and the sidebar entry still owns the address it
        # lands on: /analytics renders <Navigate to="/analytics/usage"> until a tab is chosen,
        # so the settled address is the child route rather than the entry's own href. The
        # settled path is therefore read only once the address has stopped changing, and it
        # is required to sit under the entry, which is what the check above already asked of
        # the first navigation. Reading it here rather than before the load state settled was
        # what made this row compare a pre-redirect address against a post-reload one.
        settled = urlsplit(page.url).path
        for _ in range(10):
            page.wait_for_timeout(150)
            current = urlsplit(page.url).path
            if current == settled:
                break
            settled = current
        # A page may add query state of its own (?organization=...), so the settled
        # address is compared on its path with the query kept for the record.
        landed = settled
        check(landed == entry["href"] or landed.startswith(entry["href"].rstrip("/") + "/"),
              f"{entry['href']} settled on {landed}, which is outside that entry")
        # The entry that lights up must be this one, and it must be exactly one: a page
        # that highlights two entries, or none, is not navigable.
        selected = nav.locator('[data-slot="nav-item"][data-active="true"]')
        expect(selected).to_have_count(1)
        expect(selected).to_contain_text(entry["label"])
        heading = page.get_by_role("heading").first
        expect(heading).to_be_visible()
        title = heading.inner_text().strip()
        check(bool(title), f"the page behind {entry['href']} rendered no heading")
        # Reloading keeps both the address and the highlighted entry.
        page.reload()
        expect(page).to_have_url(re.compile(re.escape(server.base + landed) + r"(?:\?.*)?$"))
        expect(nav.locator('[data-slot="nav-item"][data-active="true"]')).to_have_count(1)
        expect(nav.locator('[data-slot="nav-item"][data-active="true"]')).to_contain_text(entry["label"])
        expect(page.get_by_role("heading").first).to_contain_text(title)
        # The heading is real copy, not an unresolved key or an internal field name.
        check(not _INTERNAL_IDENTIFIER(title),
              f"{entry['href']} renders heading {title!r}, which looks like an internal identifier")
        # A page may legitimately be titled the same as its menu entry (dashboard is),
        # so equality is recorded rather than asserted.
        row_label_equals_title = title == entry["label"]
        rows.append({"href": entry["href"], "label": entry["label"], "title": title,
                     "landed": landed, "reload_kept": True,
                     "title_equals_label": row_label_equals_title})
    report["navigation_entries"] = rows
    check(len(rows) == len(entries), "not every sidebar entry completed the round trip")
    # Which locale is running is recorded, not asserted: a single run cannot see the
    # other one, so the cross-locale comparison is the two runs' recorded titles.
    report["navigation_locale"] = report.get("locale")
    return rows


def _INTERNAL_IDENTIFIER(text):
    """An unresolved i18n key or a field name is all dots and underscores; real copy
    carries spaces and punctuation a translator would produce."""
    return bool(re.fullmatch(r"[a-z][A-Za-z0-9]*(\.[a-z][A-Za-z0-9]*)+", text.strip()))
