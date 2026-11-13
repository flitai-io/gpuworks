import { LoadingOutlined } from "@ant-design/icons";
export function Spinner({ label }: { label: string }) {
  return <span role="status" className="inline-flex items-center gap-2 text-fg-muted"><LoadingOutlined aria-hidden="true" className="motion-safe:animate-spin" /><span>{label}</span></span>;
}
