import assert from "node:assert/strict";
import { afterEach, test } from "node:test";
import { createI18n, dom } from "./helpers/dom.ts";
import React from "react";
import { I18nextProvider } from "react-i18next";

const { i18n, locale } = await createI18n(["settings"]);
const settingsLocale = locale.settings;

const { cleanup, render, screen, waitFor } = await import("@testing-library/react");
const userEventModule = await import("@testing-library/user-event");
const userEvent = userEventModule.default ?? userEventModule;
const apiModule = await import("../src/api.ts");
const DeploymentPolicyCard = (await import("../src/pages/settings/DeploymentPolicyCard.tsx")).default;

afterEach(() => cleanup());

const text = (key: string) => settingsLocale.deployment[key] as string;

function stubAPI(settingsValue: Record<string, unknown> | Error, putError?: Error) {
  const puts: { key: string; value: unknown }[] = [];
  const original = { settings: apiModule.api.settings, putSetting: apiModule.api.putSetting };
  apiModule.api.settings = async () => {
    if (settingsValue instanceof Error) throw settingsValue;
    return settingsValue as never;
  };
  apiModule.api.putSetting = async (key: string, value: unknown) => {
    puts.push({ key, value });
    if (putError) throw putError;
  };
  return {
    puts,
    restore: () => {
      apiModule.api.settings = original.settings;
      apiModule.api.putSetting = original.putSetting;
    },
  };
}

function renderCard() {
  render(
    <I18nextProvider i18n={i18n}>
      <DeploymentPolicyCard />
    </I18nextProvider>,
  );
  return {
    input: () => screen.getByRole("spinbutton", { name: text("startTimeout") }) as HTMLInputElement,
    save: () => screen.getByRole("button", { name: settingsLocale.common.save as string }) as HTMLButtonElement,
  };
}

test("shows the stored start timeout and saves only that field", async () => {
  const stub = stubAPI({ deployment: { pending_timeout_sec: 600, start_timeout_sec: 1800, event_retention_days: 180 } });
  try {
    const card = renderCard();
    assert.ok(screen.getByText(text("title")));
    await waitFor(() => assert.equal(card.input().value, "1800"));

    const user = userEvent.setup({ document: dom.window.document });
    await user.clear(card.input());
    await user.type(card.input(), "3600");
    await user.click(card.save());
    await user.click(await screen.findByRole("button", { name: "Confirm", exact: true }));
    await waitFor(() => assert.equal(stub.puts.length, 1));
    // The server overlays the body on the stored policy, so the card names
    // only the field it edits and cannot reset the pending timeout.
    assert.deepEqual(stub.puts[0], { key: "deployment", value: { start_timeout_sec: 3600 } });
  } finally {
    stub.restore();
  }
});

for (const value of ["30", "86401", "60.5"]) {
  test(`an out-of-range timeout ${value} stays invalid after leaving the field`, async () => {
    const stub = stubAPI({ deployment: { start_timeout_sec: 1800 } });
    try {
      const card = renderCard();
      await waitFor(() => assert.equal(card.input().value, "1800"));
      const user = userEvent.setup({ document: dom.window.document });
      await user.clear(card.input());
      assert.equal(card.save().disabled, true, "an empty field is not a value to save");
      await user.type(card.input(), value);
      await user.tab();
      assert.equal(card.input().value, value, "blur must not silently replace the administrator's value");
      assert.equal(card.save().disabled, true);
      assert.ok(await screen.findByText("Enter a whole number between 60 and 86400 seconds."));
      await user.click(card.save());
      assert.equal(stub.puts.length, 0, "an invalid value never sends a settings mutation");
      await user.clear(card.input());
      await user.type(card.input(), "3600");
      await user.tab();
      assert.equal(card.save().disabled, false);
      await waitFor(() => assert.ok(screen.queryByText("Enter a whole number between 60 and 86400 seconds.") === null, "correcting the value clears the validation message"));
      await user.click(card.save());
      await user.click(await screen.findByRole("button", { name: "Confirm", exact: true }));
      await waitFor(() => assert.deepEqual(stub.puts, [{ key: "deployment", value: { start_timeout_sec: 3600 } }]));
    } finally {
      stub.restore();
    }
  });
}

test("a failed load says so and leaves nothing to save", async () => {
  const stub = stubAPI(new Error("settings unavailable"));
  try {
    const card = renderCard();
    await waitFor(() => assert.ok(screen.getByText(/settings unavailable/)));
    assert.equal(card.input().value, "");
    assert.equal(card.save().disabled, true);
  } finally {
    stub.restore();
  }
});

test("a rejected save reports the server's reason", async () => {
  const stub = stubAPI({ deployment: { start_timeout_sec: 1800 } }, new Error("deployment.start_timeout_sec must be between 60 and 86400"));
  try {
    const card = renderCard();
    await waitFor(() => assert.equal(card.input().value, "1800"));
    const user = userEvent.setup({ document: dom.window.document });
    await user.click(card.save());
    await user.click(await screen.findByRole("button", { name: "Confirm", exact: true }));
    await waitFor(() => assert.ok(screen.getByText(/must be between 60 and 86400/)));
  } finally {
    stub.restore();
  }
});

// The headroom beside processes outside the platform: a share of the card
// and a floor, both of which may be zero. The floor is typed in GiB and
// stored in bytes.
const headroomInputs = () => ({
  pct: screen.getByRole("spinbutton", { name: text("foreignHeadroomPct") }) as HTMLInputElement,
  floor: screen.getByRole("spinbutton", { name: text("foreignHeadroomMin") }) as HTMLInputElement,
});
const served = { deployment: { pending_timeout_sec: 600, start_timeout_sec: 1800, foreign_headroom_pct: 5, foreign_headroom_min_bytes: 4 * 2 ** 30 } };

test("shows the stored headroom and saves it beside the start timeout", async () => {
  const stub = stubAPI(served);
  try {
    const card = renderCard();
    await waitFor(() => assert.equal(headroomInputs().pct.value, "5"));
    assert.equal(headroomInputs().floor.value, "4");
    assert.ok(screen.getByText(text("foreignHeadroom")));
    assert.ok(screen.getByText(text("foreignHeadroomHint")));
    assert.equal(screen.queryByText(settingsLocale.deployment.warning.noForeignHeadroom), null);

    const user = userEvent.setup({ document: dom.window.document });
    await user.clear(headroomInputs().pct);
    await user.type(headroomInputs().pct, "10");
    await user.clear(headroomInputs().floor);
    await user.type(headroomInputs().floor, "2.5");
    await user.click(card.save());
    assert.equal(stub.puts.length, 0);
    await user.click(await screen.findByRole("button", { name: "Confirm", exact: true }));
    await waitFor(() => assert.equal(stub.puts.length, 1));
    assert.deepEqual(stub.puts[0], { key: "deployment", value: { start_timeout_sec: 1800, foreign_headroom_pct: 10, foreign_headroom_min_bytes: 2.5 * 2 ** 30 } });
  } finally {
    stub.restore();
  }
});

test("keeping no headroom is allowed, warned about, and stored as zeros", async () => {
  const stub = stubAPI(served);
  try {
    const card = renderCard();
    await waitFor(() => assert.equal(headroomInputs().pct.value, "5"));
    const user = userEvent.setup({ document: dom.window.document });
    await user.clear(headroomInputs().pct);
    await user.type(headroomInputs().pct, "0");
    // A floor still stands: something is kept, so nothing to warn about.
    assert.equal(screen.queryByText(settingsLocale.deployment.warning.noForeignHeadroom), null);
    await user.clear(headroomInputs().floor);
    await user.type(headroomInputs().floor, "0");
    assert.ok(await screen.findByText(settingsLocale.deployment.warning.noForeignHeadroom));
    assert.equal(card.save().disabled, false);
    await user.click(card.save());
    assert.equal(stub.puts.length, 0);
    await user.click(await screen.findByRole("button", { name: "Confirm", exact: true }));
    await waitFor(() => assert.deepEqual(stub.puts, [{ key: "deployment", value: { start_timeout_sec: 1800, foreign_headroom_pct: 0, foreign_headroom_min_bytes: 0 } }]));
  } finally {
    stub.restore();
  }
});

for (const [field, value] of [["pct", "51"], ["pct", "-1"], ["pct", "2.5"], ["floor", "65"], ["floor", "-1"], ["pct", ""], ["floor", ""]] as const) {
  test(`a headroom ${field} of "${value}" is refused before it is sent`, async () => {
    const stub = stubAPI(served);
    try {
      const card = renderCard();
      await waitFor(() => assert.equal(headroomInputs().pct.value, "5"));
      const user = userEvent.setup({ document: dom.window.document });
      await user.clear(headroomInputs()[field]);
      if (value) await user.type(headroomInputs()[field], value);
      await user.tab();
      assert.equal(card.save().disabled, true);
      assert.ok(await screen.findByText("Enter a whole share from 0 to 50 and a minimum from 0 to 64 GiB."));
      await user.click(card.save());
      assert.equal(stub.puts.length, 0);
    } finally {
      stub.restore();
    }
  });
}

test("a server that serves no headroom is not asked to store one", async () => {
  const stub = stubAPI({ deployment: { start_timeout_sec: 1800 } });
  try {
    const card = renderCard();
    await waitFor(() => assert.equal(card.input().value, "1800"));
    assert.equal(screen.queryByText(text("foreignHeadroom")), null);
    const user = userEvent.setup({ document: dom.window.document });
    await user.click(card.save());
    assert.equal(stub.puts.length, 0);
    await user.click(await screen.findByRole("button", { name: "Confirm", exact: true }));
    await waitFor(() => assert.deepEqual(stub.puts, [{ key: "deployment", value: { start_timeout_sec: 1800 } }]));
  } finally {
    stub.restore();
  }
});
