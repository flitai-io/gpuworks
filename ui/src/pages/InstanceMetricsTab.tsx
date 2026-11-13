import type { Instance } from "../api";
import InferenceMetricsPanel from "../components/InferenceMetricsPanel";
import KVTierPanel from "../components/KVTierPanel";
import InstanceGpuStrip from "../components/InstanceGpuStrip";

export default function InstanceMetricsTab({ inst, params }: { inst: Instance; params?: Record<string, unknown> }) {
  const live = inst.state === "running" || inst.state === "starting" || inst.state === "optimizing";
  const hasGpu = (live || (inst.vram_limit_bytes ?? 0) > 0) && !!inst.node_uuid && !!inst.gpu_indexes?.length;
  return <InferenceMetricsPanel instanceUuid={inst.uuid} historical={!live}
    cacheTiers={<KVTierPanel params={params} instanceUuid={inst.uuid} />}
    gpu={hasGpu ? selection => (
      <InstanceGpuStrip key={inst.uuid} selection={selection} historical={!live} nodeUuid={inst.node_uuid}
        gpuIndexes={inst.gpu_indexes} instanceUuid={inst.uuid} vramLimitBytes={inst.vram_limit_bytes} />
    ) : undefined}
  />;
}
