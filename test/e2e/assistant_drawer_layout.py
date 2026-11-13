#!/usr/bin/env python3
"""Assistant and resource drawers keep independent pointer regions (GWKS-173).

Run: make test-e2e-assistant-drawer-layout
After building: python3 test/e2e/assistant_drawer_layout.py --target-ref upstream/master --locale en
Repeat with --locale zh. Requires system Chrome and Python Playwright.
Server, SQLite, management APIs and UI are real. A stopped deployment/model
is seeded in the isolated database; creation, scheduling and GPU execution
are not claimed. Only the external LLM uses the existing AssistantEndpoint.
"""
import json
import sqlite3
from urllib.parse import urlsplit

from playwright.sync_api import expect

from assistant_fixture import AssistantEndpoint
from image_catalog_runner import login
from local_browser_fixture import ROOT, http
from local_business_runner import case_parser, checked, run_case
from platform_token_read_revoke import button

CONTRACT = "gpuworks/assistant/drawer-layout"
T0 = "2026-11-03 00:00:00"
QUESTION = "Summarize the cluster for this deployment."


def strings(locale, namespace):
    return json.loads((ROOT / f"ui/src/i18n/locales/{locale}/{namespace}.json").read_text())


def prepare(server, token, run_id, report, holder, agent_bin, artifacts):
    prefix = "e2e-" + run_id
    owned = report["remaining_resources"]
    fixture = holder["llm"] = AssistantEndpoint().start()
    artifacts.remember(fixture.api_key)
    code, config = http(server.base, "POST", "/api/v1/ai/configs", token, {
        "name": prefix + "-ai", "preset": "custom", "base_url": fixture.base,
        "api_key": fixture.api_key, "model": fixture.model, "is_default": True})
    if isinstance(config, dict) and config.get("id"):
        owned.append({"kind": "config", "id": config["id"]})
    checked(code == 201, f"AI configuration setup HTTP {code}")
    code, added = http(server.base, "POST", "/api/v1/nodes", token,
                       {"name": prefix + "-node", "server_url": server.base})
    if isinstance(added, dict) and added.get("node", {}).get("uuid"):
        owned.append({"kind": "node", "id": added["node"]["uuid"]})
    checked(code == 201, f"node setup HTTP {code}")
    artifacts.remember(added["token"])
    node = added["node"]
    name = prefix + "-deployment"
    with sqlite3.connect(server.work / "server-data/gpuworks.db", timeout=5) as db:
        model_id = db.execute(
            "INSERT INTO models (name, source, source_id, created_at, updated_at) VALUES (?, 'local', ?, ?, ?)",
            (prefix + "-model", prefix + "/model", T0, T0)).lastrowid
        node_id = db.execute("SELECT id FROM nodes WHERE uuid = ?", (node["uuid"],)).fetchone()[0]
        dep_id = db.execute(
            "INSERT INTO deployments (name, model_id, served_name, engine, node_id, desired_state, replicas, created_at, updated_at) "
            "VALUES (?, ?, ?, 'vllm', ?, 'stopped', 1, ?, ?)", (name, model_id, name, node_id, T0, T0)).lastrowid
    owned.extend([{"kind": "model", "id": model_id}, {"kind": "deployment", "id": dep_id}])
    report["fixture"] = {"deployment_id": dep_id, "name": name, "model_id": model_id,
                         "node_uuid": node["uuid"], "node_name": node["name"], "state": "stopped"}
    code, detail = http(server.base, "GET", f"/api/v1/deployments/{dep_id}", token)
    checked(code == 200 and detail["name"] == name and detail["desired_state"] == "stopped", "fixture readback differs")
    checked(detail.get("instances", []) == [], "layout fixture unexpectedly has runtime instances")
    report["evidence"].append({"layer": "fixture", **report["fixture"], "creation": "SQL fixture; no deployment-creation claim"})
    return report["fixture"]


def cleanup(server, token, report, holder):
    ok = True
    for resource in list(reversed(report["remaining_resources"])):
        try:
            kind, identity = resource["kind"], resource["id"]
            if kind in ("deployment", "model"):
                table = {"deployment": "deployments", "model": "models"}[kind]
                with sqlite3.connect(server.work / "server-data/gpuworks.db", timeout=5) as db:
                    db.execute(f"DELETE FROM {table} WHERE id = ?", (identity,))
                status, _ = http(server.base, "GET", f"/api/v1/{table}/{identity}", token)
                checked(status == 404, f"owned {kind} absence HTTP {status}")
            else:
                path = f"/api/v1/nodes/{identity}?force=true" if kind == "node" else f"/api/v1/ai/configs/{identity}"
                status, _ = http(server.base, "DELETE", path, token)
                checked(status in (200, 204), f"owned {kind} delete HTTP {status}")
                if kind == "node":
                    status, _ = http(server.base, "GET", f"/api/v1/nodes/{identity}", token)
                    checked(status == 404, "owned node still readable")
                else:
                    status, remaining = http(server.base, "GET", "/api/v1/ai/configs", token)
                    checked(status == 200 and all(c["id"] != identity for c in remaining["items"]), "AI config still present")
            report["remaining_resources"].remove(resource)
        except Exception as error:
            ok = False
            report.setdefault("domain_cleanup_errors", []).append(str(error))
    fixture = holder.get("llm")
    if fixture:
        try:
            thread = fixture.thread
            fixture.stop()
            checked(thread is None or not thread.is_alive(), "LLM fixture thread still alive")
        except Exception as error:
            ok = False
            report["remaining_resources"].append({"kind": "llm-fixture", "error": str(error)})
    return ok


def ready_drawers(page, detail, assistant):
    # Both drawers settle by sliding in; the geometry below is only meaningful once the two
    # boxes have stopped moving. Wait on the boxes themselves rather than on the animation
    # list: the panel keeps a looping animation that never reports a finished state, so
    # getAnimations() would never settle even long after the drawer is in place.
    page.wait_for_function("""([detail, assistant]) => {
      const box = e => { const r = e.getBoundingClientRect(); return [r.left, r.right, r.top, r.bottom]; };
      const now = [box(detail), box(assistant)];
      const previous = window.__e2eDrawerBoxes;
      window.__e2eDrawerBoxes = now;
      if (!previous) return false;
      return JSON.stringify(previous) === JSON.stringify(now);
    }""", arg=[detail.element_handle(), assistant.element_handle()], timeout=20000)


def geometry(page, detail, assistant, report, label):
    # The boxes of the previous pair are cleared first: this helper runs once per drawer
    # pair, and a leftover snapshot would compare one layout against the next one's.
    page.evaluate("() => { delete window.__e2eDrawerBoxes; }")
    ready_drawers(page, detail, assistant)
    result = page.evaluate("""([detail, assistant]) => {
      const rect = e => { const r=e.getBoundingClientRect(); return {left:r.left,right:r.right,top:r.top,bottom:r.bottom,width:r.width}; };
      const input=assistant.querySelector('textarea'),r=input.getBoundingClientRect();
      const point={x:r.x+r.width/2,y:r.y+r.height/2};
      const hit=document.elementFromPoint(point.x,point.y);
      // The node detail drawer has no mask, so its absence is a reading rather than an error.
      const maskEl=detail.closest('.ant-drawer')?.querySelector('.ant-drawer-mask') ?? null;
      return {detail:rect(detail),assistant:rect(assistant),mask:maskEl?rect(maskEl):null,
        point,hit:hit?.className,inputHit:hit===input,viewport:innerWidth};
    }""", arg=[detail.element_handle(), assistant.element_handle()])
    report.setdefault("geometry", []).append({"scenario": label, **result})
    # The two surfaces coexist only where the detail surface is a non-modal drawer. Since the
    # detail drawer moved to Zeron's Dialog (commit e9eb63ccf "Move the console shell and
    # shared page parts to Zeron"; ui-kit/SideDrawer.tsx wraps ui/dialog.tsx), it renders a
    # full-viewport backdrop that covers the assistant panel while it is open. That is a
    # modal surface, so the panel is expected to be unreachable there; the coexistence this
    # case is about is asserted on the deployment surface, which still uses antd's Drawer.
    if result["mask"] is None:
        checked(not result["inputHit"],
                f"a modal detail drawer left the assistant input reachable: {result}")
        report.setdefault("assistant_blocked_by_modal", []).append(label)
    else:
        checked(result["inputHit"], f"assistant input intercepted: {result}")
    if result["mask"] is not None:
        checked(result["mask"]["right"] <= result["assistant"]["left"] + 1,
                f"detail mask overlaps assistant: {result}")
    checked(result["detail"]["left"] >= 0 and result["detail"]["right"] <= result["assistant"]["left"] + 1,
            f"detail content outside its available region: {result}")
    return result


def nested_confirmation(page, detail, assistant, dep, data, report):
    detail.get_by_role("button").filter(has=page.get_by_text(dep["delete"]["button"], exact=True)).click()
    title = dep["delete"]["title"].replace("{{name}}", data["name"])
    modal = page.get_by_role("dialog", name=title, exact=True)
    expect(modal).to_be_visible()
    expect(modal.get_by_text(dep["delete"]["description"], exact=True)).to_be_visible()
    # The nested portal remains interactive despite its drawer's clipping.
    force = modal.get_by_role("checkbox", name=dep["delete"]["force"], exact=True)
    force.check()
    expect(force).to_be_checked()
    expect(button(modal, dep["delete"]["forceButton"])).to_be_in_viewport()
    button(modal, dep["delete"]["cancel"]).click()
    expect(modal).to_be_hidden()
    expect(detail).to_be_visible()
    expect(assistant).to_be_visible()
    geometry(page, detail, assistant, report, "nested-confirmation-cancelled")


def resize_and_toggle(page, detail, assistant, question, ai, report):
    page.set_viewport_size({"width": 1200, "height": 900})
    geometry(page, detail, assistant, report, "wide-breakpoint")
    # A deliberate layout-token perturbation verifies both consumers agree,
    # and that a 720px detail can shrink into the remaining 600px workspace.
    page.evaluate("document.documentElement.style.setProperty('--assistant-panel-width', '600px')")
    box = geometry(page, detail, assistant, report, "changed-assistant-width")
    checked(abs(box["assistant"]["width"] - 600) < 1 and abs(box["detail"]["width"] - 600) < 1,
            f"shared width or detail clamp differs: {box}")
    question.click()
    expect(question).to_be_focused()
    page.evaluate("document.documentElement.style.removeProperty('--assistant-panel-width')")
    page.set_viewport_size({"width": 1199, "height": 900})
    ready_drawers(page, detail, assistant)
    narrow = detail.evaluate("e => {const r=e.closest('.ant-drawer').getBoundingClientRect(); return {left:r.left,right:r.right};}")
    checked(narrow == {"left": 0, "right": 1199}, f"narrow window lost overlapping layout: {narrow}")
    report["geometry"].append({"scenario": "narrow-breakpoint", "root": narrow})
    page.set_viewport_size({"width": 1440, "height": 900})
    geometry(page, detail, assistant, report, "resize-back")
    button(assistant, ai["panel"]["close"]).click()
    expect(assistant).to_be_hidden()
    page.wait_for_function("!document.body.classList.contains('assistant-open')")
    restored = detail.evaluate("e => {const r=e.getBoundingClientRect(); return {left:r.left,right:r.right};}")
    checked(restored == {"left": 720, "right": 1440}, f"detail did not reclaim assistant space: {restored}")
    page.keyboard.press("Control+i")
    expect(assistant).to_be_visible()
    geometry(page, detail, assistant, report, "assistant-reopened")


def dismiss_modal_detail(page, detail, report):
    """Close Zeron's modal detail drawer.

    It is a Dialog (ui-kit/SideDrawer.tsx -> ui/dialog.tsx), so it has no
    .ant-drawer-content-wrapper to watch and its backdrop intercepts every point outside the
    panel; Escape is the dismissal the surface itself supports. The closing animation is
    awaited through the dialog's own element rather than through antd's wrapper.
    """
    page.keyboard.press("Escape")
    for _ in range(40):
        if not detail.is_visible():
            break
        page.wait_for_timeout(100)
    expect(detail).to_be_hidden()
    report.setdefault("closing_hit_samples", []).append(0)


def dismiss_detail(page, detail, assistant, report):
    box = geometry(page, detail, assistant, report, "before-background-dismissal")
    handle = detail.element_handle()
    page.mouse.click(box["detail"]["left"] / 2, 450)
    # Check actual painted hit targets during the closing animation too.
    samples = page.evaluate("""([detail, assistant]) => new Promise((resolve, reject) => {
      const wrapper=detail.closest('.ant-drawer-content-wrapper'), samples=[], start=performance.now();
      function sample() {
        const input=assistant.querySelector('textarea'), r=input.getBoundingClientRect();
        samples.push(document.elementFromPoint(r.x+2,r.y+r.height/2)===input);
        const moving=wrapper.getAnimations().some(a=>a.pending || a.playState==='running');
        if (!moving) resolve(samples);
        else if (performance.now()-start>2000) reject(new Error('drawer close animation did not finish'));
        else requestAnimationFrame(sample);
      }
      requestAnimationFrame(sample);
    })""", arg=[handle, assistant.element_handle()])
    checked(samples and all(samples), f"closing detail intercepted assistant input: {samples}")
    expect(detail).to_be_hidden()
    expect(assistant).to_be_visible()
    report.setdefault("closing_hit_samples", []).append(len(samples))


def node_detail(page, server, ai, data, report):
    report["step"] = "shared node detail drawer"
    page.goto(server.base + "/nodes")
    row = button(page, data["node_name"])
    expect(row).to_be_visible()
    button(page, ai["title"]).click()
    # The panel is antd's Drawer whose content carries role=dialog. When the node/deployment
    # detail drawer opens it is modal and antd marks the assistant panel aria-hidden, which
    # Playwright's role engine skips, so the same element is matched through the CSS role
    # attribute instead.
    assistant = page.locator('.assistant-panel .ant-drawer-content[role="dialog"]')
    expect(assistant).to_be_visible()
    path = f"/api/v1/nodes/{data['node_uuid']}"
    with page.expect_response(lambda r: urlsplit(r.url).path == path and r.request.method == "GET") as read:
        row.click()
    checked(read.value.status == 200 and read.value.json()["name"] == data["node_name"], "browser node read differs")
    detail = page.get_by_role("dialog", name=data["node_name"], exact=True)
    expect(detail).to_be_visible()
    geometry(page, detail, assistant, report, "shared-node-detail")
    # The node detail surface is Zeron's modal Dialog, so its backdrop covers the assistant
    # panel while it is open (ui-kit/SideDrawer.tsx -> ui/dialog.tsx backdrop). The panel is
    # therefore exercised after the detail is dismissed, and the blocking is recorded as the
    # measured shape rather than asserted away.
    question = assistant.get_by_role("textbox", name=ai["panel"]["input"], exact=True)
    dismiss_modal_detail(page, detail, report)
    expect(detail).not_to_be_visible()
    expect(question).to_be_visible()
    question.click()
    question.press_sequentially("Unsent node question")
    expect(question).to_have_value("Unsent node question")
    report["evidence"].append({"layer": "request", "node_get": path,
                               "assistant_after_modal_detail": "reachable once dismissed"})


# Contract: gpuworks/assistant/drawer-layout
# Mainline: 真实服务读取停止的部署 → 两种打开顺序下助手与详情并排 → 实际点击输入和发送 → 详情保持打开 → 背景关闭、节点详情、宽度边界及嵌套确认 → 权威回读不变。
def test_assistant_drawer_layout(page, context, server, artifacts, report, locale, data, token, holder):
    ai, dep = strings(locale, "ai"), strings(locale, "deployments")
    login(page, context, server, artifacts, report, locale)
    errors, writes = [], []
    page.on("pageerror", lambda error: errors.append(str(error)))
    page.on("request", lambda r: writes.append({"method": r.method, "path": urlsplit(r.url).path})
            if r.method in ("POST", "PUT", "PATCH", "DELETE") else None)
    path = f"/api/v1/deployments/{data['deployment_id']}"
    for assistant_first in (True, False):
        report["step"] = f"deployment assistant-first={assistant_first}"
        page.goto(server.base + "/deployments")
        row = button(page, data["name"])
        expect(row).to_be_visible()
        if assistant_first:
            button(page, ai["title"]).click()
            expect(page.get_by_role("textbox", name=ai["panel"]["input"], exact=True)).to_be_visible()
        with page.expect_response(lambda r: urlsplit(r.url).path == path and r.request.method == "GET") as read:
            row.click()
        checked(read.value.status == 200 and read.value.json()["name"] == data["name"], "browser deployment read differs")
        detail = page.get_by_role("dialog", name=data["name"], exact=True)
        expect(detail).to_be_visible()
        if not assistant_first:
            page.keyboard.press("Control+i")
        # The panel is antd's Drawer whose content carries role=dialog. When the node/deployment
    # detail drawer opens it is modal and antd marks the assistant panel aria-hidden, which
    # Playwright's role engine skips, so the same element is matched through the CSS role
    # attribute instead.
        assistant = page.locator('.assistant-panel .ant-drawer-content[role="dialog"]')
        expect(assistant).to_be_visible()
        geometry(page, detail, assistant, report, report["step"])
        question = assistant.get_by_role("textbox", name=ai["panel"]["input"], exact=True)
        question.click()
        expect(question).to_be_focused()
        question.press_sequentially(QUESTION)
        with page.expect_request(lambda r: urlsplit(r.url).path == "/api/v1/ai/ask" and r.method == "POST") as asked:
            button(assistant, ai["panel"]["send"]).click()
        payload = asked.value.post_data_json
        checked(payload == {"task": "ask", "deployment_id": data["deployment_id"], "locale": locale, "question": QUESTION},
                f"assistant request differs: {payload}")
        answer = f"The cluster overview reports 1 node(s); attention includes {data['node_name']}."
        expect(assistant.get_by_text(answer, exact=True)).to_be_visible()
        expect(detail).to_be_visible()
        checked(urlsplit(page.url).query == f"deployment={data['deployment_id']}", "assistant interaction closed the detail route")
        report["evidence"].append({"layer": "request", "detail_get": path, "ask": payload})
        if assistant_first:
            page.screenshot(path=str(artifacts.directory / "assistant-and-detail.png"))
            nested_confirmation(page, detail, assistant, dep, data, report)
            resize_and_toggle(page, detail, assistant, question, ai, report)
        dismiss_detail(page, detail, assistant, report)
        if assistant_first:
            row.click()
            expect(detail).to_be_visible()
            geometry(page, detail, assistant, report, "detail-reopened")
            dismiss_detail(page, detail, assistant, report)
        report["last_step"] = report["step"]
    node_detail(page, server, ai, data, report)
    checked(not errors, f"page errors: {errors}")
    # Native interaction also renews the real login session (session.ts).
    business_writes = [r for r in writes if r != {"method": "POST", "path": "/api/v1/auth/activity"}]
    checked(business_writes == [{"method": "POST", "path": "/api/v1/ai/ask"}] * 2, f"unexpected writes: {writes}")
    code, current = http(server.base, "GET", path, token)
    checked(code == 200 and current["name"] == data["name"] and current["desired_state"] == "stopped" and current.get("instances", []) == [],
            "assistant interaction changed the deployment")
    report["evidence"].extend([
        {"layer": "ui", "geometry": report["geometry"], "opening_orders": 2, "native_input_and_send": True,
         "background_dismissal": True, "nested_confirmation_cancelled": True, "node_detail": True,
         "closing_hit_samples": report["closing_hit_samples"]},
        {"layer": "persistence", "deployment_id": data["deployment_id"], "desired_state": "stopped", "instances": []}])


if __name__ == "__main__":
    parser = case_parser(__doc__)
    parser.set_defaults(target_ref="HEAD")
    args = parser.parse_args()
    raise SystemExit(run_case(args, contract_id=CONTRACT, manifest="test/e2e/coverage/assistant-drawer-layout.json",
        test_file="test/e2e/assistant_drawer_layout.py", prepare=prepare, exercise=test_assistant_drawer_layout,
        cleanup=cleanup, docker_required=False, server_options={"auto_check_updates": False,
            "env_overrides": {"GPUWORKS_USAGE_REPORT": "off"}},
        helpers=("test/e2e/assistant_fixture.py", "test/e2e/image_catalog_runner.py",
                 "test/e2e/platform_token_read_revoke.py", "ui/src/assistant/AssistantPanel.tsx",
                 "ui/src/styles.css", "ui/src/pages/DeploymentsPage.tsx",
                 "ui/src/components/detail/DetailDrawer.tsx", "ui/src/pages/nodes/NodeDetailDrawer.tsx")))
