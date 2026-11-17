"""Ethernet create dialog shows one info alert with the selector note."""
from __future__ import annotations

from playwright.sync_api import expect


def exercise_selector_note(dialog, note):
    """The default ethernet form shows one info alert and its localized message."""
    alert = dialog.locator('[data-slot="alert"][data-status="info"]')
    expect(alert).to_have_count(1)
    expect(alert).to_be_visible()
    expect(alert.locator('[data-slot="alert-title"]')).to_have_text(note)
    return {"alerts": 1, "info": True}
