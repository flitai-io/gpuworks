import { useLayoutEffect, useRef, useState, type ReactNode } from "react";
import { Tooltip } from "../components/ui/tooltip";

type FieldProps = { label: string; hint?: ReactNode; children: ReactNode };

function TableLabel({ label, hint }: Pick<FieldProps, "label" | "hint">) {
  const ref = useRef<HTMLSpanElement>(null);
  const [clipped, setClipped] = useState(false);
  useLayoutEffect(() => {
    const el = ref.current;
    if (!el) return;
    const measure = () => setClipped(el.scrollHeight > el.clientHeight + 1);
    measure();
    if (typeof ResizeObserver === "undefined") return;
    const observer = new ResizeObserver(measure);
    observer.observe(el);
    return () => observer.disconnect();
  }, [label]);
  return <span className="deployment-table-label">
    <Tooltip content={clipped ? label : undefined}>
      <span className="deployment-table-label-text" ref={ref} tabIndex={clipped ? 0 : undefined}>{label}</span>
    </Tooltip>
    {hint}
  </span>;
}

export function DeploymentInfoList({ children, className = "" }: { children: ReactNode; className?: string }) {
  return <div className={`deployment-field-table ${className}`}><table><tbody>{children}</tbody></table></div>;
}

export default function DeploymentInfoField({ label, hint, children }: FieldProps) {
  return <tr>
    <th scope="row"><TableLabel label={label} hint={hint} /></th>
    <td><div className="deployment-table-value">{children}</div></td>
  </tr>;
}
