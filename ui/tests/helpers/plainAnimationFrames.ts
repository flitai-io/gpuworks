// Animation frames as plain timers that always run.
//
// helpers/dom.ts drops every frame a finished test left pending, so a chart
// disposed with the test is never painted into. framer-motion's frame loop is
// process-wide and asks for the next frame only once the previous one has run:
// one dropped frame leaves it waiting forever, and from then on every exit
// animation (AnimatePresence mode="wait") in the process hangs. A test that ends
// a moment before its component schedules a frame loses it that way, which on a
// loaded CI runner broke the next tests of the file. A file with no chart to
// protect installs these instead. They must be installed before framer-motion
// is imported, which captures requestAnimationFrame when it loads.
export function installPlainAnimationFrames() {
  const timers = new Map<number, ReturnType<typeof setTimeout>>();
  let next = 0;
  const request = (callback: (time: number) => void) => {
    const id = ++next;
    timers.set(id, setTimeout(() => {
      timers.delete(id);
      callback(performance.now());
    }, 16));
    return id;
  };
  const cancel = (id: number) => {
    clearTimeout(timers.get(id));
    timers.delete(id);
  };
  for (const target of [globalThis, (globalThis as { window?: object }).window ?? globalThis]) {
    Object.defineProperties(target, {
      requestAnimationFrame: { configurable: true, writable: true, value: request },
      cancelAnimationFrame: { configurable: true, writable: true, value: cancel },
    });
  }
  /** Frames requested and not yet run or cancelled. */
  return { pendingFrameCount: () => timers.size };
}
