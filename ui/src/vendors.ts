// The accelerator vendors, as the server's vendor dictionary lists them
// (GET /api/v1/accelerator-vendors): their order, names and the frameworks
// their engines are built for. The console keeps no copy of those. Before
// the dictionary arrives a framework names no vendor and vendors sort by
// name; a vendor's own id still names it. Components read the dictionary
// through useVendorDictionary, which loads it.

/** One vendor of the server's dictionary. */
export interface VendorEntry {
  id: string;
  brand: string;
  frameworks: string[];
  runtimes: string[];
  features: string[];
  partition_backend?: string;
}

/** A vendor id from the dictionary, or "unknown". */
export type Vendor = string;

let entries: readonly VendorEntry[] = [];
const listeners = new Set<() => void>();

export function vendorDictionary(): readonly VendorEntry[] {
  return entries;
}

export function setVendorDictionary(next: readonly VendorEntry[]): void {
  entries = next;
  for (const listener of listeners) listener();
}

export function subscribeVendorDictionary(listener: () => void): () => void {
  listeners.add(listener);
  return () => { listeners.delete(listener); };
}

/** The name the server gives a vendor; undefined for one it does not list. */
export function vendorBrand(vendor: string): string | undefined {
  return entries.find((entry) => entry.id === vendor)?.brand;
}

/** The vendor for a framework name or a vendor name, in any case; "unknown" otherwise. */
export function vendorForFramework(framework?: string): Vendor {
  const key = (framework || "").trim().toLowerCase();
  if (!key) return "unknown";
  const named = entries.find((entry) => entry.id === key) ?? entries.find((entry) => entry.frameworks.includes(key));
  return named?.id ?? "unknown";
}

/** The vendor a card, node or spec names by its own id. Without the
 * dictionary the id is taken as given, so cards of different vendors stay
 * apart; with it, an id the server does not list is "unknown". */
export function vendorNamed(vendor?: string): Vendor {
  const id = (vendor || "").trim().toLowerCase();
  if (!id) return "unknown";
  if (entries.length === 0) return id;
  return entries.some((entry) => entry.id === id) ? id : "unknown";
}

/** What to call a vendor the console has no translated name for: the
 * server's name for it, or its id in capitals. */
export function vendorFallbackName(vendor: string): string {
  return vendorBrand(vendor) ?? vendor.toUpperCase();
}

/** Whether a component is reading the dictionary right now. */
export function vendorDictionaryWatched(): boolean {
  return listeners.size > 0;
}

/** Sort the listed vendors first, then the rest by name. The order is the dictionary's unless one is given. */
export function compareVendors(left: string, right: string, order?: readonly string[]): number {
  const listed = order ?? entries.map((entry) => entry.id);
  const a = listed.indexOf(left);
  const b = listed.indexOf(right);
  return (a < 0 ? listed.length : a) - (b < 0 ? listed.length : b) || left.localeCompare(right);
}
