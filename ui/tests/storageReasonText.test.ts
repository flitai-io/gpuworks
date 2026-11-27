import assert from "node:assert/strict";
import { test } from "node:test";
import { createI18n } from "./helpers/dom.ts";
import { storageReasonText } from "../src/storageDisplay.ts";

for (const language of ["en", "zh"] as const) {
  test(`a storage reason reads from the common words or from the storage pages' own (${language})`, async () => {
    const { i18n, locale } = await createI18n(["storage"], language);
    const own = storageReasonText(i18n.t, "not_shared_with_node");
    assert.equal(own, locale.storage.storageReason.not_shared_with_node);
    assert.match(own, /manifest/, "says how to make the node share the directory");
    assert.equal(storageReasonText(i18n.t, "missing_client"), locale.common.storageReason.missing_client);
    assert.equal(storageReasonText(i18n.t, "a_code_from_a_newer_agent"), "a_code_from_a_newer_agent");
  });
}
