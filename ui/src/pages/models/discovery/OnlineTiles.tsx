import { useTranslation } from "react-i18next";
import { Badge } from "../../../components/ui/badge";
import type { ReactNode } from "react";
import { ModelTileFrame, ModelSourceTag } from "../../../pages/ModelsPageParts";
import { CatalogAvatar } from "./CatalogAvatar";
import type { HubModel, Provider } from "./hubs";

interface Props {
  models: HubModel[];
  source: Provider;
  onOpen: (id: string) => void;
  registerButton: (model: HubModel) => ReactNode;
  taskLabel: (task: string) => string;
}

export function OnlineTiles({
  models,
  source,
  onOpen,
  registerButton,
  taskLabel,
}: Props) {
  const {t} = useTranslation("models");
  return (
    <div className="model-tile-grid discovery-tiles">
      {models.map((m) => {
        return (
          <div key={m.id} data-model={m.id}>
            <ModelTileFrame
              name={m.id}
              onOpen={() => onOpen(m.id)}
              logo={<CatalogAvatar modelID={m.id} />}
              tags={
                <>
                  {m.tasks.map((task) => (
                    <Badge key={task} size="sm" color="blue">{taskLabel(task)}</Badge>
                  ))}
                </>
              }
              actions={registerButton(m)}
              footer={
                <>
                  <ModelSourceTag source={source} badge />
                  {m.downloads != null && (
                    <span>{t("catalog.download")} {m.downloads.toLocaleString()}</span>
                  )}
                  {m.updated && <span>{m.updated.slice(0, 10)}</span>}
                </>
              }
            />
          </div>
        );
      })}

    </div>
  );
}
