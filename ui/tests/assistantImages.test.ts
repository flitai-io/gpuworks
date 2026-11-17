import assert from "node:assert/strict";
import { test } from "node:test";
import { dom } from "./helpers/dom.ts";

Object.defineProperty(globalThis, "FileReader", { value: dom.window.FileReader, configurable: true });
const { prepareImage, keepNewestImages, dataURLBytes, ImagePrepError } = await import("../src/assistant/images.ts");
type Decoded = import("../src/assistant/images.ts").Decoded;

const limit = { per_message: 4, per_request: 8, max_bytes: 2 << 20, max_side: 1568 };
const file = (bytes: number, type = "image/png", name = "shot.png") => new dom.window.File([new Uint8Array(bytes)], name, { type }) as unknown as File;

/** decoderOf is a canvas stand-in: the image's size, and an encoder that records what it was asked for. */
function decoderOf(width: number, height: number, encodedBytes: number) {
  const calls: number[][] = [];
  const decode = async (): Promise<Decoded> => ({
    width, height,
    async encode(w, h, q) {
      calls.push([w, h, q]);
      return "data:image/jpeg;base64," + Buffer.alloc(encodedBytes).toString("base64");
    },
  });
  return { decode, calls };
}

test("a screenshot larger than the server's longest side is shrunk to it and re-encoded", async () => {
  const { decode, calls } = decoderOf(3136, 1960, 300_000);
  const url = await prepareImage(file(400_000), limit, decode);
  assert.deepEqual(calls, [[1568, 980, 0.85]]);
  assert.ok(url.startsWith("data:image/jpeg;base64,"));
  assert.equal(dataURLBytes(url), 300_000);
});

test("a small image within the side limit is sent as it is", async () => {
  const { decode, calls } = decoderOf(800, 600, 1);
  const url = await prepareImage(file(50_000), limit, decode);
  assert.equal(calls.length, 0);
  assert.ok(url.startsWith("data:image/png;base64,"));
  assert.equal(dataURLBytes(url), 50_000);
});

test("a heavy image within the side limit is re-encoded at its own size", async () => {
  const { decode, calls } = decoderOf(1200, 900, 400_000);
  await prepareImage(file(1_500_000), limit, decode);
  assert.deepEqual(calls, [[1200, 900, 0.85]]);
});

test("without a canvas the file goes as it is when it fits, and is refused when it does not", async () => {
  const none = async () => null;
  assert.equal(dataURLBytes(await prepareImage(file(1000), limit, none)), 1000);
  await assert.rejects(prepareImage(file((2 << 20) + 1), limit, none), (e: unknown) => e instanceof ImagePrepError && e.reason === "too_large");
  // A decoder that throws is treated as none.
  assert.equal(dataURLBytes(await prepareImage(file(10), limit, async () => { throw new Error("corrupt"); })), 10);
});

test("an image still over the byte limit after shrinking is refused", async () => {
  const { decode } = decoderOf(4000, 3000, (2 << 20) + 10);
  await assert.rejects(prepareImage(file(9_000_000), limit, decode), (e: unknown) => e instanceof ImagePrepError && e.reason === "too_large" && e.name === "shot.png");
});

test("a file of another type is refused before it is read", async () => {
  await assert.rejects(prepareImage(file(10, "image/svg+xml", "a.svg"), limit, async () => null), (e: unknown) => e instanceof ImagePrepError && e.reason === "format");
  await assert.rejects(prepareImage(file(10, "application/pdf", "a.pdf"), limit, async () => null), ImagePrepError);
});

test("keepNewestImages keeps the question's images and then the newest turns'", () => {
  const turns = [["a1", "a2"], [], ["b1", "b2", "b3"], ["c1"]];
  assert.deepEqual(keepNewestImages(turns, 2, 8), [["a1", "a2"], [], ["b1", "b2", "b3"], ["c1"]]);
  assert.deepEqual(keepNewestImages(turns, 4, 8), [[], [], ["b1", "b2", "b3"], ["c1"]]);
  // Within a turn the newest images stay.
  assert.deepEqual(keepNewestImages(turns, 5, 8), [[], [], ["b2", "b3"], ["c1"]]);
  assert.deepEqual(keepNewestImages(turns, 8, 8), [[], [], [], []]);
  assert.deepEqual(keepNewestImages([], 0, 8), []);
});

test("dataURLBytes counts the decoded bytes", () => {
  for (const n of [0, 1, 2, 3, 10, 1000]) {
    assert.equal(dataURLBytes("data:image/png;base64," + Buffer.alloc(n).toString("base64")), n);
  }
});
