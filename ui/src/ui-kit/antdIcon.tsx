import type { ComponentType, CSSProperties } from "react";
import type { IconComponent } from "@/lib/icon-context";

type AntdIcon = ComponentType<{ className?: string; style?: CSSProperties; "aria-hidden"?: boolean }>;

/**
 * fromAntd lets an @ant-design/icons glyph fill Zeron's icon slots (Button
 * leadingIcon, MenuItem icon, …) at the size the slot asks for. It bridges
 * the migration: the console's icons move to Zeron's catalog in the last step.
 */
export function fromAntd(Icon: AntdIcon): IconComponent {
  function Adapted({ size, className }: { size?: number | string; className?: string }) {
    return <Icon aria-hidden className={className} style={{ fontSize: size, lineHeight: 0 }} />;
  }
  Adapted.displayName = `fromAntd(${Icon.displayName ?? Icon.name ?? "Icon"})`;
  return Adapted as unknown as IconComponent;
}
