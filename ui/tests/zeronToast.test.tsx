// Zeron toasts (ui/src/components/ui/toast.tsx): the shared toast() store and
// <Toaster> the app's FeedbackHost mounts, and the presentational ToastStack.
// Its own file: the toast store is module-wide, and exits wait on framer-motion.
import { dom } from "./helpers/dom.ts";
import assert from "node:assert/strict";
import { afterEach, test } from "node:test";
import React from "react";

for (const name of ["KeyboardEvent", "MouseEvent", "PointerEvent", "Document"] as const) {
  if (dom.window[name]) Object.defineProperty(globalThis, name, { value: dom.window[name], configurable: true });
}
// helpers/dom.ts drops animation frames a finished test left pending, which
// leaves framer-motion's frame loop waiting forever and later toasts never
// finish their exit. This file only animates toasts, so plain timer frames
// (never dropped) are safe here. Installed before framer-motion loads.
const timerFrame = (callback: (time: number) => void) => setTimeout(() => callback(performance.now()), 16) as unknown as number;
const cancelTimerFrame = (id: number) => clearTimeout(id);
for (const target of [globalThis, dom.window] as object[]) {
  Object.defineProperty(target, "requestAnimationFrame", { value: timerFrame, configurable: true });
  Object.defineProperty(target, "cancelAnimationFrame", { value: cancelTimerFrame, configurable: true });
}
const { render, screen, fireEvent, waitFor, cleanup, act } = await import("@testing-library/react");
const { toast, Toaster, ToastStack, Toast } = await import("../src/components/ui/toast.tsx");
const { PortalContainerProvider } = await import("../src/lib/portal-container-context.tsx");

afterEach(() => { act(() => toast.dismiss()); cleanup(); });
const gone = (text: string, timeout?: number) => waitFor(() => assert.ok(screen.queryByText(text) === null, `${text} removed`), { timeout, mutationObserverOptions: { childList: true, subtree: true } });

test("toasts announce by status: errors as alerts, everything else as status, with title and description", async () => {
  render(<Toaster aria-label="Notices" />);
  act(() => {
    toast("Plain note", { duration: 0 });
    toast.neutral("Neutral note", { duration: 0 });
    toast.info("Sync started", { description: "3 nodes", duration: 0 });
    toast.success("Saved", { duration: 0 });
    toast.error("Delete failed", { description: "Node is busy", duration: 0 });
  });
  const list = await screen.findByRole("list", { name: "Notices" });
  assert.ok(list.parentElement === document.body, "a fixed toaster portals to the body");
  assert.equal(screen.getByRole("alert").textContent?.includes("Delete failed"), true);
  assert.ok(screen.getByRole("alert").textContent?.includes("Node is busy"));
  const statuses = screen.getAllByRole("status").map((item) => item.textContent);
  assert.deepEqual(statuses.map((text) => text?.replace("Dismiss notification", "")), ["Neutral note", "Sync started3 nodes", "Saved"]);
  assert.equal(list.children.length, 4, "four visible by default, the oldest dropped");
  assert.ok(screen.queryByText("Plain note") === null);
});

test("the close button dismisses a toast; non-dismissible toasts have none", async () => {
  render(<Toaster closeLabel="Close" />);
  act(() => {
    toast.info("Closable", { duration: 0 });
    toast.info("Pinned", { duration: 0, dismissible: false });
  });
  await screen.findByText("Pinned");
  const closes = screen.getAllByRole("button", { name: "Close" });
  assert.equal(closes.length, 1);
  fireEvent.click(closes[0]);
  await gone("Closable");
  assert.ok(screen.getByText("Pinned"));
});

test("an action runs with the toast and dismisses it unless asked to stay", async () => {
  const seen: unknown[] = [];
  render(<Toaster />);
  act(() => {
    toast.error("Upload failed", { id: "upload", duration: 0, action: { label: "Retry", onClick: (item) => seen.push(item.id) } });
    toast.info("Model ready", { duration: 0, action: { label: "Open", onClick: () => seen.push("open"), dismissOnClick: false } });
  });
  fireEvent.click(await screen.findByRole("button", { name: "Retry" }));
  assert.deepEqual(seen, ["upload"]);
  await gone("Upload failed");
  fireEvent.click(screen.getByRole("button", { name: "Open" }));
  assert.deepEqual(seen, ["upload", "open"]);
  assert.ok(screen.getByText("Model ready"));
});

test("update rewrites a toast in place, an id is reused instead of stacking, and dismiss clears by id or all", async () => {
  render(<Toaster />);
  let id!: ReturnType<typeof toast>;
  act(() => { id = toast.loading("Uploading", { duration: 0 }); });
  await screen.findByText("Uploading");
  act(() => { toast.update(id, { title: "Uploaded", status: "success" }); });
  await screen.findByText("Uploaded");
  await gone("Uploading");
  act(() => { toast.info("First", { id: "same", duration: 0 }); toast.info("Second", { id: "same", duration: 0 }); });
  await screen.findByText("Second");
  assert.ok(screen.queryByText("First") === null);
  act(() => { toast.dismiss(id); });
  await gone("Uploaded");
  assert.ok(screen.getByText("Second"));
  act(() => { toast.success("Third", { duration: 0 }); });
  await screen.findByText("Third");
  act(() => { toast.dismiss(); });
  await gone("Second");
  await gone("Third");
});

test("toasts expire after their duration, the toaster default applies, and loading waits until dismissed", async () => {
  // Toaster default 2000ms (built-in 4200ms), own lifetime 30ms. The margins leave
  // a loaded CI runner room: the default toast is checked long before 2000ms, and
  // its removal is awaited for less time than the built-in default would take.
  render(<Toaster duration={2000} />);
  act(() => {
    toast.success("Default lifetime");
    toast.info("Own lifetime", { duration: 30 });
    toast.loading("Still working");
  });
  await screen.findByText("Still working");
  await gone("Own lifetime", 1500);
  assert.ok(screen.getByText("Default lifetime"), "the toaster default outlives a shorter own duration");
  await gone("Default lifetime", 3500);
  assert.ok(screen.getByText("Still working"), "loading has no lifetime");
  // Turning a timed toast into a persistent one (duration 0) cancels its timer.
  act(() => { toast.info("Refreshing", { id: "refresh", duration: 1000 }); });
  await screen.findByText("Refreshing");
  act(() => { toast.update("refresh", { duration: 0 }); });
  await new Promise((resolve) => setTimeout(resolve, 1600));
  assert.ok(screen.getByText("Refreshing"), "a toast updated to duration 0 outlives its former 1000ms lifetime");
});

test("a static stack shows the latest toasts up to maxVisible and reports dismissals", () => {
  const dismissed: unknown[] = [];
  const toasts = ["One", "Two", "Three"].map((title, i) => ({ id: i, title }));
  render(<ToastStack toasts={toasts} maxVisible={2} onDismiss={(id) => dismissed.push(id)} />);
  const list = screen.getByRole("list", { name: "Notifications" });
  assert.equal(list.getAttribute("style"), null, "a static stack has no edge offsets");
  assert.deepEqual([...list.children].map((item) => item.textContent?.replace("Dismiss notification", "")), ["Two", "Three"]);
  fireEvent.click(screen.getAllByRole("button", { name: "Dismiss notification" })[1]);
  assert.deepEqual(dismissed, [2]);
});

test("a stack without onDismiss offers no close button, and maxVisible below one still shows the newest", () => {
  render(<ToastStack toasts={[{ id: 1, title: "Old" }, { id: 2, title: "New" }]} maxVisible={0} />);
  assert.ok(screen.getByText("New"));
  assert.ok(screen.queryByText("Old") === null);
  assert.ok(screen.queryByRole("button") === null);
});

test("custom rendering and icons replace the defaults", () => {
  render(
    <ToastStack
      toasts={[
        { id: "a", title: "Uses status icon", status: "success" },
        { id: "b", title: "Uses own icon", status: "success", icon: <i data-testid="own" /> },
        { id: "c", title: "Spinner", status: "loading" },
      ]}
      icons={{ success: <i data-testid="status-success" />, loading: <i data-testid="status-loading" /> }}
    />,
  );
  assert.equal(screen.getAllByTestId("status-success").length, 1);
  assert.ok(screen.getByTestId("own"));
  assert.ok(screen.getByTestId("status-loading"));
  cleanup();
  render(<ToastStack toasts={[{ id: 1, title: "Raw", description: "detail" }]} renderToast={(item) => <p>Custom {String(item.title)}</p>} />);
  assert.ok(screen.getByText("Custom Raw"));
  assert.ok(screen.queryByText("detail") === null);
});

test("anchored stacks sit at their edges using the offset, with per-edge overrides and safe-area defaults", () => {
  const style = (props: Partial<React.ComponentProps<typeof ToastStack>>) => {
    const { unmount } = render(<ToastStack toasts={[]} placement="absolute" {...props} />);
    const list = screen.getByRole("list");
    const s = list.style;
    const result = { top: s.top, bottom: s.bottom, left: s.left, right: s.right };
    unmount();
    return result;
  };
  assert.deepEqual(style({ position: "top-left", offset: 8 }), { top: "8px", bottom: "", left: "8px", right: "" });
  assert.deepEqual(style({ position: "bottom-right", offset: "2rem" }), { top: "", bottom: "2rem", left: "", right: "2rem" });
  assert.deepEqual(style({ position: "top-right", offset: { top: 4, right: 12 } }), { top: "4px", bottom: "", left: "", right: "12px" });
  assert.deepEqual(style({ position: "bottom-left", offset: { left: 6 } }).left, "6px");
  // Centred stacks get no side inset. (The default max(1rem, env(safe-area-inset-*))
  // value is dropped by jsdom's CSS parser, so it is not observable here.)
  const centered = style({ position: "top-center", offset: 10, toasts: [{ id: 1, title: "Centred" }] });
  assert.deepEqual(centered, { top: "10px", bottom: "", left: "", right: "" });
  const { unmount } = render(<ToastStack toasts={[]} placement="absolute" position="bottom-center" offset={3} style={{ zIndex: 5 }} />);
  const list = screen.getByRole("list");
  assert.equal(list.style.bottom, "3px");
  assert.equal(list.style.zIndex, "5", "caller style merges with the edge offsets");
  unmount();
  style({ position: "bottom-left" });
});

test("a portalled stack mounts into an explicit container, else the ambient one", () => {
  const host = document.createElement("section");
  const ambient = document.createElement("aside");
  document.body.append(host, ambient);
  try {
    render(<ToastStack toasts={[{ id: 1, title: "In host" }]} portal container={host} />);
    assert.ok(host.contains(screen.getByText("In host")));
    cleanup();
    render(<PortalContainerProvider value={ambient}><ToastStack toasts={[{ id: 1, title: "In ambient" }]} placement="fixed" /></PortalContainerProvider>);
    assert.ok(ambient.contains(screen.getByText("In ambient")));
    cleanup();
    render(<Toaster placement="static" aria-label="Inline" />);
    assert.ok(screen.getByRole("list", { name: "Inline" }).parentElement?.parentElement === document.body, "a static toaster renders in place");
  } finally {
    host.remove();
    ambient.remove();
  }
});

test("a toast for a top position renders standalone with its close action", () => {
  const dismissed: unknown[] = [];
  render(<ul><Toast toast={{ id: 9, title: "Solo", action: { label: "Undo", onClick: () => {} } }} position="top-left" onDismiss={(id) => dismissed.push(id)} /></ul>);
  fireEvent.click(screen.getByRole("button", { name: "Undo" }));
  assert.deepEqual(dismissed, [9]);
});

test("swiping a toast toward its edge dismisses it; a short swipe or the wrong direction does not", async () => {
  const dismissed: unknown[] = [];
  const swipe = async (position: React.ComponentProps<typeof ToastStack>["position"], dx: number) => {
    dismissed.length = 0;
    const { unmount } = render(<ToastStack toasts={[{ id: position!, title: `Swipe ${position}` }]} position={position} onDismiss={(id) => dismissed.push(id)} />);
    const item = screen.getByText(`Swipe ${position}`).closest("li")!;
    const opts = { isPrimary: true, button: 0, pointerId: 1, pointerType: "mouse" };
    fireEvent.pointerDown(item, { ...opts, clientX: 200, clientY: 10 });
    for (let step = 1; step <= 4; step++) {
      await act(() => new Promise((resolve) => setTimeout(resolve, 20)));
      fireEvent.pointerMove(window, { ...opts, clientX: 200 + (dx * step) / 4, clientY: 10 });
    }
    await act(() => new Promise((resolve) => setTimeout(resolve, 20)));
    fireEvent.pointerUp(window, { ...opts, clientX: 200 + dx, clientY: 10 });
    await act(() => new Promise((resolve) => setTimeout(resolve, 20)));
    const result = [...dismissed];
    unmount();
    return result;
  };
  assert.deepEqual(await swipe("bottom-right", 160), ["bottom-right"]);
  assert.deepEqual(await swipe("top-left", -160), ["top-left"]);
  assert.deepEqual(await swipe("bottom-center", -160), ["bottom-center"]);
  assert.deepEqual(await swipe("bottom-right", -160), [], "a right-anchored toast ignores a swipe to the left");
  assert.deepEqual(await swipe("top-left", 20), [], "a short swipe snaps back");
});
