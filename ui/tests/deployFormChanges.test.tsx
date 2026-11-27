import { dom } from "./helpers/dom.ts";
import assert from "node:assert/strict";
import { after, afterEach, test } from "node:test";
import React from "react";

// A card index names a card on one machine (GWKS-522): when the operator picks
// another node, the card choice made for the previous one starts over. The
// hook is wired to antd's onValuesChange, as on the deploy page, so only what
// the operator changes reaches it; loading an edit's values does not.

const { render, cleanup, fireEvent, screen } = await import(
  "@testing-library/react"
);
const { Form } = await import("antd");
const useDeployFormChanges = (
  await import("../src/pages/deploy/useDeployFormChanges.ts")
).default;

afterEach(() => cleanup());
after(() => dom.window.close());

function mount(initial: Record<string, unknown>) {
  let form: import("antd").FormInstance;
  function Host() {
    [form] = Form.useForm();
    const onValuesChange = useDeployFormChanges({
      form,
      templates: [],
      images: [],
    });
    return (
      <Form form={form} initialValues={initial} onValuesChange={onValuesChange}>
        <Form.Item name="node_uuid">
          <input aria-label="node" />
        </Form.Item>
        <Form.Item name="tp">
          <input aria-label="tp" />
        </Form.Item>
      </Form>
    );
  }
  render(<Host />);
  return {
    gpuIndexes: () => form.getFieldValue("gpu_indexes"),
    nodeUUID: () => form.getFieldValue("node_uuid"),
    setValues: (values: Record<string, unknown>) => form.setFieldsValue(values),
  };
}

test("a changed node starts the card choice over", () => {
  const host = mount({ node_uuid: "card-7", gpu_indexes: [7], tp: 1 });
  fireEvent.change(screen.getByLabelText("node"), {
    target: { value: "one-card" },
  });
  assert.equal(host.nodeUUID(), "one-card");
  assert.equal(host.gpuIndexes(), undefined);
});

test("loading an edit's node and cards keeps the cards", () => {
  const host = mount({});
  host.setValues({ node_uuid: "card-7", gpu_indexes: [7] });
  assert.deepEqual(host.gpuIndexes(), [7]);
});

test("changing something else leaves the cards alone", () => {
  const host = mount({ node_uuid: "card-7", gpu_indexes: [7], tp: 1 });
  fireEvent.change(screen.getByLabelText("tp"), { target: { value: "2" } });
  assert.deepEqual(host.gpuIndexes(), [7]);
});
