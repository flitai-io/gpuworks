import { Button, Input, Select } from "antd";
import { useTranslation } from "react-i18next";
import { NO_FILTERS, type Filters, type GroupBy, type OverviewModel, type SortBy, type StateFilter } from "./overviewModel";

const STATES: Exclude<StateFilter, "">[] = ["free", "sleepers", "held", "offline"];
const SORTS: SortBy[] = ["name", "free", "contention"];

export interface ViewState {
  filters: Filters;
  groupBy: GroupBy;
  sortBy: SortBy;
}

export const DEFAULT_VIEW: ViewState = { filters: NO_FILTERS, groupBy: "node", sortBy: "name" };

/** isDefault says the view is the unfiltered, by-node, by-name one. */
export function isDefault(view: ViewState): boolean {
  const { filters } = view;
  return !filters.models.length && !filters.label && !filters.state && !filters.search && view.groupBy === "node" && view.sortBy === "name";
}

/**
 * OverviewFilters narrows the heatmap by GPU model, node label and node state,
 * finds the cards a deployment occupies, and picks how the nodes are grouped
 * and ordered. Everything it offers comes from the response itself.
 */
export default function OverviewFilters({ model, view, onChange }: { model: OverviewModel; view: ViewState; onChange: (view: ViewState) => void }) {
  const { t } = useTranslation("scheduling");
  const set = (patch: Partial<Filters>) => onChange({ ...view, filters: { ...view.filters, ...patch } });
  const labelOptions = model.labels.flatMap(({ key, values }) => values.map((value) => ({ value: `${key}=${value}`, label: `${key}=${value}` })));
  return (
    <div className="resource-table-toolbar hm-filters" role="search" aria-label={t("filters.title")}>
      <div className="resource-table-toolbar-left hm-filter-fields">
        <Input
          allowClear
          aria-label={t("filters.search")}
          placeholder={t("filters.searchPlaceholder")}
          value={view.filters.search}
          onChange={(event) => set({ search: event.target.value })}
          className="hm-search-input"
        />
        <Select<string[]> className="hm-filter-select"
          mode="multiple"
          allowClear
          maxTagCount="responsive"
          popupMatchSelectWidth={280}
          aria-label={t("filters.models")}
          placeholder={t("filters.modelsPlaceholder")}
          value={view.filters.models}
          options={model.models.map((name) => ({ value: name, label: name }))}
          onChange={(models) => set({ models })}
        />
        <Select<string> className="hm-filter-select"
          allowClear
          showSearch
          popupMatchSelectWidth={280}
          aria-label={t("filters.label")}
          placeholder={t("filters.labelPlaceholder")}
          value={view.filters.label || undefined}
          options={labelOptions}
          onChange={(label) => set({ label: label ?? "" })}
          notFoundContent={t("filters.noLabels")}
        />
        <Select<StateFilter> className="hm-filter-select"
          allowClear
          popupMatchSelectWidth={false}
          aria-label={t("filters.state")}
          placeholder={t("filters.statePlaceholder")}
          value={view.filters.state || undefined}
          options={STATES.map((state) => ({ value: state, label: t(`filters.stateOptions.${state}`) }))}
          onChange={(state) => set({ state: state ?? "" })}
        />
        {!isDefault(view) && <Button type="link" onClick={() => onChange(DEFAULT_VIEW)}>{t("filters.reset")}</Button>}
      </div>
      <div className="resource-table-toolbar-right hm-view-options flex gap-1">
        <Select<GroupBy>
          variant="borderless"
          popupMatchSelectWidth={false}
          aria-label={t("filters.groupBy")}
          value={view.groupBy}
          options={[
            { value: "node", label: t("filters.group.node") },
            { value: "model", label: t("filters.group.model") },
            ...model.labels.map(({ key }) => ({ value: `label:${key}` as GroupBy, label: t("filters.group.label", { key }) })),
          ]}
          onChange={(groupBy) => onChange({ ...view, groupBy })}
        />
        <Select<SortBy>
          variant="borderless"
          popupMatchSelectWidth={false}
          aria-label={t("filters.sortBy")}
          value={view.sortBy}
          options={SORTS.map((sort) => ({ value: sort, label: t(`filters.sort.${sort}`) }))}
          onChange={(sortBy) => onChange({ ...view, sortBy })}
        />
      </div>
    </div>
  );
}
