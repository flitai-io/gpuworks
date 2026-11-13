"""Instance container shell / real PTY output, target rejection and release.

Run: make test-e2e-container-terminal E2E_LOCALE=en
No host terminal, arbitrary network outage or GPU behavior is claimed.
"""
import json
import shlex
from urllib.parse import parse_qs, urlencode, urlsplit

from playwright.sync_api import expect
from platform_token_read_revoke import check
import instance_diagnostics_browser as diagnostics
import instance_diagnostics_fixture as runtime
from local_browser_fixture import http
import container_terminal_fixture as fixture


def observe(target, holder):
    record = {"preflight": [], "sockets": [], "controls": [], "errors": []}
    holder.setdefault("terminal_pages", []).append(target)
    target.on("pageerror", lambda error: record["errors"].append(str(error)))
    def response_seen(response):
        if urlsplit(response.url).path.endswith("/terminal/settings"):
            record["preflight"].append(response)
    target.on("response", response_seen)
    def socket_seen(socket):
        record["sockets"].append(socket.url)
        def received(payload):
            if isinstance(payload, str):
                record["controls"].append(json.loads(payload))
        def sent(payload):
            # The browser's own frames: keystrokes and resize reports. A resize the UI never sends
            # would leave the agent's pty at its original geometry.
            if isinstance(payload, str):
                record.setdefault("sent", []).append(json.loads(payload))
        socket.on("framereceived", received)
        socket.on("framesent", sent)
    target.on("websocket", socket_seen)
    return record


def new_context(context, holder, locale, authenticated=True):
    options = {"locale": "zh-CN" if locale == "zh" else "en-US", "timezone_id": "UTC",
               "viewport": {"width": 1440, "height": 900}}
    if authenticated:
        options["storage_state"] = context.storage_state()
    child = context.browser.new_context(**options)
    observed = {"context": child, "closed": False}
    child.on("close", lambda _: observed.update(closed=True))
    holder.setdefault("terminal_contexts", []).append(observed)
    return child


def popup_from_instance(page, panel, context, holder, text):
    records = {}
    def created(target):
        records[target] = observe(target, holder)
    context.on("page", created)
    try:
        with context.expect_page() as popup:
            panel.get_by_role("button", name=text["instanceTerminal"]["open"], exact=True).click()
        target = popup.value
        target.wait_for_load_state()
        return target, records[target]
    finally:
        context.remove_listener("page", created)


def verify_connected(target, record, server, node_uuid, ref, nodes):
    expect(target.get_by_text(nodes["terminalView"]["status"]["open"], exact=True)).to_be_visible()
    expect(target.locator(".xterm")).to_be_visible()
    query = {"target": ["container"], "ref": [ref]}
    check(urlsplit(target.url).path == "/nodes/" + node_uuid + "/terminal"
          and parse_qs(urlsplit(target.url).query) == query, "container terminal page target differs")
    check(len(record["preflight"]) == 1, "terminal preflight request count differs")
    pre = record["preflight"][0]
    diagnostics.request_contract(pre, "/api/v1/nodes/" + node_uuid + "/terminal/settings", {})
    value = pre.json()
    fixture.assert_preflight(value)
    check(value["enabled"] is True and value["can_open"] is True and value["reason"] == ""
          and value["active_node"] == 0 and value["active_total"] == 0
          and value["container_supported"] is True and value["container_runtime"] == "docker"
          and value["container_capability"] == "webterm.container" and value["container_reason"] == "",
          "container admission/capability differs")
    check(len(record["sockets"]) == 1, "one terminal page must open exactly one WS")
    wire = urlsplit(record["sockets"][0])
    params = parse_qs(wire.query)
    check(wire.path == "/api/v1/nodes/" + node_uuid + "/terminal"
          and set(params) == {"target", "ref", "rows", "cols"}
          and params["target"] == ["container"] and params["ref"] == [ref],
          "actual WS target differs or leaked a URL token")
    check(all(len(params[key]) == 1 and params[key][0].isdigit() and 1 <= int(params[key][0]) <= 1000
              for key in ("rows", "cols")), "WS terminal geometry is invalid")
    return params


def execute(target, data, report, mode, params):
    stem = "/tmp/e2e-" + report["run_id"] + "-" + mode
    marker = "e2e-" + report["run_id"] + "-" + mode + " 中文终端"
    command = ("umask 077; printf '%s\\n' \"$$\" > " + shlex.quote(stem + ".pid")
        + "; cat /proc/$$/stat > " + shlex.quote(stem + ".stat")
        + "; stty size > " + shlex.quote(stem + ".size")
        + "; printf 'e2e-%s-%s 中文终端\\n' " + shlex.quote(report["run_id"]) + " " + shlex.quote(mode)
        + " > " + shlex.quote(stem + ".txt") + "; cat " + shlex.quote(stem + ".txt"))
    check(marker not in command, "PTY input echo must not satisfy the output assertion")
    target.locator(".xterm").click()
    target.keyboard.insert_text(command)
    target.keyboard.press("Enter")
    expect(target.locator(".xterm-rows")).to_contain_text(marker)
    content = runtime.docker("exec", data["container"], "cat", stem + ".txt")
    check(content == (marker + "\n").encode(), "PTY command did not write exact bytes into target container")
    pid = runtime.docker("exec", data["container"], "cat", stem + ".pid").decode().strip()
    check(pid.isdigit() and int(pid) > 1, "container PTY did not report its shell PID")
    identity = fixture.process_identity(runtime.docker("exec", data["container"], "cat", stem + ".stat").decode().strip(), int(pid))
    check(fixture.observed_process(data["container"], identity) == identity, "PTY shell is not alive in the selected container")
    geometry = runtime.docker("exec", data["container"], "cat", stem + ".size").decode().split()
    check(geometry == [params["rows"][0], params["cols"][0]], "container PTY geometry does not match actual browser WS")
    report.setdefault("terminal_processes", []).append({"mode": mode, "container": data["container"],
        "pid": identity[0], "start_ticks": identity[1], "file": stem + ".txt", "expected_bytes_hex": content.hex()})
    return identity


def verify_resize(target, record, data, report, stem, params):
    """19.3: a viewport change must be reported to the pty, and the pty must adopt it.

    The browser is resized, the UI is required to send a resize frame carrying new geometry, and the
    container's own shell is then asked for its size again - so a UI that resized only its canvas, or
    a frame that the server dropped, would leave the pty at the old geometry and fail here.
    """
    before = (int(params["rows"][0]), int(params["cols"][0]))
    record.setdefault("sent", []).clear()
    target.set_viewport_size({"width": 1000, "height": 620})
    resized = None
    for _ in range(80):
        for frame in record.get("sent", []):
            if frame.get("type") == "resize":
                resized = frame
                break
        if resized:
            break
        target.wait_for_timeout(250)
    check(resized is not None, "the browser never reported its new geometry to the terminal")
    after = (int(resized["rows"]), int(resized["cols"]))
    check(after != before, f"the resize frame repeated the old geometry {before} -> {after}")
    check(all(isinstance(v, int) and 1 <= v <= 1000 for v in after),
          f"the resize frame carries an invalid geometry: {after}")
    state = "/tmp/e2e-" + report["run_id"] + "-resize.size"
    target.locator(".xterm").click()
    target.keyboard.insert_text("stty size > " + shlex.quote(state))
    target.keyboard.press("Enter")
    observed = None
    for _ in range(40):
        try:
            observed = runtime.docker("exec", data["container"], "cat", state).decode().split()
        except Exception:
            observed = None
        if observed and len(observed) == 2:
            break
        target.wait_for_timeout(250)
    check(observed is not None, "the pty size was not readable after the resize")
    check((int(observed[0]), int(observed[1])) == after,
          f"the pty did not adopt the reported geometry: frame {after}, stty {observed}")
    target.set_viewport_size({"width": 1440, "height": 900})
    report.setdefault("resize", []).append({"from": list(before), "to": list(after),
                                            "pty": observed})
    return after


# Mainline: instance's container shell button → exact container WS/real PTY
# → independently read marker file and shell identity → exit / tab close /
# context disconnect → audit ended, process gone and concurrency zero; wrong
# ref exits without shell, read-only identity is rejected before WS admission.
def test_container_terminal(page, context, server, artifacts, report, locale, data, token, holder):
    text, nodes = diagnostics.strings(locale), diagnostics.strings(locale, "nodes")
    diagnostics.login(page, context, server, artifacts, report, locale)
    _, panel = diagnostics.open_instance(page, server, data, report)
    node_uuid, ref = data["node"]["uuid"], data["container"]
    evidence, completed = [], []
    for mode in ("exit", "close", "disconnect", "network"):
        report["step"] = "container PTY " + mode
        prior = {row["id"] for row in fixture.audits(server, token)}
        child = None
        if mode in ("disconnect", "network"):
            child = new_context(context, holder, locale)
            target = child.new_page()
            record = observe(target, holder)
            target.goto(server.base + "/nodes/" + node_uuid + "/terminal?" + urlencode({"target": "container", "ref": ref}))
        else:
            target, record = popup_from_instance(page, panel, context, holder, text)
        params = verify_connected(target, record, server, node_uuid, ref, nodes)
        audit = fixture.wait_audit(page, server, token, node_uuid, ref, prior, True)
        identity = execute(target, data, report, mode, params)
        if mode == "exit":
            report["step"] = "container PTY resize reaches the pty"
            verify_resize(target, record, data, report,
                          "/tmp/e2e-" + report["run_id"] + "-exit", params)
        if mode == "exit":
            target.keyboard.type("exit")
            target.keyboard.press("Enter")
            expect(target.get_by_text(nodes["terminalView"]["status"]["exited"], exact=True)).to_be_visible()
            check(fixture.exit_code(record["controls"]) == 0,
                  "shell exit did not deliver a successful actual WS exit frame")
            target.close()
        elif mode == "close":
            target.close()
        elif mode == "network":
            # A real network break rather than a tidy close: the browser loses the network, so the
            # socket dies without a close handshake and the page cannot clean up after itself. The
            # server must still end the session and reap the pty, which is the half of 19.4 that a
            # deliberate tab close never exercises.
            child.set_offline(True)
            for _ in range(80):
                if fixture.admission(server, token, node_uuid)["active_node"] == 0:
                    break
                target.wait_for_timeout(250)
            child.set_offline(False)
            child.close()
        else:
            child.close()
        check(target.is_closed(), "terminal page did not close")
        ended = fixture.wait_released(page, server, token, node_uuid, ref, identity, audit["id"])
        check(ended["close_reason"] == ("shell_exited" if mode == "exit" else "client_closed"),
              f"terminal audit close cause differs in {mode}: {ended['close_reason']}")
        check(record["errors"] == [], "container terminal page JS errors")
        evidence.append({"mode": mode, "audit_id": audit["id"], "close_reason": ended["close_reason"],
            "target": "container", "ref": ref, "ws_query": params, "process_released": list(identity)})
        completed.append(mode)
    report["last_step"] = "container-exit-close-context-disconnect-released"

    report["step"] = "nonexistent container reference"
    missing = "e2e-" + report["run_id"] + "-missing"
    prior = {row["id"] for row in fixture.audits(server, token)}
    rejected = context.new_page()
    rejected_record = observe(rejected, holder)
    rejected.goto(server.base + "/nodes/" + node_uuid + "/terminal?" + urlencode({"target": "container", "ref": missing}))
    expect(rejected.get_by_text(nodes["terminalView"]["status"]["exited"], exact=True)).to_be_visible()
    expect(rejected.locator(".xterm-rows")).to_contain_text("No such container: " + missing)
    check(fixture.exit_code(rejected_record["controls"]) != 0, "wrong container did not return a failing WS exit")
    wrong_audit = fixture.wait_audit(page, server, token, node_uuid, missing, prior, False)
    check(fixture.admission(server, token, node_uuid)["active_total"] == 0, "wrong ref consumed a concurrency slot")
    rejected.close()
    check(runtime.inspect(ref)["Id"] == ref, "wrong reference changed the real target")

    # 19.5: the per-node concurrency limit and the idle policy are both real. Sessions are opened
    # until the node reports itself full, then one more page is opened and required to be refused
    # with the busy reason rather than admitted; the refused page must leave no session behind, and
    # closing the held ones must return the node to empty. The idle timeout is required to be a
    # positive number of minutes reported by the server, because a limit of zero would silently
    # mean "never" and the notice it drives would be unreachable.
    report["step"] = "concurrency limit refuses and releases"
    stream_path = ("/api/v1/nodes/" + node_uuid + "/terminal?"
                   + urlencode({"target": "container", "ref": ref}))
    policy = fixture.admission(server, token, node_uuid)
    per_node = policy["max_per_node"]
    check(isinstance(per_node, int) and per_node >= 1, f"per-node terminal limit is not set: {per_node}")
    check(isinstance(policy["idle_timeout_min"], int) and policy["idle_timeout_min"] >= 1,
          f"idle timeout is not a positive number of minutes: {policy['idle_timeout_min']}")
    prior = fixture.audits(server, token)
    held = []
    try:
        for index in range(per_node):
            extra = new_context(context, holder, locale)
            extra_page = extra.new_page()
            observe(extra_page, holder)
            extra_page.goto(server.base + "/nodes/" + node_uuid + "/terminal?"
                            + urlencode({"target": "container", "ref": ref}))
            expect(extra_page.locator(".xterm-rows")).to_be_visible()
            # The admission is counted by the server when the WS begins, so the next attempt is
            # gated on the server seeing this one rather than on the page having rendered.
            for _ in range(60):
                if fixture.admission(server, token, node_uuid)["active_node"] == index + 1:
                    break
                extra_page.wait_for_timeout(250)
            else:
                check(False, f"held terminal {index + 1} never became an active session")
            held.append((extra, extra_page))
        full = fixture.admission(server, token, node_uuid)
        check(full["can_open"] is False,
              f"the node admitted past its limit of {per_node}: {json.dumps(full)}")
        check(full["reason"] != "", "a full node reported no reason")
        check(full["active_node"] >= per_node, f"active_node {full['active_node']} below the limit {per_node}")
        # The fifth attempt must be refused at admission. It is driven from a page that is already
        # signed in, and the socket is opened directly so the outcome is the server's admission
        # answer rather than a rendering detail.
        refused_ws = held[0][1].evaluate(
            """async ([path]) => {
                return await new Promise((resolve) => {
                    const ws = new WebSocket((location.protocol === "https:" ? "wss:" : "ws:")
                        + "//" + location.host + path);
                    ws.onopen = () => { ws.close(); resolve({ opened: true }); };
                    ws.onerror = () => resolve({ opened: false });
                    setTimeout(() => { try { ws.close(); } catch (e) {} resolve({ opened: false }); }, 8000);
                });
            }""", [stream_path])
        check(refused_ws["opened"] is False, "a full node accepted another terminal socket")
        full_after = fixture.admission(server, token, node_uuid)
        check(full_after["active_node"] <= per_node,
              f"the refused attempt pushed active_node past the limit: {full_after['active_node']}")
        report["concurrency"] = {"max_per_node": per_node, "idle_timeout_min": policy["idle_timeout_min"],
                                 "refused_reason": full["reason"], "refused_opened_ws": False}
    finally:
        for extra, extra_page in held:
            extra_page.close()
            extra.close()
    released = fixture.admission(server, token, node_uuid)
    check(released["active_node"] == 0 and released["can_open"] is True,
          f"closing the held terminals did not release the node: {released}")
    report["last_step"] = "concurrency-limit-refused-and-released"

    # 19.1: the kill switch is real and it is a kill switch, not a label. The terminal policy is
    # switched off through the settings route, and the preflight is required to say it cannot open
    # with the disabled reason while a socket attempt is refused. The policy is then restored and the
    # preflight is required to allow again, so a value that only decorated the preflight would fail.
    report["step"] = "kill switch refuses and restores"
    settings_key = "/api/v1/settings/terminal"
    original_status, original_policy = http(server.base, "GET", "/api/v1/nodes/" + node_uuid
                                            + "/terminal/settings", token=token)
    check(original_status == 200, "terminal preflight is unreadable before the kill switch")
    settings_status, current_settings = http(server.base, "GET", "/api/v1/settings", token=token)
    check(settings_status == 200, "settings are unreadable")
    seeded = current_settings.get("terminal")
    check(isinstance(seeded, dict), f"the terminal policy is not readable from settings: {seeded}")
    disabled = dict(seeded)
    disabled["enabled"] = False
    off_status, off_body = http(server.base, "PUT", settings_key, token=token, body=disabled)
    check(off_status == 200, f"the terminal kill switch was refused: {off_status} {off_body}")
    try:
        off = fixture.admission(server, token, node_uuid)
        check(off["enabled"] is False, f"the kill switch did not take effect: {json.dumps(off)}")
        check(off["can_open"] is False and off["reason"] != "",
              f"a disabled terminal still allowed a session: {json.dumps(off)}")
        check(off["reason"] == "web terminal is disabled in platform settings", f"the disabled reason differs: {off['reason']}")
        blocked = page.evaluate(
            """async ([path]) => await new Promise((resolve) => {
                const ws = new WebSocket((location.protocol === "https:" ? "wss:" : "ws:")
                    + "//" + location.host + path);
                ws.onopen = () => { ws.close(); resolve({ opened: true }); };
                ws.onerror = () => resolve({ opened: false });
                setTimeout(() => { try { ws.close(); } catch (e) {} resolve({ opened: false }); }, 8000);
            })""", [stream_path])
        check(blocked["opened"] is False, "a disabled terminal accepted a socket")
        report["kill_switch"] = {"can_open": off["can_open"], "reason": off["reason"],
                                 "socket_refused": True}
    finally:
        on_status, on_body = http(server.base, "PUT", settings_key, token=token, body=disabled | {"enabled": True})
        check(on_status == 200, f"the terminal kill switch could not be restored: {on_body}")
    restored = fixture.admission(server, token, node_uuid)
    check(restored["enabled"] is True and restored["can_open"] is True,
          f"the terminal did not come back after the kill switch: {json.dumps(restored)}")
    report["last_step"] = "kill-switch-refused-and-restored"

    report["step"] = "read-only principal rejected before WS"
    prior = fixture.audits(server, token)
    reader = new_context(context, holder, locale, authenticated=False)
    reader_page = reader.new_page()
    diagnostics.login(reader_page, reader, server, artifacts, report, locale, data["username"])
    denied = observe(reader_page, holder)
    with reader_page.expect_response(lambda r: urlsplit(r.url).path == "/api/v1/nodes/" + node_uuid + "/terminal/settings") as refusal:
        reader_page.goto(server.base + "/nodes/" + node_uuid + "/terminal?" + urlencode({"target": "container", "ref": ref}))
    check(refusal.value.status == 403 and refusal.value.json().get("code") == "ADMINISTRATOR_REQUIRED",
          "read-only principal did not get exact administrator denial")
    common = diagnostics.strings(locale, "common")
    expect(reader_page.get_by_role("alert")).to_contain_text(common["apiError"]["ADMINISTRATOR_REQUIRED"])
    check(denied["sockets"] == [], "denied principal opened a terminal WS")
    check(fixture.audits(server, token) == prior, "denied principal created an audit/session")
    reader.close()
    report["evidence"].extend([
        {"layer": "ui", "container_entry": text["instanceTerminal"]["open"], "outcomes": completed,
         "wrong_ref": missing, "permission_error": "ADMINISTRATOR_REQUIRED"},
        {"layer": "request", "sessions": evidence, "permission_preflight": {"status": 403, "code": "ADMINISTRATOR_REQUIRED"}},
        {"layer": "persistence", "audit_records": evidence, "wrong_target_audit_id": wrong_audit["id"],
         "permission_no_session": True},
        {"layer": "execution", "real_container_shells": report["terminal_processes"],
         "active_node": 0, "active_total": 0}])
    report["last_step"] = "container-terminal-failures-and-release-complete"
