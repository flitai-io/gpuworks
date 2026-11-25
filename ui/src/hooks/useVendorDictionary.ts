import { useEffect, useSyncExternalStore } from "react";

import { setVendorDictionary, subscribeVendorDictionary, vendorDictionary, vendorDictionaryWatched, type VendorEntry } from "../vendors";

type FetchVendors = () => Promise<{ items: VendorEntry[] }>;

/** How long a failed load is left alone before it is tried again. */
export const vendorDictionaryRetry = { ms: 30_000 };

let loading: Promise<void> | null = null;
let failedAt = 0;
let retry: ReturnType<typeof setTimeout> | null = null;

// The HTTP client is loaded when the dictionary is first asked for: it pulls
// in the localization setup, which a module that only names vendors (the
// vendor logo) must not load with it.
async function fetchVendorDictionary(): Promise<{ items: VendorEntry[] }> {
  const { getJSON } = await import("../api/http");
  return getJSON<{ items: VendorEntry[] }>("/api/v1/accelerator-vendors");
}

// One retry is pending at a time, and it asks only while some component
// still reads the dictionary: a page left open through a failed load gets
// its vendors without being opened again, and rows that mount meanwhile do
// not each send a request.
function retryLater(fetchVendors: FetchVendors) {
  failedAt = Date.now();
  if (retry) return;
  retry = setTimeout(() => {
    retry = null;
    failedAt = 0;
    if (vendorDictionaryWatched()) void loadVendorDictionary(fetchVendors);
  }, vendorDictionaryRetry.ms);
  (retry as { unref?: () => void }).unref?.();
}

/** Loads the vendor dictionary once. A failed load leaves it empty and is
 * tried again after vendorDictionaryRetry.ms, not before; a framework names
 * no vendor meanwhile. */
export function loadVendorDictionary(fetchVendors: FetchVendors = fetchVendorDictionary): Promise<void> {
  if (vendorDictionary().length > 0) return Promise.resolve();
  if (failedAt && Date.now() - failedAt < vendorDictionaryRetry.ms) return Promise.resolve();
  loading ??= fetchVendors()
    .then((list) => { failedAt = 0; setVendorDictionary(list.items); })
    .catch(() => retryLater(fetchVendors))
    .finally(() => { loading = null; });
  return loading;
}

/** The vendor dictionary, loading it on first use. A component that orders
 * or names vendors calls this so it renders again when the dictionary arrives. */
export function useVendorDictionary(): readonly VendorEntry[] {
  const entries = useSyncExternalStore(subscribeVendorDictionary, vendorDictionary, vendorDictionary);
  useEffect(() => { void loadVendorDictionary(); }, []);
  return entries;
}
