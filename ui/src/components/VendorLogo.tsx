import type { CSSProperties } from "react";
import { VENDOR_ASSETS, VENDOR_LOGO_INK } from "./vendorAssets";
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

// The key a vendor's mark is filed under: T-Head PPU cards use T-Head's mark.
export function vendorMarkKey(vendor: string) {
  return vendor === "ppu" ? "thead" : vendor;
}

export function hasVendorMark(vendor: string) {
  return Boolean(VENDOR_ASSETS[vendorMarkKey(vendor)]);
}

// The shared wrapper selects the theme treatment from the artwork's ink tone.
// Callers size the image. A wrapper that adds a plate exposes its extra
// padding as --vendor-mark-pad-x/-y for callers with a fixed box.
export function VendorMarkImage({ vendor, alt, style }: { vendor: string; alt: string; style?: CSSProperties }) {
  const key = vendorMarkKey(vendor);
  const src = VENDOR_ASSETS[key];
  if (!src) return null;
  return (
    <span className={`vendor-mark vendor-mark-ink-${VENDOR_LOGO_INK[key] ?? "dark"}`}>
      <img src={src} alt={alt} style={style} />
    </span>
  );
}

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
  return (
    <span
      className={`vendor-logo vendor-logo-${vendor}`}
      title={`${label} · ${(vendorName || framework || t("vendor.unknownFramework")).toUpperCase()}`}
      style={{ display: "inline-flex", alignItems: "center", gap: 6, ...style }}
    >
      {hasVendorMark(vendor) ? (
        <VendorMarkImage
          vendor={vendor}
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
