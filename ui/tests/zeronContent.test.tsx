// Zeron content components: Empty, MetricCard, Badge (+ badge-colors), Alert and Container.
// These render static content, so the contract is mostly the text, roles and
// accessibility attributes each prop produces; where a prop only changes the
// visual treatment (status colour, data-* state) that attribute is the contract.
import "./helpers/dom.ts";
import assert from "node:assert/strict";
import { afterEach, test } from "node:test";
import React from "react";
const { render, screen, fireEvent, cleanup, within } = await import("@testing-library/react");
const { Empty, EmptyActions, EmptyContent, EmptyDescription, EmptyHeader, EmptyHelp, EmptyIllustration, EmptyMedia, EmptyTitle } = await import("../src/components/ui/empty.tsx");
const { MetricCard } = await import("../src/components/ui/metric-card.tsx");
const { Badge, badgeColors } = await import("../src/components/ui/badge.tsx");
const { badgeCategoricalTokens, badgeStatusTokens } = await import("../src/components/ui/badge-colors.ts");
const { Alert, AlertAction, AlertDescription, AlertIcon, AlertTitle } = await import("../src/components/ui/alert.tsx");
const { Container, ContainerBody, ContainerFooter, ContainerHeader } = await import("../src/components/ui/container.tsx");
const { useSurface } = await import("../src/lib/surface-context.tsx");
afterEach(cleanup);

const series = (n: number, f = (i: number) => i) => Array.from({ length: n }, (_, i) => f(i));

// ---------- Empty ----------

test("empty state used by Result shows its title, description and actions as a silent section", () => {
  render(<Empty reason="informational" scope="section"><EmptyHeader><EmptyTitle>No models yet</EmptyTitle><EmptyDescription>Deploy one to start.</EmptyDescription></EmptyHeader><EmptyActions><button>Deploy</button></EmptyActions></Empty>);
  const title = screen.getByRole("heading", { level: 3, name: "No models yet" });
  const root = title.closest("[data-slot=empty]")!;
  assert.equal(root.getAttribute("data-reason"), "informational");
  assert.equal(root.getAttribute("data-scope"), "section");
  assert.equal(root.getAttribute("role"), null);
  assert.equal(root.getAttribute("aria-live"), null);
  assert.ok(screen.getByText("Deploy one to start."));
  assert.ok(screen.getByRole("button", { name: "Deploy" }));
  assert.equal(screen.queryByRole("status"), null);
});

test("an announced empty result becomes a polite status region, and explicit role and aria-live win", () => {
  const { rerender } = render(<Empty reason="no-results" announce><EmptyHeader><EmptyDescription>No matches</EmptyDescription></EmptyHeader></Empty>);
  const status = screen.getByRole("status");
  assert.equal(status.getAttribute("aria-live"), "polite");
  assert.ok(within(status).getByText("No matches"));
  rerender(<Empty reason="no-results" announce role="alert" aria-live="assertive"><EmptyDescription>No matches</EmptyDescription></Empty>);
  const alert = screen.getByRole("alert");
  assert.equal(alert.getAttribute("aria-live"), "assertive");
});

test("empty title follows the requested heading level and scope, density and alignment are exposed", () => {
  render(<Empty reason="first-use" scope="page" density="comfortable" align="start" data-testid="root"><EmptyMedia variant="icon"><svg /></EmptyMedia><EmptyHeader><EmptyTitle as="h2">Welcome</EmptyTitle></EmptyHeader><EmptyContent><input aria-label="Search" /></EmptyContent><EmptyActions><button>Start</button></EmptyActions><EmptyHelp><a href="/docs">Docs</a></EmptyHelp></Empty>);
  assert.ok(screen.getByRole("heading", { level: 2, name: "Welcome" }));
  const root = screen.getByTestId("root");
  assert.equal(root.getAttribute("data-scope"), "page");
  assert.equal(root.getAttribute("data-density"), "comfortable");
  assert.equal(root.getAttribute("data-align"), "start");
  assert.ok(screen.getByRole("textbox", { name: "Search" }));
  assert.ok(screen.getByRole("link", { name: "Docs" }));
});

test("empty media is hidden from assistive technology unless it is marked meaningful", () => {
  render(<Empty reason="no-data" scope="inline" density="compact">
    <EmptyMedia variant="icon" data-testid="icon"><svg /></EmptyMedia>
    <EmptyMedia variant="custom" decorative={false}><img alt="Empty shelf" src="data:," /></EmptyMedia>
    <EmptyMedia data-testid="illustration"><svg /></EmptyMedia>
    <EmptyHeader><EmptyTitle as="h4">Nothing here</EmptyTitle><EmptyDescription>Inline hint</EmptyDescription></EmptyHeader>
    <EmptyContent>content</EmptyContent>
  </Empty>);
  assert.equal(screen.getByTestId("icon").getAttribute("aria-hidden"), "true");
  assert.equal(screen.getByTestId("illustration").getAttribute("data-variant"), "illustration");
  assert.ok(screen.getByRole("img", { name: "Empty shelf" }));
  assert.ok(screen.getByRole("heading", { level: 4, name: "Nothing here" }));
});

test("empty subcomponents also render outside an Empty with section defaults", () => {
  render(<><EmptyMedia variant="icon"><svg /></EmptyMedia><EmptyMedia data-testid="loose-media"><svg /></EmptyMedia><EmptyHeader><EmptyTitle>Loose</EmptyTitle></EmptyHeader><EmptyContent>Body</EmptyContent><EmptyActions>Act</EmptyActions></>);
  assert.ok(screen.getByRole("heading", { name: "Loose" }));
  assert.ok(screen.getByText("Body"));
  assert.equal(screen.getByTestId("loose-media").getAttribute("aria-hidden"), "true");
});

test("page-scoped compact empty states and start-aligned sections render every part", () => {
  const { rerender } = render(<Empty reason="empty-group" scope="page" density="compact"><EmptyMedia><svg /></EmptyMedia><EmptyHeader><EmptyTitle>Compact page</EmptyTitle></EmptyHeader><EmptyContent>Page body</EmptyContent></Empty>);
  assert.ok(screen.getByRole("heading", { name: "Compact page" }));
  rerender(<Empty reason="no-filter-results" align="start"><EmptyHeader><EmptyTitle>Start aligned</EmptyTitle></EmptyHeader><EmptyContent>Section body</EmptyContent><EmptyActions>Act</EmptyActions></Empty>);
  assert.equal(screen.getByRole("heading", { name: "Start aligned" }).closest("[data-slot=empty]")!.getAttribute("data-align"), "start");
  assert.ok(screen.getByText("Section body"));
});

test("built-in illustrations are decorative by default and labelled images when not", () => {
  const variants = ["general", "resources", "search", "filter", "inbox", "analytics"] as const;
  for (const variant of variants) {
    const { container, unmount } = render(<EmptyIllustration variant={variant} />);
    const svg = container.querySelector("svg")!;
    assert.equal(svg.getAttribute("data-variant"), variant);
    assert.equal(svg.getAttribute("aria-hidden"), "true");
    assert.equal(svg.getAttribute("role"), null);
    assert.ok(svg.childNodes.length > 0, `${variant} draws something`);
    unmount();
  }
  render(<EmptyIllustration decorative={false} label="No resources" />);
  const img = screen.getByRole("img", { name: "No resources" });
  assert.equal(img.getAttribute("data-variant"), "general");
  assert.equal(img.getAttribute("viewBox"), "0 0 208 96");
});

test("preview illustration places the supplied media on its front card with the requested fit", () => {
  const { container, rerender } = render(<EmptyIllustration variant="preview" media={<img alt="" src="data:," data-testid="media" />} />);
  const svg = container.querySelector("svg")!;
  assert.equal(svg.getAttribute("data-media-fit"), "contain");
  assert.equal(svg.getAttribute("viewBox"), "0 0 72 80");
  assert.ok(screen.getByTestId("media").closest("[data-slot=empty-illustration-media]"));
  rerender(<EmptyIllustration variant="preview" mediaFit="cover" mediaClassName="extra" media={<span>Doc</span>} />);
  assert.equal(container.querySelector("svg")!.getAttribute("data-media-fit"), "cover");
  assert.ok(screen.getByText("Doc"));
});

// ---------- MetricCard ----------

test("dashboard metric card shows its label, value, unit, meta and footer", () => {
  render(<MetricCard label="GPUs" value="12" unit="cards" meta="3 nodes" footer="All healthy" description="Online now" leading={<span>icon</span>} title="12 exactly" />);
  const card = screen.getByTitle("12 exactly");
  assert.equal(card.getAttribute("data-state"), "ready");
  assert.equal(card.getAttribute("aria-busy"), null);
  for (const text of ["GPUs", "cards", "3 nodes", "All healthy", "Online now", "icon"]) assert.ok(within(card).getByText(text), text);
  assert.match(card.textContent!, /12cards/);
  assert.equal(screen.queryByRole("button"), null);
});

test("a loading metric card is busy, hides its value and footer and skips the click target", () => {
  let clicks = 0;
  render(<MetricCard label="Requests" value="42" meta="today" description="desc" footer="up 3%" state="loading" statusMessage="hidden" onClick={() => clicks++} content={{ type: "breakdown", items: [{ label: "A", value: 1 }] }} data-testid="card" />);
  const card = screen.getByTestId("card");
  assert.equal(card.getAttribute("aria-busy"), "true");
  assert.equal(screen.queryByText("42"), null);
  assert.equal(screen.queryByText("today"), null);
  assert.equal(screen.queryByText("desc"), null);
  assert.equal(screen.queryByText("up 3%"), null);
  assert.equal(screen.queryByText("hidden"), null);
  assert.equal(screen.queryByRole("list"), null);
  assert.equal(screen.queryByRole("button"), null);
  assert.ok(card.querySelector("[data-slot=metric-card-content-skeleton]"));
  assert.equal(clicks, 0);
});

test("unavailable and failed metrics show a dash and their status message with the right role", () => {
  const { rerender } = render(<MetricCard label="Latency" value="120" unit="ms" description="p95" footer="trend" statusMessage="No samples" state="unavailable" separator content={{ type: "breakdown", items: [{ label: "A", value: 1 }] }} />);
  assert.ok(screen.getByText("—"));
  assert.equal(screen.queryByText("120"), null);
  assert.equal(screen.queryByText("p95"), null);
  assert.equal(screen.queryByText("trend"), null);
  assert.equal(screen.queryByRole("separator"), null);
  assert.equal(screen.queryByRole("list"), null);
  assert.equal(screen.getByRole("status").textContent, "No samples");
  rerender(<MetricCard label="Latency" value="120" statusMessage="Query failed" state="error" />);
  assert.equal(screen.getByRole("alert").textContent, "Query failed");
  rerender(<MetricCard label="Latency" value="120" statusMessage="5 min old" state="stale" />);
  assert.ok(screen.getByText("120"));
  assert.equal(screen.getByRole("status").textContent, "5 min old");
});

test("split layout hides meta when unavailable but keeps it when ready", () => {
  const { rerender } = render(<MetricCard layout="split" label="Tokens" value="9K" meta="last hour" footer="steady" state="unavailable" data-testid="card" />);
  assert.equal(screen.getByTestId("card").getAttribute("data-layout"), "split");
  assert.equal(screen.queryByText("last hour"), null);
  rerender(<MetricCard layout="split" label="Tokens" value="9K" meta="last hour" footer="steady" data-testid="card" />);
  assert.ok(screen.getByText("last hour"));
  assert.ok(screen.getByText("steady"));
});

test("a clickable metric card exposes one named button and honours interactive=false", () => {
  let clicks = 0;
  const { rerender } = render(<MetricCard label="Nodes" value="4" onClick={() => clicks++} />);
  fireEvent.click(screen.getByRole("button", { name: "Nodes" }));
  assert.equal(clicks, 1);
  rerender(<MetricCard label="Nodes" value="4" actionLabel="Open nodes" onClick={() => clicks++} action={<a href="/n">Manage</a>} />);
  fireEvent.click(screen.getByRole("button", { name: "Open nodes" }));
  assert.equal(clicks, 2);
  assert.ok(screen.getByRole("link", { name: "Manage" }));
  rerender(<MetricCard label="Nodes" value="4" interactive={false} onClick={() => clicks++} />);
  assert.equal(screen.queryByRole("button"), null);
  rerender(<MetricCard label="Nodes" value="4" interactive />);
  assert.equal(screen.queryByRole("button"), null);
});

test("breakdown content lists each item, in list or grid layout, with a separator when asked", () => {
  const { rerender } = render(<MetricCard label="Jobs" value="10" separator content={{ type: "breakdown", items: [{ label: "Running", value: 7, tone: "positive" }, { label: "Failed", value: 3, tone: "critical" }] }} />);
  const list = screen.getByRole("list");
  assert.equal(list.getAttribute("data-layout"), "list");
  const items = within(list).getAllByRole("listitem");
  assert.deepEqual(items.map((i) => i.textContent), ["Running7", "Failed3"]);
  assert.ok(screen.getByRole("separator"));
  rerender(<MetricCard label="Jobs" value="10" tone="warning" content={{ type: "breakdown", layout: "grid", items: [{ label: "Queued", value: "2" }] }} />);
  assert.equal(screen.getByRole("list").getAttribute("data-layout"), "grid");
  assert.equal(screen.queryByRole("separator"), null);
});

test("a separator without supporting content is not drawn", () => {
  render(<MetricCard label="Jobs" value="10" separator />);
  assert.equal(screen.queryByRole("separator"), null);
});

test("a chart with fewer than 24 finite points explains that more data is required", () => {
  render(<MetricCard label="QPS" value="5" content={{ type: "visualization", chart: "line", data: [...series(23), Number.NaN, Infinity], accessibleLabel: "QPS trend" }} />);
  assert.ok(screen.getByRole("img", { name: "QPS trend. At least 24 data points are required." }));
});

test("keyboard inspection of a line chart announces each point and clamps at the ends", () => {
  render(<MetricCard label="QPS" value="5" content={{ type: "visualization", chart: "line", data: series(24, (i) => i * 100), accessibleLabel: "QPS trend" }} />);
  const chart = screen.getByRole("img", { name: "QPS trend" });
  assert.equal(chart.getAttribute("tabindex"), "0");
  assert.match(document.getElementById(chart.getAttribute("aria-describedby")!)!.textContent!, /arrow keys/);
  fireEvent.keyDown(chart, { key: "ArrowRight" });
  assert.equal(chart.getAttribute("aria-label"), "QPS trend. Point 2: 100");
  fireEvent.keyDown(chart, { key: "ArrowLeft" });
  fireEvent.keyDown(chart, { key: "ArrowLeft" });
  assert.equal(chart.getAttribute("aria-label"), "QPS trend. Point 1: 0");
  fireEvent.keyDown(chart, { key: "End" });
  assert.equal(chart.getAttribute("aria-label"), "QPS trend. Point 24: 2.3K");
  fireEvent.keyDown(chart, { key: "ArrowRight" });
  assert.equal(chart.getAttribute("aria-label"), "QPS trend. Point 24: 2.3K");
  const tooltip = document.querySelector("[data-slot=metric-card-chart-tooltip]")!;
  assert.equal(tooltip.getAttribute("aria-live"), "polite");
  assert.match(tooltip.textContent!, /Point 24.*2\.3K/);
  fireEvent.keyDown(chart, { key: "Home" });
  assert.equal(chart.getAttribute("aria-label"), "QPS trend. Point 1: 0");
  fireEvent.keyDown(chart, { key: "Enter" });
  assert.equal(chart.getAttribute("aria-label"), "QPS trend. Point 1: 0");
  fireEvent.blur(chart);
  assert.equal(chart.getAttribute("aria-label"), "QPS trend");
  assert.equal(document.querySelector("[data-slot=metric-card-chart-tooltip]"), null);
});

test("pointer inspection picks the nearest point by position and clears on leave", () => {
  render(<MetricCard label="Tokens" value="5" content={{ type: "visualization", chart: "area", data: series(24, (i) => ({ value: i * 0.25, label: `t${i}` })), accessibleLabel: "Tokens" }} />);
  const chart = screen.getByRole("img", { name: "Tokens" });
  chart.getBoundingClientRect = () => ({ left: 0, width: 230, top: 0, height: 60, right: 230, bottom: 60, x: 0, y: 0, toJSON() {} }) as DOMRect;
  fireEvent.pointerMove(chart, { clientX: 230 });
  assert.equal(chart.getAttribute("aria-label"), "Tokens. t23: 5.75");
  fireEvent.pointerMove(chart, { clientX: -50 });
  assert.equal(chart.getAttribute("aria-label"), "Tokens. t0: 0");
  fireEvent.pointerMove(chart, { clientX: 10 });
  assert.equal(chart.getAttribute("aria-label"), "Tokens. t1: 0.25");
  fireEvent.pointerLeave(chart);
  assert.equal(chart.getAttribute("aria-label"), "Tokens");
});

test("bar charts use a custom formatter and handle negative and million-scale values", () => {
  const data = series(24, (i) => (i === 0 ? -2_500_000 : i * 1_000_000));
  render(<MetricCard label="Spend" value="5" content={{ type: "visualization", chart: "bar", data, accessibleLabel: "Spend", formatValue: (v) => `$${v}` }} />);
  const chart = screen.getByRole("img", { name: "Spend" });
  assert.equal(chart.getAttribute("data-chart"), "bar");
  assert.equal(chart.querySelectorAll("rect").length, 24);
  fireEvent.keyDown(chart, { key: "Home" });
  assert.equal(chart.getAttribute("aria-label"), "Spend. Point 1: $-2500000");
  cleanup();
  render(<MetricCard label="Spend" value="5" content={{ type: "visualization", chart: "line", data, accessibleLabel: "Spend" }} />);
  const line = screen.getByRole("img", { name: "Spend" });
  fireEvent.keyDown(line, { key: "Home" });
  assert.equal(line.getAttribute("aria-label"), "Spend. Point 1: -2.5M");
  fireEvent.keyDown(line, { key: "ArrowRight" });
  assert.equal(line.getAttribute("aria-label"), "Spend. Point 2: 1.0M");
});

test("chart default formatting rounds values of ten or more and trims small decimals", () => {
  const data = series(24, (i) => (i === 0 ? 12.6 : i === 1 ? 0.5 : 0));
  render(<MetricCard label="Rate" value="5" content={{ type: "visualization", chart: "line", data, accessibleLabel: "Rate" }} />);
  const chart = screen.getByRole("img", { name: "Rate" });
  fireEvent.keyDown(chart, { key: "Home" });
  assert.equal(chart.getAttribute("aria-label"), "Rate. Point 1: 13");
  fireEvent.keyDown(chart, { key: "ArrowRight" });
  assert.equal(chart.getAttribute("aria-label"), "Rate. Point 2: 0.5");
});

test("a chart inside a clickable card is not separately focusable or inspectable", () => {
  render(<MetricCard label="QPS" value="5" onClick={() => {}} content={{ type: "visualization", chart: "line", data: series(24, () => 0), accessibleLabel: "Flat" }} />);
  const chart = screen.getByRole("img", { name: "Flat" });
  assert.equal(chart.getAttribute("tabindex"), null);
  assert.equal(chart.getAttribute("aria-describedby"), null);
  fireEvent.keyDown(chart, { key: "End" });
  fireEvent.pointerMove(chart, { clientX: 10 });
  assert.equal(chart.getAttribute("aria-label"), "Flat");
});

// ---------- Badge ----------

test("notice severity badges carry their product status and label", () => {
  render(<><Badge status="danger" size="sm">Critical</Badge><Badge size="sm">Update</Badge></>);
  const critical = screen.getByText("Critical").closest("[data-slot=badge]") as HTMLElement;
  assert.equal(critical.getAttribute("data-status"), "danger");
  assert.equal(critical.style.color, badgeStatusTokens.danger.foreground);
  assert.equal(critical.style.backgroundColor, badgeStatusTokens.danger.background);
  const plainKind = screen.getByText("Update").closest("[data-slot=badge]") as HTMLElement;
  assert.equal(plainKind.getAttribute("data-status"), null);
  assert.equal(plainKind.getAttribute("data-variant"), "solid");
  assert.equal(plainKind.querySelector("[data-slot=badge-marker]"), null);
});

test("strong status badges use the strong status tokens, with neutral mapped to its own token", () => {
  render(<><Badge variant="strong" status="success">Ready</Badge><Badge variant="strong" status="neutral">Idle</Badge></>);
  const ready = screen.getByText("Ready").parentElement as HTMLElement;
  assert.equal(ready.style.color, "var(--fg-on-success-strong)");
  assert.equal(ready.style.backgroundColor, "var(--success-strong)");
  const idle = screen.getByText("Idle").parentElement as HTMLElement;
  assert.equal(idle.style.backgroundColor, "var(--neutral-status-strong)");
  assert.equal(idle.style.color, "var(--fg-on-neutral-status-strong)");
});

test("dot and plain badges show a decorative marker sized by badge size unless an icon replaces it", () => {
  render(<>
    <Badge variant="dot" size="sm" status="warning">Warn</Badge>
    <Badge variant="plain" size="lg">Plain</Badge>
    <Badge variant="dot" color="blue">Blue</Badge>
    <Badge variant="dot" leadingIcon={<svg data-testid="icon" />} status="info">With icon</Badge>
    <Badge variant="solid" leadingIcon={<svg />} aria-label="Icon only" />
  </>);
  const marker = (text: string) => screen.getByText(text).parentElement!.querySelector("[data-slot=badge-marker]") as HTMLElement | null;
  assert.equal(marker("Warn")!.getAttribute("aria-hidden"), "true");
  assert.equal(marker("Warn")!.style.width, "6px");
  assert.equal(marker("Warn")!.style.backgroundColor, badgeStatusTokens.warning.icon);
  assert.equal(marker("Plain")!.style.width, "8px");
  assert.equal(marker("Blue")!.style.width, "7px");
  assert.equal(marker("With icon"), null);
  const iconWrap = screen.getByTestId("icon").parentElement as HTMLElement;
  assert.equal(iconWrap.getAttribute("aria-hidden"), "true");
  assert.equal(iconWrap.style.color, badgeStatusTokens.info.icon);
  const iconOnly = screen.getByLabelText("Icon only");
  assert.equal(iconOnly.querySelector("[data-slot=badge-label]"), null);
  assert.equal((iconOnly.querySelector("[data-slot=badge-icon]") as HTMLElement).style.color, "");
});

test("plain badges take the status foreground, falling back to the default foreground", () => {
  render(<><Badge variant="plain" status="danger">Down</Badge><Badge variant="plain">Text</Badge></>);
  assert.equal((screen.getByText("Down").parentElement as HTMLElement).style.color, "var(--fg-danger)");
  assert.equal((screen.getByText("Text").parentElement as HTMLElement).style.color, "var(--fg-default)");
});

test("custom badge colours feed soft, strong and dot treatments, and style overrides win", () => {
  const custom = { base: "rgb(1, 2, 3)", onStrong: "rgb(250, 250, 250)" };
  render(<>
    <Badge color={custom} variant="strong">Strong</Badge>
    <Badge color={{ ...custom, softBackground: "rgb(9, 9, 9)", onSoft: "rgb(8, 8, 8)", dot: "rgb(7, 7, 7)" }}>Soft</Badge>
    <Badge color={{ ...custom, dot: "rgb(7, 7, 7)" }} variant="dot">Dot</Badge>
    <Badge style={{ color: "rgb(4, 4, 4)" }}>Override</Badge>
  </>);
  const strong = screen.getByText("Strong").parentElement as HTMLElement;
  assert.equal(strong.style.backgroundColor, "rgb(1, 2, 3)");
  assert.equal(strong.style.color, "rgb(250, 250, 250)");
  const soft = screen.getByText("Soft").parentElement as HTMLElement;
  assert.equal(soft.style.backgroundColor, "rgb(9, 9, 9)");
  assert.equal(soft.style.color, "rgb(8, 8, 8)");
  assert.equal((screen.getByText("Dot").parentElement!.querySelector("[data-slot=badge-marker]") as HTMLElement).style.backgroundColor, "rgb(7, 7, 7)");
  assert.equal((screen.getByText("Override").parentElement as HTMLElement).style.color, "rgb(4, 4, 4)");
});

test("categorical colour tokens pair light and dark values and expose each palette dot", () => {
  const gray = badgeCategoricalTokens("gray");
  assert.equal(gray.soft.background, "var(--muted)");
  assert.equal(gray.dot, "var(--fg-muted)");
  assert.equal(gray.strong.background, "light-dark(#525252, #A3A3A3)");
  const red = badgeCategoricalTokens("red");
  assert.equal(red.soft.background, "light-dark(#F8DFDF, #7B1D1D)");
  assert.equal(red.strong.foreground, "light-dark(#FFFFFF, #00040D)");
  assert.equal(red.dot, "#EF4444");
  const custom = badgeCategoricalTokens({ base: "#123456", onStrong: "#fff" });
  assert.equal(custom.soft.background, "color-mix(in oklab, #123456 16%, transparent)");
  assert.equal(custom.soft.foreground, "var(--fg-default)");
  assert.equal(custom.dot, "#123456");
  assert.equal(badgeColors.blue, "#3B82F6");
  assert.equal(Object.isFrozen(badgeColors), true);
  assert.equal(Object.keys(badgeColors).length, 17);
});

// ---------- Alert ----------

test("notice alerts announce danger as an alert and other tones as status", () => {
  const { rerender } = render(<Alert status="danger"><AlertIcon><svg /></AlertIcon><AlertTitle>Disk full</AlertTitle><AlertDescription>Free space</AlertDescription><AlertAction><button>Clean</button></AlertAction></Alert>);
  const alert = screen.getByRole("alert");
  assert.equal(alert.getAttribute("data-status"), "danger");
  assert.ok(within(alert).getByText("Disk full"));
  assert.ok(within(alert).getByText("Free space"));
  assert.ok(within(alert).getByRole("button", { name: "Clean" }));
  assert.equal(alert.querySelector("[data-slot=alert-icon]")!.getAttribute("aria-hidden"), "true");
  for (const status of ["info", "warning", "neutral"] as const) {
    rerender(<Alert status={status}><AlertTitle>{status}</AlertTitle></Alert>);
    assert.equal(screen.getByRole("status").getAttribute("data-status"), status);
    assert.equal(screen.queryByRole("alert"), null);
  }
});

test("a default alert without icon or description is a status and a custom role is kept", () => {
  const { rerender } = render(<Alert><AlertTitle>Saved</AlertTitle><AlertDescription>All good</AlertDescription><AlertAction>Undo</AlertAction></Alert>);
  const status = screen.getByRole("status");
  assert.equal(status.getAttribute("data-status"), "default");
  assert.equal(status.querySelector("[data-slot=alert-icon]"), null);
  rerender(<Alert role="note"><AlertTitle>Plain</AlertTitle><AlertAction>Go</AlertAction></Alert>);
  assert.ok(screen.getByRole("note"));
  assert.equal(screen.queryByRole("status"), null);
});

// ---------- Container ----------

test("container provides raised and floating surfaces to its header, body and footer", () => {
  function Surface({ label }: { label: string }) { return <span>{`${label}:${useSurface()}`}</span>; }
  render(<Container data-testid="c"><ContainerHeader><Surface label="header" /></ContainerHeader><ContainerBody><Surface label="body" /></ContainerBody><ContainerFooter><Surface label="footer" /></ContainerFooter></Container>);
  assert.ok(screen.getByText("header:raised"));
  assert.ok(screen.getByText("body:floating"));
  assert.ok(screen.getByText("footer:raised"));
  assert.equal(screen.getByText("header:raised").closest("header")!.getAttribute("data-slot"), "container-header");
  assert.equal(screen.getByText("footer:raised").closest("footer")!.getAttribute("data-slot"), "container-footer");
});

test("container body caps its height only when maxHeight is set and keeps caller styles", () => {
  const { rerender } = render(<ContainerBody data-testid="b" style={{ color: "red" }}>x</ContainerBody>);
  const body = screen.getByTestId("b");
  assert.equal(body.style.maxHeight, "");
  assert.equal(body.style.color, "red");
  rerender(<ContainerBody data-testid="b" style={{ color: "red" }} maxHeight={240}>x</ContainerBody>);
  assert.equal(screen.getByTestId("b").style.maxHeight, "240px");
  assert.equal(screen.getByTestId("b").style.color, "red");
});
