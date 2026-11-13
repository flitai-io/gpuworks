import { ConsoleIcon } from "../../ui-kit/consoleIcons";

const PROVIDER_LOGOS: Record<string, string> = {
  aliyun: "/provider-logos/aliyun.webp",
  autodl: "/provider-logos/autodl.webp",
  compshare: "/provider-logos/compshare.webp",
  digitalocean: "/provider-logos/digitalocean.svg",
  runpod: "/provider-logos/runpod.webp",
  vast: "/provider-logos/vast.webp",
  zstack: "/provider-logos/zstack.svg",
};

// Vendor marks are UI assets under public/provider-logos; a kind without one
// shows its initial instead, and a cluster connected on its own the
// Kubernetes menu icon (its card is named by the platform type's id).
const KUBERNETES_LOGO = "kubernetes";
export default function ProviderKindLogo({ kind }: { kind: string }) {
  const logo = PROVIDER_LOGOS[kind];
  if (logo) {
    return <img className="provider-kind-logo" src={logo} alt="" width={32} height={32} />;
  }
  if (kind === KUBERNETES_LOGO) {
    return <span className="provider-kind-logo provider-kind-logo-fallback" aria-hidden="true"><ConsoleIcon.kubernetes size={20} /></span>;
  }
  return <span className="provider-kind-logo provider-kind-logo-fallback" aria-hidden="true">{kind.slice(0, 1).toUpperCase()}</span>;
}
