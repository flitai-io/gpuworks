// Chromium starts a SharedWorker script on the page but reports completion on
// the worker target. The target ID is the script's Network.requestId.
export async function watchSharedWorkerScripts(page, open, changed) {
  const session = await page.context().newCDPSession(page);
  const scripts = new Map();
  const workers = new Set();
  const attaching = new Set();
  let documentId;
  const hasOtherTargets = () => page.frames().length > 1 || page.workers().length > 0;
  let otherTargetSeen = hasOtherTargets();
  page.on("frameattached", () => { otherTargetSeen = true; });
  page.on("worker", () => { otherTargetSeen = true; });

  function observeWorker(id) {
    if (!scripts.has(id) || !workers.has(id) || attaching.has(id)) return;
    attaching.add(id);
    // Inspector.workerScriptLoaded is replayed on attachment. A disappearing
    // target supplies no evidence that the script completed.
    session.send("Target.attachToTarget", { targetId: id, flatten: false }).catch(() => {});
  }

  session.on("Network.requestWillBeSent", ({ requestId, loaderId, type, request }) => {
    if (type !== "Script") return;
    scripts.set(requestId, { url: request.url, loader: loaderId || documentId, loaded: false });
    observeWorker(requestId);
  });
  const end = ({ requestId }) => { scripts.delete(requestId); };
  session.on("Network.loadingFinished", end);
  session.on("Network.loadingFailed", end);
  session.on("Target.targetCreated", ({ targetInfo }) => {
    if (targetInfo.type !== "shared_worker") return;
    workers.add(targetInfo.targetId);
    observeWorker(targetInfo.targetId);
  });
  session.on("Target.targetDestroyed", ({ targetId }) => {
    workers.delete(targetId);
    attaching.delete(targetId);
  });
  session.on("Target.receivedMessageFromTarget", ({ targetId, sessionId, message }) => {
    if (!attaching.has(targetId) || JSON.parse(message).method !== "Inspector.workerScriptLoaded") return;
    const script = scripts.get(targetId);
    if (script && !script.loaded) {
      script.loaded = true;
      reconcile();
      changed();
    }
    session.send("Target.detachFromTarget", { sessionId }).catch(() => {});
  });
  session.on("Page.frameNavigated", ({ frame }) => {
    if (frame.parentId) return;
    documentId = frame.loaderId;
    otherTargetSeen = hasOtherTargets();
    for (const [id, script] of scripts) {
      if (script.loader !== documentId) scripts.delete(id);
    }
  });
  await session.send("Page.enable");
  await session.send("Network.enable");
  await session.send("Target.setDiscoverTargets", { discover: true, filter: [{ type: "shared_worker" }, { exclude: true }] });

  // Playwright omits the CDP ID. Reconcile only a complete group of scripts
  // with the same URL: every CDP ID in it must be a confirmed worker and
  // there must be exactly as many open Playwright requests. Consume both
  // sides once, so an old worker cannot release another request to its URL.
  function reconcile() {
    if (!scripts.size || !open.size || otherTargetSeen || hasOtherTargets()) return;
    const byUrl = new Map();
    for (const [id, script] of scripts) {
      const group = byUrl.get(script.url) ?? { ids: [], requests: [], loading: false };
      group.ids.push(id);
      if (!script.loaded) group.loading = true;
      byUrl.set(script.url, group);
    }
    for (const request of open) {
      if (request.resourceType() === "script") byUrl.get(request.url())?.requests.push(request);
    }
    for (const { ids, requests, loading } of byUrl.values()) {
      if (loading || ids.length !== requests.length) continue;
      for (const request of requests) open.delete(request);
      for (const id of ids) scripts.delete(id);
    }
  }
  return reconcile;
}
