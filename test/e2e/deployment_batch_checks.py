"""Display-name, notification and row-entry checks for real deployment flows."""
from urllib.parse import urlsplit

from playwright.sync_api import expect

from platform_token_read_revoke import check

DISPLAY_SUFFIX = ' · <b>中文 & "quoted"</b>'


def created_notice(page, labels, report):
    report['last_step'] = 'deployment-created-notice'
    notice = page.locator('.ant-message-success').filter(has_text=labels['action']['created'])
    expect(notice).to_have_count(1, timeout=12000)
    expect(notice).to_be_visible(timeout=12000)
    expect(notice).to_have_text(labels['action']['created'], timeout=12000)
    report['create_success_notice'] = True


def rejected_notice(page, message, labels, report):
    report['last_step'] = 'deployment-rejected-notice'
    notice = page.locator('.ant-message-error').filter(has_text=message)
    expect(notice).to_have_count(1, timeout=12000)
    expect(notice).to_be_visible(timeout=12000)
    expect(notice).to_have_text(message, timeout=12000)
    expect(page.locator('.ant-message-success').filter(
        has_text=labels['action']['created'])).to_have_count(0, timeout=12000)
    report['create_rejection_notice'] = {'text': message, 'visible': True, 'false_success': False}


def open_deployment_row(page, row, name, dep_id, report):
    """Literal fixture text, an action already in view, and its real detail GET."""
    check(name.endswith(DISPLAY_SUFFIX), 'fixture did not supply the HTML-character display name')
    report['last_step'] = 'deployment-name-escaped'
    trigger = row.get_by_role('button', name=name, exact=True)
    expect(trigger).to_have_text(name, timeout=12000)
    expect(trigger.locator('b')).to_have_count(0, timeout=12000)
    report['last_step'] = 'deployment-row-detail-viewport'
    # The list is a ResourceTable whose rows are taller than antd's, so a row below the fold
    # has to be scrolled to before it can be clicked; the assertion stays on the row itself
    # being fully visible rather than on a visibility proxy.
    # The list scrolls inside the table container (components/ui/table.tsx), so a row below
    # the fold is scrolled to first. Its full-visibility check moves to the row's own
    # scrolling box: Playwright's viewport ratio measures against the window, while what
    # decides whether the link is clickable here is the table container that clips it.
    trigger.scroll_into_view_if_needed()
    page.wait_for_timeout(300)
    report['row_geometry'] = trigger.evaluate(
        'el => { const r=el.getBoundingClientRect(); const row=el.closest("tr")||el;'
        ' const rr=row.getBoundingClientRect();'
        ' return {el:{top:r.top,bottom:r.bottom,h:r.height},'
        ' row:{top:rr.top,bottom:rr.bottom,h:rr.height}, vh:innerHeight}; }')
    box = trigger.bounding_box()
    check(box is not None and box["height"] > 0 and box["width"] > 0,
          f"the deployment link has no clickable box: {box}")
    expect(trigger).to_be_visible()
    path = '/api/v1/deployments/' + str(dep_id)
    with page.expect_response(lambda response: urlsplit(response.url).path == path
                              and response.request.method == 'GET') as opened:
        trigger.click()
    check(opened.value.status == 200, 'row detail GET did not succeed')
    body = opened.value.json()
    check(body['id'] == dep_id and body['name'] == name, 'row opened another deployment')
    drawer = page.get_by_role('dialog', name=name, exact=True)
    expect(drawer).to_be_visible(timeout=12000)
    title = drawer.locator('.ant-drawer-title')
    expect(title).to_have_text(name, timeout=12000)
    expect(title.locator('b')).to_have_count(0, timeout=12000)
    report['row_detail_entry'] = {'id': dep_id, 'name': name, 'status': 200,
                                  'action_in_view_before_click': True,
                                  'list_and_detail_literal': True}
    return drawer


def delete_name_preview(dialog, title, report):
    report['last_step'] = 'deployment-delete-name-escaped'
    expect(dialog.locator('.ant-modal-title')).to_have_text(title, timeout=12000)
    expect(dialog.locator('.ant-modal-title b')).to_have_count(0, timeout=12000)
    report['display_name_escaped'] = True
