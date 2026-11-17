import { useMemo, useState, type ReactNode } from "react";
import { Select, Space, Typography } from "antd";
import { useTranslation } from "react-i18next";
import { Link } from "react-router-dom";
import { api, usePolling, type ModelCore } from "../../api";
import { ModelSourceTag } from "../ModelsPageParts";
import ModelRegistrationDetails from "./ModelRegistrationDetails";

/** How many models one search offers; typing narrows the list on the server. */
export const MODEL_PICKER_PAGE_SIZE = 50;

const modelLabel = (model: ModelCore): ReactNode => (
  <Space size={6} wrap>
    <span>{model.name || model.source_id}</span>
    <ModelSourceTag source={model.source} />
    <ModelRegistrationDetails model={model} />
  </Space>
);

/**
 * ModelPicker selects the model to deploy by searching the registered models
 * on the server (GET /models with page parameters) instead of listing them
 * all. The chosen model keeps its name when a search leaves it out: the page
 * reads it by id and passes it in as `selected`.
 *
 * It works as an antd Form control (value/onChange).
 */
export default function ModelPicker({
  value,
  onChange,
  selected,
  id,
  ariaLabel,
}: {
  value?: number;
  onChange?: (value: number | undefined) => void;
  selected?: ModelCore;
  id?: string;
  /** Names the control where no form label does (a mount row). */
  ariaLabel?: string;
}) {
  const { t } = useTranslation("deploy");
  const [search, setSearch] = useState("");
  const page = usePolling(
    () => api.modelsPage({ page: 1, page_size: MODEL_PICKER_PAGE_SIZE, q: search }),
    0,
    [search],
  );
  const items = page.data?.items;
  const options = useMemo(() => {
    const modelOption = (model: ModelCore) => {
      const name = model.name || model.source_id;
      const revision = model.revision
        ? t("weightSourceModal.revision", { revision: model.revision })
        : t("weightSourceModal.revisionUnknown");
      return {
        value: model.id,
        label: (
          <span title={`${name} · ${model.source} · ${revision}`}>
            <span>{name}</span>{" "}
            <ModelSourceTag source={model.source} />
            <Typography.Text type="secondary">{revision}</Typography.Text>
          </span>
        ),
        model,
      };
    };
    const out = (items ?? []).map(modelOption);
    if (selected && selected.id === value && !out.some((option) => option.value === value)) {
      out.unshift(modelOption(selected));
    }
    return out;
  }, [items, selected, value, t]);
  // No model at all, rather than none matching what was typed.
  const noModels = !search && page.data?.total === 0;
  return (
    <Select
      id={id}
      aria-label={ariaLabel}
      showSearch
      filterOption={false}
      onSearch={setSearch}
      onOpenChange={(open) => { if (!open) setSearch(""); }}
      value={value}
      onChange={onChange}
      options={options}
      optionRender={(option) => modelLabel(option.data.model)}
      loading={page.loading}
      placeholder={t("basic.modelPlaceholder")}
      notFoundContent={
        noModels ? (
          <Typography.Text type="secondary">
            {t("basic.noModels")} <Link to="/models">{t("basic.registerModel")}</Link>
          </Typography.Text>
        ) : undefined
      }
    />
  );
}
