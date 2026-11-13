// The accelerator vendors the UI knows, in display order, and the compute
// framework each one runs. One place for this fact: the vendor logo, the
// template catalog and the template detail page all read it from here.

export const knownVendors = ["nvidia", "amd", "ascend", "hygon", "ppu", "apple", "kunlunxin"] as const;
export type KnownVendor = (typeof knownVendors)[number];
export type Vendor = KnownVendor | "unknown";

/** The framework a vendor's engines are built for. */
export const vendorFramework: Record<KnownVendor, string> = {
  nvidia: "cuda",
  amd: "rocm",
  ascend: "cann",
  hygon: "dtk",
  ppu: "hggc",
  apple: "metal",
  kunlunxin: "xre",
};

const frameworkVendors: Record<string, KnownVendor> = Object.fromEntries(
  knownVendors.map((vendor) => [vendorFramework[vendor], vendor]),
);

/** A vendor's framework; a name this module does not know is returned unchanged. */
export function frameworkForVendor(vendor: string): string {
  return (vendorFramework as Record<string, string>)[vendor] || vendor;
}

/** The vendor for a framework name or a vendor name, in any case; "unknown" otherwise. */
export function vendorForFramework(framework?: string): Vendor {
  const key = (framework || "").trim().toLowerCase();
  if ((knownVendors as readonly string[]).includes(key)) return key as KnownVendor;
  return frameworkVendors[key] || "unknown";
}

/** Sort comparator: known vendors in display order, then the rest by name. */
export function compareVendors(left: string, right: string): number {
  const a = (knownVendors as readonly string[]).indexOf(left);
  const b = (knownVendors as readonly string[]).indexOf(right);
  return (a < 0 ? knownVendors.length : a) - (b < 0 ? knownVendors.length : b) || left.localeCompare(right);
}
