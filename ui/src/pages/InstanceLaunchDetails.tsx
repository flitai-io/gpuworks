import DeploymentInfoField, { DeploymentInfoList } from "./DeploymentInfoField";
import { useId, useState } from "react";

import { Button } from "../components/ui/button";
import { useTranslation } from "react-i18next";
import type { Instance } from "../api";
import { ConsoleIcon } from "../ui-kit/consoleIcons";
import InstanceLaunchFacts from "./InstanceLaunchFacts";
import InstanceLaunchValue from "./InstanceLaunchValue";

// Under --ipc=host the launch states no /dev/shm size: the container uses the
// host's own (docs/reference/deployment-launch-sources.md).
function sharesHostIPC(args: string[] | undefined): boolean {
  return (args || []).some((arg, index, all) => {
    const token = arg.trim();
    return token === "--ipc=host" || token === "--ipc host" || (token === "--ipc" && all[index + 1]?.trim() === "host");
  });
}

export default function InstanceLaunchDetails({ inst, preparation }: { inst: Instance; preparation: string }) {
  const { t } = useTranslation("deployments");
  const id = useId();
  const [open, setOpen] = useState<"preparation" | "runtime" | "facts" | null>(null);
  const environment = Object.entries(inst.env ?? {});
  const hasRuntime = Boolean(environment.length || inst.mounts?.length || inst.shm_bytes || inst.docker_args?.length);
  const hostIPC = sharesHostIPC(inst.docker_args);

  return (
    <div className="instance-launch-details">
      <div className="instance-launch-buttons">
        {preparation && (
          <Button
            variant="tertiary"
            size="sm"
            leadingIcon={ConsoleIcon.logs}
            trailingIcon={ConsoleIcon.chevronDown}
            active={open === "preparation"}
            aria-expanded={open === "preparation"}
            aria-controls={`${id}-preparation`}
            onClick={() => setOpen(open === "preparation" ? null : "preparation")}
          >
            {t("instance.runtime.preparation")}
          </Button>
        )}
        {hasRuntime && (
          <Button
            variant="tertiary"
            size="sm"
            leadingIcon={ConsoleIcon.settings}
            trailingIcon={ConsoleIcon.chevronDown}
            active={open === "runtime"}
            aria-expanded={open === "runtime"}
            aria-controls={`${id}-runtime`}
            onClick={() => setOpen(open === "runtime" ? null : "runtime")}
          >
            {t("instance.runtime.title")}
          </Button>
        )}
        <Button
          variant="tertiary"
          size="sm"
          leadingIcon={ConsoleIcon.monitor}
          trailingIcon={ConsoleIcon.chevronDown}
          active={open === "facts"}
          aria-expanded={open === "facts"}
          aria-controls={`${id}-facts`}
          onClick={() => setOpen(open === "facts" ? null : "facts")}
        >
          {t("instance.launchFacts.title")}
        </Button>
      </div>
      {preparation && (
        <div id={`${id}-preparation`} className="instance-launch-detail-body" hidden={open !== "preparation"}>
          {open === "preparation" && (
            <InstanceLaunchValue copy={{ label: t("instance.copyPreparation") }}>
              {preparation}
            </InstanceLaunchValue>
          )}
        </div>
      )}
      {hasRuntime && (
        <div id={`${id}-runtime`} className="instance-launch-detail-body" hidden={open !== "runtime"}>
          {open === "runtime" && (
            <>
              <DeploymentInfoList>
                {environment.length > 0 && (
                  <DeploymentInfoField label={t("instance.runtime.environment")}>
                    <div className="instance-launch-env">
                      {environment.map(([key, value]) => <InstanceLaunchValue key={key}>{`${key}=${value}`}</InstanceLaunchValue>)}
                    </div>
                  </DeploymentInfoField>
                )}
                {!!inst.mounts?.length && (
                  <DeploymentInfoField label={t("instance.runtime.mounts")}>
                    <div>{inst.mounts.map((mount) => <div key={mount.target}><InstanceLaunchValue>{mount.target}</InstanceLaunchValue>{mount.read_only && ` (${t("instance.runtime.readOnly")})`}</div>)}</div>
                  </DeploymentInfoField>
                )}
                {(!!inst.shm_bytes || hostIPC) && (
                  <DeploymentInfoField label={t("instance.runtime.shm")}>
                    {inst.shm_bytes ? t("instance.runtime.shmValue", { size: +(inst.shm_bytes / (1 << 30)).toFixed(2) }) : t("instance.runtime.shmHost")}
                  </DeploymentInfoField>
                )}
                {!!inst.docker_args?.length && (
                  <DeploymentInfoField label={t("instance.runtime.dockerArgs")}><InstanceLaunchValue>{inst.docker_args.join(" ")}</InstanceLaunchValue></DeploymentInfoField>
                )}
              </DeploymentInfoList>
              {(inst.vram_limit_bytes ?? 0) > 0 && (
                <span className="text-fg-subtle">{t("instance.runtime.dgpu")}</span>
              )}
            </>
          )}
        </div>
      )}
      <div id={`${id}-facts`} className="instance-launch-detail-body" hidden={open !== "facts"}>
        <InstanceLaunchFacts uuid={inst.uuid} expanded={open === "facts"} />
      </div>
    </div>
  );
}
