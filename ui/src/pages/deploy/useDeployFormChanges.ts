import type { FormInstance } from "antd";
import type { ImageCatalogEntry, InferenceTemplateIndexItem } from "../../api";
import type { ImageSelectionMode } from "./deployHelpers";

export default function useDeployFormChanges({
  form,
  templates,
  images,
}: {
  form: FormInstance;
  templates: InferenceTemplateIndexItem[];
  images: ImageCatalogEntry[];
}) {
  return (changed: Record<string, unknown>) => {
    if (Object.prototype.hasOwnProperty.call(changed, "target_vendor")) {
      const vendor = String(changed.target_vendor || "");
      // A changed vendor re-opens the node choice: the next evaluation's
      // eligible_nodes answer marks the stale node ineligible, and the
      // dropdown disabled it already. Clearing here keeps the draft honest.
      form.setFieldValue("node_uuid", undefined);
      const currentProfile = templates
        .flatMap((item) => item.profiles.map((profile) => ({ item, profile })))
        .find(
          ({ item, profile }) =>
            item.id === form.getFieldValue("template_id") &&
            profile.name === form.getFieldValue("template_spec_name") &&
            (profile.source || item.source) ===
              form.getFieldValue("template_spec_source"),
        );
      if (
        currentProfile &&
        currentProfile.profile.gpus.vendor.toLowerCase() !==
          vendor.toLowerCase()
      ) {
        // Drop what the template wrote along with it: its image, and its
        // engine, which useDeployPrefill then resets to the server default.
        form.setFieldsValue({
          template_id: undefined,
          template_spec_source: undefined,
          template_spec_name: undefined,
          image: undefined,
          engine: undefined,
        });
      }
    }
    if (
      Object.prototype.hasOwnProperty.call(changed, "node_uuid") &&
      !Object.prototype.hasOwnProperty.call(changed, "gpu_indexes")
    ) {
      // Card indexes belong to the previous node; the server refuses a card
      // the node lacks (PLACEMENT_GPU_NOT_ON_NODE).
      form.setFieldValue("gpu_indexes", undefined);
    }
    if (Object.prototype.hasOwnProperty.call(changed, "image_mode")) {
      const mode = changed.image_mode as ImageSelectionMode;
      form.setFieldsValue({
        image: mode === "auto" ? undefined : form.getFieldValue("image"),
        image_series_id: undefined,
        image_version_id: undefined,
      });
    }
    if (Object.prototype.hasOwnProperty.call(changed, "image_series_id"))
      form.setFieldsValue({ image: undefined, image_version_id: undefined });
    if (Object.prototype.hasOwnProperty.call(changed, "image_version_id")) {
      const version = images.find(
        (image) => image.id === changed.image_version_id,
      );
      form.setFieldValue("image", version?.image_ref);
    }
  };
}
