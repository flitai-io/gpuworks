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
