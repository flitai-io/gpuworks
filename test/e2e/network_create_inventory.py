"""First-open inventory of the network create dialog."""
from __future__ import annotations

import re

from platform_token_read_revoke import check
from playwright.sync_api import expect

TYPES = ["host_ethernet", "host_rdma", "accelerator_rdma", "accelerator_hccs"]
ROLES = ["storage_sync", "compute", "serving"]
CIDR_PLACEHOLDER = "10.20.0.0/16"


def exercise_network_create_inventory(dialog, t):
    """Labels, control types, required flags, defaults, range, and ethernet condition."""
    type_group = dialog.get_by_role("tablist", name=t["form"]["type"], exact=True)
    expect(type_group).to_have_count(1)
    expect(type_group.get_by_role("tab")).to_have_count(4)
    for kind in TYPES:
        tab = type_group.get_by_role("tab", name=t["type"][kind], exact=True)
        expect(tab).to_have_count(1)
        expect(tab).to_be_visible()
        expect(tab).to_have_attribute("aria-selected", "true" if kind == TYPES[0] else "false")
    check(type_group.get_attribute("aria-required") != "true", "the type is required")

    name = dialog.get_by_role("textbox", name=t["form"]["name"], exact=True)
    expect(name).to_be_visible()
    expect(name).to_be_empty()
    check((name.get_attribute("type") or "text") == "text", "the name is not a text input")
    expect(name).to_have_attribute("aria-required", "true")
    expect(name).to_have_attribute("maxlength", "64")
    check(name.get_attribute("placeholder") is None, "the name has a placeholder")

    description = dialog.get_by_label(t["form"]["description"], exact=True)
    expect(description).to_be_visible()
    expect(description).to_be_empty()
    check((description.get_attribute("type") or "text") == "text", "the description is not a text input")
    check(description.get_attribute("aria-required") != "true", "the description is required")
    expect(description).to_have_attribute("maxlength", "256")
    check(description.get_attribute("placeholder") is None, "the description has a placeholder")

    role_group = dialog.get_by_role("group", name=re.compile(t["form"]["roles"]))
    expect(role_group).to_have_count(1)
    expect(role_group).to_have_attribute("aria-required", "true")
    expect(role_group.get_by_role("checkbox")).to_have_count(3)
    for role in ROLES:
        box = role_group.get_by_role("checkbox", name=t["role"][role], exact=True)
        expect(box).to_be_visible()
        expect(box).to_be_enabled()
        expect(box).not_to_be_checked()

    cidr = dialog.get_by_label(t["form"]["cidr"], exact=True)
    expect(cidr).to_be_visible()
    expect(cidr).to_be_empty()
    check((cidr.get_attribute("type") or "text") == "text", "the cidr is not a text input")
    expect(cidr).to_have_attribute("placeholder", CIDR_PLACEHOLDER)
    check(cidr.get_attribute("aria-required") != "true", "the cidr is required")
    check(cidr.get_attribute("maxlength") is None, "the cidr has a maximum length")
    interface = dialog.get_by_label(t["form"]["interface"], exact=True)
    expect(interface).to_be_visible()
    expect(interface).to_be_empty()
    check((interface.get_attribute("type") or "text") == "text", "the interface is not a text input")
    expect(interface).to_have_attribute("placeholder", t["form"]["interfacePlaceholder"])
    check(interface.get_attribute("aria-required") != "true", "the interface is required")
    check(interface.get_attribute("maxlength") is None, "the interface has a maximum length")
    expect(dialog.get_by_label(t["form"]["bootstrapCidr"], exact=True)).to_have_count(0)
    expect(dialog.get_by_role("button").filter(has_text=t["form"]["advanced"])).to_have_count(0)

    expect(dialog.get_by_role("spinbutton")).to_have_count(0)
    expect(dialog.locator(".ant-input-group-addon")).to_have_count(0)
    expect(dialog.get_by_role("combobox")).to_have_count(0)
    expect(dialog.get_by_role("textbox")).to_have_count(4)
    return {
        "type_tabs": 4,
        "default_type": TYPES[0],
        "name_maxlength": 64,
        "description_maxlength": 256,
        "name_required": True,
        "roles_required": True,
        "description_required": False,
        "type_required": False,
        "cidr_placeholder": CIDR_PLACEHOLDER,
        "cidr_visible": True,
        "interface_visible": True,
        "bootstrap_visible": False,
        "spinbuttons": 0,
        "units": 0,
        "textboxes": 4,
    }
