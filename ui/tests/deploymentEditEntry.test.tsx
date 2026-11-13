import assert from "node:assert/strict";
import { after, afterEach, test } from "node:test";
import React from "react";
import { I18nextProvider } from "react-i18next";
import { createI18n, dom } from "./helpers/dom.ts";

const { i18n, locale } = await createI18n(["deployments"], "en");
const { cleanup, render, screen } = await import("@testing-library/react");
const { default: userEvent } = await import("@testing-library/user-event");
const { MemoryRouter, Route, Routes, useLocation } = await import("react-router-dom");
const { ReplicaRetryAlert } = await import("../src/pages/DeploymentLifecycleActions.tsx");
type Deployment = import("../src/api.ts").Deployment;

afterEach(cleanup);
after(() => dom.window.close());
const d = locale.deployments.detail;

function Probe() {
  const location = useLocation();
  return <div data-testid="route">{location.pathname + location.search}</div>;
}

function mount(node: React.ReactNode) {
  render(
    <MemoryRouter initialEntries={["/deployments?deployment=7"]}>
      <I18nextProvider i18n={i18n}>
        <Routes>
          <Route path="/deployments" element={node} />
          <Route path="*" element={<Probe />} />
        </Routes>
      </I18nextProvider>
    </MemoryRouter>,
  );
  return userEvent.setup({ document: dom.window.document });
}
const dep = (over: Partial<Deployment> = {}) => ({ id: 7, name: "qwen", desired_state: "active", replicas: 1, ...over }) as Deployment;
const route = () => screen.getByTestId("route").textContent;

test("the give-up banner offers to fix the parameters, and the plain retry stays", async () => {
  const user = mount(
    <ReplicaRetryAlert dep={dep({ retry_exhausted: { index: 0, failed_attempts: 3, last_failure: "out of memory" } })} onChanged={() => {}} />,
  );
  assert.ok(screen.getByText(/out of memory/));
  assert.ok(screen.getByRole("button", { name: d.retry.action }));
  await user.click(screen.getByRole("button", { name: d.retry.editAndRetry }));
  assert.equal(route(), "/deploy?edit=7");
});
