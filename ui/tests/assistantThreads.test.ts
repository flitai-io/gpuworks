import assert from "node:assert/strict";
import { test } from "node:test";
import { IDBFactory } from "fake-indexeddb";
import { MAX_THREADS, memoryThreadDB, newestFirst, openThreadDB, overflow, titleFrom, type Thread } from "../src/assistant/threads.ts";

const thread = (id: string, userID: number, updatedAt: number): Thread => ({
  id, userID, title: id, createdAt: updatedAt, updatedAt,
  turns: [{ task: "ask", question: id, images: ["data:image/png;base64,AAAA"], status: "done", text: "answer", reasoning: "", steps: [] }],
});

for (const [name, open] of [["IndexedDB", () => openThreadDB(new IDBFactory())], ["memory", async () => memoryThreadDB()]] as const) {
  test(`${name}: threads are kept per account, read back whole and removed by id`, async () => {
    const db = await open();
    await db.put(thread("a", 1, 10));
    await db.put(thread("b", 1, 20));
    await db.put(thread("c", 2, 30));
    const mine = (await db.list(1)).sort(newestFirst);
    assert.deepEqual(mine.map((x) => x.id), ["b", "a"]);
    assert.deepEqual(mine[1].turns[0].images, ["data:image/png;base64,AAAA"]);
    assert.deepEqual((await db.list(2)).map((x) => x.id), ["c"]);
    await db.put({ ...thread("a", 1, 40), title: "renamed" });
    assert.equal((await db.list(1)).find((x) => x.id === "a")?.title, "renamed");
    await db.remove(["a", "missing"]);
    await db.remove([]);
    assert.deepEqual((await db.list(1)).map((x) => x.id), ["b"]);
  });
}

test("a browser without IndexedDB is told so, and the caller keeps threads in memory", async () => {
  await assert.rejects(openThreadDB(undefined));
});

test("overflow drops the oldest beyond the limit", () => {
  const all = Array.from({ length: MAX_THREADS + 2 }, (_, i) => thread(`t${i}`, 1, i));
  assert.deepEqual(overflow(all).sort(), ["t0", "t1"]);
  assert.deepEqual(overflow(all.slice(0, 3)), []);
});

test("titleFrom takes the first non-empty line, shortened", () => {
  assert.equal(titleFrom("\n  why is my node offline?  \nmore"), "why is my node offline?");
  const long = "x".repeat(60);
  assert.equal(titleFrom(long).length, 40);
  assert.ok(titleFrom(long).endsWith("…"));
  assert.equal(titleFrom(""), "");
});
