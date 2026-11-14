// A chart disposed while its progressive layer still has frames to paint must
// not fail the run: what those frames throw once the test has ended is dropped (see
// tests/helpers/dom.ts). zrender paints at most 15 ms per frame; a getTime
// that advances 20 ms per call makes every frame run out, which is what a
// loaded CI runner does to the analytics heat map.
import assert from "node:assert/strict";
import { after, test } from "node:test";
import { dom, pendingFrameCount } from "./helpers/dom.ts";
import * as fx from "./helpers/analyticsFixtures.ts";

fx.installCanvas(dom);
const { waitFor } = await import("@testing-library/react");
const { platformApi, setPlatformAPI } = await import("zrender/lib/core/platform.js");
const echarts = await import("echarts/core");
const { HeatmapChart } = await import("echarts/charts");
const { GridComponent, VisualMapComponent } = await import("echarts/components");
const { CanvasRenderer } = await import("echarts/renderers");
echarts.use([HeatmapChart, GridComponent, VisualMapComponent, CanvasRenderer]);

const realGetTime = platformApi.getTime;
after(() => {
  setPlatformAPI({ getTime: realGetTime });
  dom.window.close();
});

test("a heat map disposed mid progressive paint leaves frames behind", () => {
  let now = 0;
  setPlatformAPI({ getTime: () => (now += 20) });
  const host = document.createElement("div");
  document.body.appendChild(host);
  const chart = echarts.init(host, undefined, { renderer: "canvas", width: 600, height: 400 });
  const data: number[][] = [];
  for (let y = 0; y < 100; y++) for (let x = 0; x < 4; x++) data.push([x, y, x + y]);
  chart.setOption({
    xAxis: { type: "category", data: ["a", "b", "c", "d"] },
    yAxis: { type: "category", data: Array.from({ length: 100 }, (_, y) => String(y)) },
    visualMap: { min: 0, max: 200, show: false },
    series: [{ type: "heatmap", data, progressive: 2000 }],
  });
  assert.ok(pendingFrameCount() > 0, "the paint must still be pending, or this test proves nothing");
  chart.dispose();
  host.remove();
});

test("the next test runs after those frames without an error from the disposed chart", async () => {
  setPlatformAPI({ getTime: realGetTime });
  await waitFor(() => assert.equal(pendingFrameCount(), 0));
});
