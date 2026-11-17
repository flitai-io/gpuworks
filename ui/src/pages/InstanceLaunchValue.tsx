import CopyButton from "../components/CopyButton";

export default function InstanceLaunchValue({ children, title, copy }: {
  children: string;
  title?: string;
  copy?: { label: string; text?: string };
}) {
  return (
    <span className="instance-launch-value">
      <code title={title}>{children}</code>
      {copy && <CopyButton size="xs" text={copy.text ?? children} label={copy.label} />}
    </span>
  );
}
