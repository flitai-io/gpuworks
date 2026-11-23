"""Edit cycles for the telemetry export card.

The collector address, the export switch, the trace link, and one header each
change while the other fields stay. Export is left off so the later test span
still sees the seeded setting.
"""
from __future__ import annotations

import re
from urllib.parse import urlsplit

from playwright.sync_api import TimeoutError as PlaywrightTimeout
from playwright.sync_api import expect
from settings_confirm import confirm_setting
from platform_token_read_revoke import check

TELEMETRY = "/api/v1/telemetry/export"
ADDRESS_A = "http://collector-a.example"
ADDRESS_B = "http://collector-b.example"
TRACE_LINK = "https://traces.example/cycle/{trace_id}"
OPTIONAL_ENDPOINT = "http://collector-optional.example"
OPTIONAL_TRACE = "https://traces.example/optional/{trace_id}"
OPTIONAL_HEADER = "X-Optional"
OPTIONAL_VALUE = "optional-1"
_DEFAULTS = {"enabled": False, "endpoint": "", "headers": [], "sample_ratio": 1, "trace_link": ""}


def _format(template: str, **values) -> str:
    for key, value in values.items():
        template = template.replace("{{" + key + "}}", str(value))
    return template


def _accessible(label: str) -> str:
    return (r"\s*".join(label) if re.fullmatch(r"[㐀-鿿]{2}", label) else re.escape(label))


def _save(page, card, base, save_name, body):
    save = card.get_by_role("button", name=re.compile(_accessible(save_name))).first
    expect(save).to_be_enabled()
    with page.expect_response(lambda response: urlsplit(response.url).path == TELEMETRY
                              and response.request.method == "PUT") as saved:
        confirm_setting(page, save)
    check(saved.value.status == 200, f"telemetry cycle save HTTP {saved.value.status}")
    check(saved.value.request.post_data_json == body,
          f"telemetry cycle body {saved.value.request.post_data_json}")
    read = page.request.get(base + TELEMETRY)
    check(read.status == 200, f"telemetry cycle GET HTTP {read.status}")
    doc = read.json()
    headers = [{"name": item["name"]} for item in body["headers"]]
    check(doc["enabled"] is body["enabled"] and doc["endpoint"] == body["endpoint"]
          and doc["sample_ratio"] == body["sample_ratio"]
          and doc.get("trace_link", "") == body["trace_link"]
          and doc["headers"] == headers,
          f"telemetry cycle readback {doc}")
    return doc


def exercise_telemetry_minimum(page, card, telemetry, base, save_name, tab_name):
    """Save the untouched defaults. No optional field is filled."""
    label = re.compile("^" + _accessible(save_name) + "$")
    expect(card.get_by_role("button", name=label)).to_have_count(1)
    switch = card.get_by_role("switch", name=telemetry["enabled"], exact=True)
    endpoint = card.get_by_label(telemetry["endpoint"], exact=True)
    ratio = card.get_by_label(telemetry["sampleRatio"], exact=True)
    trace = card.get_by_label(telemetry["traceLink"], exact=True)
    expect(switch).not_to_be_checked()
    expect(endpoint).to_have_value("")
    expect(ratio).to_have_value("100")
    expect(trace).to_have_value("")
    expect(card.get_by_role("textbox")).to_have_count(2)
    save = card.get_by_role("button", name=label).first
    with page.expect_response(lambda response: urlsplit(response.url).path == TELEMETRY
                              and response.request.method == "PUT") as saved:
        confirm_setting(page, save)
    check(saved.value.status == 200, f"minimum telemetry save HTTP {saved.value.status}")
    check(saved.value.request.post_data_json == _DEFAULTS,
          f"minimum telemetry body {saved.value.request.post_data_json}")
    expect(page.get_by_text(telemetry["saved"], exact=True).first).to_be_visible()
    read = page.request.get(base + TELEMETRY)
    check(read.status == 200, f"minimum telemetry GET HTTP {read.status}")
    doc = read.json()
    check(doc["enabled"] is False and doc["endpoint"] == "" and doc["headers"] == []
          and doc["sample_ratio"] == 1 and doc.get("trace_link", "") == "",
          f"minimum telemetry readback {doc}")
    page.reload()
    expect(page.get_by_role("tab", name=tab_name, exact=True)).to_have_attribute("aria-selected", "true")
    refreshed = page.locator(".ant-card").filter(has_text=telemetry["title"]).first
    expect(refreshed.get_by_role("switch", name=telemetry["enabled"], exact=True)).not_to_be_checked()
    expect(refreshed.get_by_label(telemetry["endpoint"], exact=True)).to_have_value("")
    expect(refreshed.get_by_label(telemetry["sampleRatio"], exact=True)).to_have_value("100")
    expect(refreshed.get_by_label(telemetry["traceLink"], exact=True)).to_have_value("")
    return {
        "fields_filled": 0,
        "primary_button": True,
        "method": "PUT",
        "path": TELEMETRY,
        "status": 200,
        "body": dict(_DEFAULTS),
        "response_matches_get": True,
        "reload_matches": True,
    }


def exercise_telemetry_optionals(page, card, telemetry, base, save_name):
    """Every optional field is stored, and a body that omits them returns defaults."""
    endpoint = card.get_by_label(telemetry["endpoint"], exact=True)
    switch = card.get_by_role("switch", name=telemetry["enabled"], exact=True)
    ratio = card.get_by_label(telemetry["sampleRatio"], exact=True)
    trace = card.get_by_label(telemetry["traceLink"], exact=True)
    expect(switch).not_to_be_checked()
    expect(endpoint).to_have_value("")
    expect(ratio).to_have_value("100")
    expect(trace).to_have_value("")
    endpoint.fill(OPTIONAL_ENDPOINT)
    card.get_by_role("button", name=re.compile(_accessible(telemetry["addHeader"]))).first.click()
    name = card.get_by_label(_format(telemetry["headerName"], n=1), exact=True)
    value = card.get_by_label(_format(telemetry["headerValueOf"], n=1), exact=True)
    name.fill(OPTIONAL_HEADER)
    value.fill(OPTIONAL_VALUE)
    ratio.fill("50")
    trace.fill(OPTIONAL_TRACE)
    filled = {
        "enabled": False, "endpoint": OPTIONAL_ENDPOINT,
        "headers": [{"name": OPTIONAL_HEADER, "value": OPTIONAL_VALUE}],
        "sample_ratio": 0.5, "trace_link": OPTIONAL_TRACE,
    }
    _save(page, card, base, save_name, filled)
    stored = page.request.get(base + TELEMETRY)
    check(stored.status == 200 and OPTIONAL_VALUE not in stored.text(),
          "the optional header value was returned")
    omitted = page.request.put(base + TELEMETRY, data="{}")
    check(omitted.status == 200, f"omitted optionals PUT HTTP {omitted.status}")
    read = page.request.get(base + TELEMETRY)
    check(read.status == 200, f"omitted optionals GET HTTP {read.status}")
    doc = read.json()
    check(doc["enabled"] is False and doc["endpoint"] == "" and doc["headers"] == []
          and doc["sample_ratio"] == 1 and doc.get("trace_link", "") == ""
          and OPTIONAL_VALUE not in read.text(),
          f"omitted optionals did not return backend defaults: {doc}")
    endpoint.fill("")
    ratio.fill("100")
    trace.fill("")
    card.get_by_role("button", name=_format(telemetry["removeHeader"], n=1), exact=True).click()
    _save(page, card, base, save_name, _DEFAULTS)
    return {"filled": filled, "omitted_status": 200, "defaults": dict(_DEFAULTS)}


def exercise_telemetry_field_cycles(page, card, telemetry, base, save_name):
    """A→B, on→off→on, value→empty, and header A→B→none, then restore defaults."""
    optionals = exercise_telemetry_optionals(page, card, telemetry, base, save_name)
    endpoint = card.get_by_label(telemetry["endpoint"], exact=True)
    switch = card.get_by_role("switch", name=telemetry["enabled"], exact=True)
    ratio = card.get_by_label(telemetry["sampleRatio"], exact=True)
    trace = card.get_by_label(telemetry["traceLink"], exact=True)
    endpoint.fill(ADDRESS_A)
    expect(endpoint).to_have_value(ADDRESS_A)
    endpoint.fill(ADDRESS_B)
    expect(endpoint).to_have_value(ADDRESS_B)
    expect(switch).not_to_be_checked()
    expect(ratio).to_have_value("100")
    expect(trace).to_have_value("")
    _save(page, card, base, save_name, {
        "enabled": False, "endpoint": ADDRESS_B, "headers": [],
        "sample_ratio": 1, "trace_link": "",
    })

    switch.click()
    expect(switch).to_be_checked()
    expect(endpoint).to_have_value(ADDRESS_B)
    switch.click()
    expect(switch).not_to_be_checked()
    expect(endpoint).to_have_value(ADDRESS_B)
    switch.click()
    expect(switch).to_be_checked()
    expect(endpoint).to_have_value(ADDRESS_B)
    expect(ratio).to_have_value("100")
    expect(trace).to_have_value("")
    switch.click()
    expect(switch).not_to_be_checked()
    expect(endpoint).to_have_value(ADDRESS_B)
    _save(page, card, base, save_name, {
        "enabled": False, "endpoint": ADDRESS_B, "headers": [],
        "sample_ratio": 1, "trace_link": "",
    })

    trace.fill(TRACE_LINK)
    expect(trace).to_have_value(TRACE_LINK)
    expect(endpoint).to_have_value(ADDRESS_B)
    _save(page, card, base, save_name, {
        "enabled": False, "endpoint": ADDRESS_B, "headers": [],
        "sample_ratio": 1, "trace_link": TRACE_LINK,
    })
    trace.fill("")
    expect(trace).to_have_value("")
    expect(switch).not_to_be_checked()
    expect(ratio).to_have_value("100")
    _save(page, card, base, save_name, {
        "enabled": False, "endpoint": ADDRESS_B, "headers": [],
        "sample_ratio": 1, "trace_link": "",
    })

    card.get_by_role("button", name=re.compile(_accessible(telemetry["addHeader"]))).first.click()
    name = card.get_by_label(_format(telemetry["headerName"], n=1), exact=True)
    value = card.get_by_label(_format(telemetry["headerValueOf"], n=1), exact=True)
    name.fill("Alpha")
    value.fill("one")
    _save(page, card, base, save_name, {
        "enabled": False, "endpoint": ADDRESS_B,
        "headers": [{"name": "Alpha", "value": "one"}],
        "sample_ratio": 1, "trace_link": "",
    })
    name = card.get_by_label(_format(telemetry["headerName"], n=1), exact=True)
    value = card.get_by_label(_format(telemetry["headerValueOf"], n=1), exact=True)
    expect(name).to_have_value("Alpha")
    name.fill("Beta")
    value.fill("two")
    _save(page, card, base, save_name, {
        "enabled": False, "endpoint": ADDRESS_B,
        "headers": [{"name": "Beta", "value": "two"}],
        "sample_ratio": 1, "trace_link": "",
    })
    card.get_by_role("button", name=_format(telemetry["removeHeader"], n=1), exact=True).click()
    expect(endpoint).to_have_value(ADDRESS_B)
    expect(switch).not_to_be_checked()
    expect(ratio).to_have_value("100")
    expect(trace).to_have_value("")
    _save(page, card, base, save_name, {
        "enabled": False, "endpoint": ADDRESS_B, "headers": [],
        "sample_ratio": 1, "trace_link": "",
    })

    endpoint.fill("")
    expect(endpoint).to_have_value("")
    _save(page, card, base, save_name, {
        "enabled": False, "endpoint": "", "headers": [],
        "sample_ratio": 1, "trace_link": "",
    })
    return {
        "address_from": ADDRESS_A,
        "address_to": ADDRESS_B,
        "switch": ["on", "off", "on"],
        "trace_link": TRACE_LINK,
        "trace_cleared": "",
        "headers": ["Alpha", "Beta", []],
        "restored": {"enabled": False, "endpoint": "", "headers": [], "sample_ratio": 1, "trace_link": ""},
        "optionals": optionals,
    }


OTHER_ENDPOINT = "http://other-session.example"


def exercise_telemetry_conflict(page, card, telemetry, base, save_name):
    """Another session replaces the stored export; the open draft is still saved."""
    endpoint = card.get_by_label(telemetry["endpoint"], exact=True)
    switch = card.get_by_role("switch", name=telemetry["enabled"], exact=True)
    ratio = card.get_by_label(telemetry["sampleRatio"], exact=True)
    expect(endpoint).to_have_value("")
    expect(switch).not_to_be_checked()
    expect(ratio).to_have_value("100")
    other = {**_DEFAULTS, "endpoint": OTHER_ENDPOINT}
    wrote = page.request.put(base + TELEMETRY, data=other)
    check(wrote.status == 200, f"the other session was rejected: HTTP {wrote.status}")
    read = page.request.get(base + TELEMETRY)
    check(read.status == 200, f"other-session GET HTTP {read.status}")
    doc = read.json()
    check(doc["enabled"] is False and doc["endpoint"] == OTHER_ENDPOINT
          and doc["headers"] == [] and doc["sample_ratio"] == 1
          and doc.get("trace_link", "") == "",
          f"the other session was not stored: {doc}")
    expect(endpoint).to_have_value("")
    expect(switch).not_to_be_checked()
    expect(ratio).to_have_value("100")
    _save(page, card, base, save_name, _DEFAULTS)
    return {"other_session": 200,
            "other_read": other,
            "draft": {"endpoint": "", "enabled": False, "ratio": "100"},
            "saved_status": 200, "saved_read": dict(_DEFAULTS),
            "conflict_status": False}


def _box(locator):
    locator.scroll_into_view_if_needed()
    return locator.evaluate(
        """el => {
          const r = el.getBoundingClientRect();
          return {left: r.left, top: r.top, right: r.right, bottom: r.bottom,
                  width: r.width, height: r.height,
                  pageLeft: r.left + window.scrollX, pageTop: r.top + window.scrollY};
        }""")


def _overlaps(a, b):
    return not (a["pageLeft"] + a["width"] <= b["pageLeft"]
                or b["pageLeft"] + b["width"] <= a["pageLeft"]
                or a["pageTop"] + a["height"] <= b["pageTop"]
                or b["pageTop"] + b["height"] <= a["pageTop"])


def exercise_telemetry_keyboard(page, card, telemetry, save_name, puts):
    """Tab reaches each field, Enter does not save, and the controls fit 1440×900."""
    switch = card.get_by_role("switch", name=telemetry["enabled"], exact=True)
    endpoint = card.get_by_label(telemetry["endpoint"], exact=True)
    add = card.get_by_role("button", name=re.compile(_accessible(telemetry["addHeader"]))).first
    ratio = card.get_by_label(telemetry["sampleRatio"], exact=True)
    trace = card.get_by_label(telemetry["traceLink"], exact=True)
    save = card.get_by_role("button", name=re.compile(_accessible(save_name))).first
    test = card.get_by_role("button", name=re.compile(_accessible(telemetry["test"]))).first
    switch.focus()
    expect(switch).to_be_focused()
    switch.press("Tab")
    expect(endpoint).to_be_focused()
    endpoint.press("Tab")
    expect(add).to_be_focused()
    add.press("Tab")
    expect(ratio).to_be_focused()
    ratio.press("Tab")
    expect(trace).to_be_focused()
    save.focus()
    expect(save).to_be_focused()
    before = len(puts)
    try:
        with page.expect_request(lambda request: urlsplit(request.url).path == TELEMETRY
                                  and request.method == "PUT", timeout=1000):
            endpoint.press("Enter")
        submitted = True
    except PlaywrightTimeout:
        submitted = False
    check(not submitted, "Enter submitted telemetry")
    expect(endpoint).to_have_value("")
    check(len(puts) == before, "Enter recorded a telemetry PUT")
    view = page.viewport_size
    check(view == {"width": 1440, "height": 900}, f"viewport {view}")
    boxes = {name: _box(locator) for name, locator in (
        ("endpoint", endpoint), ("ratio", ratio), ("trace", trace), ("save", save), ("test", test))}
    for name, box in boxes.items():
        check(box["width"] > 0 and box["height"] > 0, f"{name} has no box")
        check(-1 <= box["left"] and box["right"] <= view["width"] + 1
              and -1 <= box["top"] and box["bottom"] <= view["height"] + 1,
              f"{name} is outside the viewport {box}")
    check(not _overlaps(boxes["endpoint"], boxes["save"]), "the address overlaps Save")
    check(not _overlaps(boxes["trace"], boxes["test"]), "the trace link overlaps the test button")
    check(not _overlaps(boxes["ratio"], boxes["test"]), "the sample ratio overlaps the test button")
    return {"tab": ["switch", "address", "add header", "ratio", "trace", "save"],
            "enter_submitted": False, "viewport": view,
            "outside": [], "overlaps": []}
