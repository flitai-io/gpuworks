import type { CSSProperties } from "react";
import { VENDOR_ASSETS } from "./vendorAssets";
import { useTranslation } from "react-i18next";
import { vendorForFramework, type Vendor } from "../vendors";

const META: Record<Vendor, { label?: string; labelKey?: string; color: string; mark: string }> = {
  nvidia: { label: "NVIDIA", color: "#76b900", mark: "N" },
  amd: { label: "AMD", color: "#ed1c24", mark: "A" },
  ascend: { labelKey: "vendor.ascend", color: "#c8102e", mark: "A" },
  ppu: { labelKey: "vendor.ppu", color: "#1677ff", mark: "P" },
  hygon: { labelKey: "vendor.hygon", color: "#6b4fbb", mark: "H" },
  kunlunxin: { labelKey: "vendor.kunlunxin", color: "var(--brand)", mark: "K" },
  apple: { labelKey: "vendor.apple", color: "#555555", mark: "M" },
  unknown: { labelKey: "vendor.unknown", color: "#8c8c8c", mark: "?" },
};
export { vendorForFramework };
export default function VendorLogo({
  vendor: vendorName,
  framework,
  size = 22,
  style,
}: {
  vendor?: string;
  framework?: string;
  size?: number;
  style?: CSSProperties;
}) {
  const { t } = useTranslation("common");
  const vendor = vendorForFramework(vendorName ?? framework);
  const meta = META[vendor];
  const label = meta.label || t(meta.labelKey!);
  const asset = VENDOR_ASSETS[vendor === "ppu" ? "thead" : vendor];
  return (
    <span
      title={`${label} · ${(vendorName || framework || t("vendor.unknownFramework")).toUpperCase()}`}
      style={{ display: "inline-flex", alignItems: "center", gap: 6, ...style }}
    >
      {asset ? (
        <img
          src={asset}
          alt={label}
          style={{
            width: size * 2.5,
            maxWidth: 96,
            height: size,
            objectFit: "contain",
          }}
        />
      ) : (
        <svg
          width={size}
          height={size}
          viewBox="0 0 24 24"
          aria-label={label}
          role="img"
        >
          <circle cx="12" cy="12" r="11" fill={meta.color} opacity="0.14" />
          <path
            d="M12 2.5a9.5 9.5 0 1 0 0 19 9.5 9.5 0 0 0-0-19Z"
            fill="none"
            stroke={meta.color}
            strokeWidth="1.5"
          />
          <text
            x="12"
            y="16"
            textAnchor="middle"
            fontSize="12"
            fontWeight="700"
            fill={meta.color}
          >
            {meta.mark}
          </text>
        </svg>
      )}
    </span>
  );
}
