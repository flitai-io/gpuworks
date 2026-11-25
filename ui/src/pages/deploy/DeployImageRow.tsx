import { Alert, AutoComplete, Form, Typography } from "antd";
import { useTranslation } from "react-i18next";
import type {
  AcquisitionResult,
  ImageCatalogEntry,
  ProductWarning,
} from "../../api";
import { warningText } from "./deployWarnings";
import { useControlLocked } from "./DeployEditContext";

// `evaluated` separates "the server has not answered yet" from "the server
// answered and could not resolve an image". Without it a failed evaluation —
// which returns no acquisition at all — is indistinguishable from a pending
// one, and the row waits forever on a request that already came back.
export default function DeployImageRow({
  acquisition,
  evaluated = false,
  images = [],
  warnings = [],
}: {
  acquisition?: AcquisitionResult;
  evaluated?: boolean;
  images?: ImageCatalogEntry[];
  warnings?: ProductWarning[];
}) {
  const { t } = useTranslation("deploy");
  const form = Form.useFormInstance();
  const engineLocked = useControlLocked("engine");
  const selectedImage = Form.useWatch("image", { form, preserve: true }) as
    string | undefined;
  const templateID = Form.useWatch("template_id", { form, preserve: true });
  const image = acquisition?.image;
  const canonicalImage = image?.image?.trim() || "";
  const compatibleOptions = (image?.options || []).filter(
    (option) => option.compatible,
  );
  const selectableOptions =
    compatibleOptions.some((option) => option.image === canonicalImage) ||
    !canonicalImage
      ? compatibleOptions
      : [
          { image: canonicalImage, reason: "selected_image", compatible: true },
          ...compatibleOptions,
        ];
  const effectiveImage = selectedImage || canonicalImage || "";

  return (
    <div className="deploy-workbench-row">
      <div className="deploy-workbench-label">
        {t("workbench.acquisition.imageTitle")}
      </div>
      <div className="deploy-workbench-body deploy-image-picker">
        <AutoComplete
          aria-label={t("workbench.acquisition.imageOptions")}
          value={effectiveImage}
          options={selectableOptions.map((option) => ({
            value: option.image,
            label: option.image,
          }))}
          onChange={(value: string) => {
            const catalogImage = images.find(
              (candidate) =>
                candidate.image_ref.toLowerCase() === value.toLowerCase(),
            );
            // The image follows the engine; an edit cannot change the engine.
            if (!templateID && catalogImage?.engine && !engineLocked) {
              form.setFieldsValue({
                image: value,
                engine: catalogImage.engine,
              });
              return;
            }
            form.setFieldValue("image", value);
          }}
          placeholder={t("catalog.imagePlaceholder")}
          className="deploy-image-select"
          style={{ width: "100%" }}
        />
        {!acquisition && !evaluated && (
          <Typography.Text type="secondary">
            {t("workbench.acquisition.loading")}
          </Typography.Text>
        )}
        {!acquisition && evaluated && !selectedImage && (
          <Alert
            type="error"
            showIcon
            message={t("workbench.acquisition.unavailable")}
          />
        )}
        {acquisition && !canonicalImage && !selectedImage && (
          <Alert
            type="warning"
            showIcon
            message={t("workbench.acquisition.imageUnavailable")}
            description={
              warnings.find((warning) => warning.code.startsWith("IMAGE_"))
                ? warningText(
                    t,
                    warnings.find((warning) =>
                      warning.code.startsWith("IMAGE_"),
                    )!,
                  )
                : t("workbench.acquisition.imageUnavailableDescription")
            }
          />
        )}
        {selectedImage &&
          !images.some(
            (candidate) =>
              candidate.image_ref.toLowerCase() === selectedImage.toLowerCase(),
          ) && (
            <Alert
              type="warning"
              showIcon
              message={t("catalog.imageUnverified")}
            />
          )}
        <Typography.Text type="secondary">
          {t("workbench.acquisition.imageSelectionDescription")}
        </Typography.Text>
      </div>
    </div>
  );
}
