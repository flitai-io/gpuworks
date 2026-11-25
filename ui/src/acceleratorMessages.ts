/** What a vendor requires of a node's host, as one phrase: the operating
 * system and architecture when it restricts them, then the runtimes it runs
 * on. A vendor that restricts neither is described by its runtimes alone. */
export function hostRequirement(params?: Record<string, unknown>): string {
  const stated = (value: unknown): value is string => typeof value === "string" && value !== "";
  const host = [params?.required_os, params?.required_arch].filter(stated).join("/");
  return [host, params?.required_runtime].filter(stated).join(" · ");
}

/** Localize accelerator facts only at the display boundary. Both API errors
 * and scheduler refusals use this vocabulary; unknown future features retain
 * their identifier so an older client still gives a useful diagnostic. */
export function acceleratorMessageParams(
  t: (key: string, options: Record<string, unknown>) => string,
  params?: Record<string, unknown>,
): Record<string, unknown> {
  const feature = String(params?.feature ?? "");
  return { ...params, feature: t(`common:acceleratorFeature.names.${feature}`, { defaultValue: feature }) };
}
