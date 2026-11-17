import { useState } from "react";
import { modelOrganizationColor, modelOrganizationIcon } from "../../inferenceTemplatePresentation";

export function CatalogAvatar({ modelID }: { modelID: string }) {
  const publisher = modelID.split("/", 1)[0] || modelID;
  const icon = modelOrganizationIcon(modelID);
  const [failedIcon, setFailedIcon] = useState<string>();
  const showIcon = icon && icon !== failedIcon;

  return (
    <span className="catalog-avatar" aria-hidden="true"
      style={showIcon ? undefined : { backgroundColor: modelOrganizationColor(publisher) }}>
      {showIcon
        ? <img src={icon} alt="" onError={() => setFailedIcon(icon)} />
        : Array.from(publisher)[0]?.toUpperCase()}
    </span>
  );
}
