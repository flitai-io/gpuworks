import type { EngineProfile, EvaluateResponse } from "../../api";

// Evaluation returns a display projection, not a new user override. Keep its
// provenance through the feedback loop so an earlier default cannot defeat a
// newly selected template (ADR 0020). Creation still confirms the complete
// evaluated form; this filter only prepares another evaluation.
export class EvaluateQuickParams {
  private displayed: Record<string, unknown> = {};
  // The engine whose dictionary produced `displayed`.
  private displayedEngine = "";
  private authored = new Set<string>();
  // Keys the deployer has edited at any point. Unlike `authored`, which
  // follows what the next request states, this is never pruned: placement
  // and argument fields (tp, pp, extra_args) never travel in quick_params,
  // yet a value typed there is still the deployer's own. Never pruning is
  // deliberate: authored values remain theirs across engines, except a
  // numeric automatic sentinel the target dictionary cannot express.
  private touched = new Set<string>();
  // The dictionary parameters, the only values whose meaning comes from one
  // engine's dictionary: learned from each answer's effective_params (the
  // launch configuration form_values is built from, before the argument text
  // and placement are added) and from the keys a request states.
  private paramKeys = new Set<string>();

  noteChanged(changed: Record<string, unknown>) {
    for (const key of Object.keys(changed)) {
      this.authored.add(key);
      this.touched.add(key);
    }
  }

  prepare(payload: Record<string, unknown>): Record<string, unknown> {
    const quick = (payload.quick_params ?? {}) as Record<string, unknown>;
    // Cleared controls and disabled tiers state no value. Their explicit
    // tombstones already travel in quick_params_unset, which is left intact.
    for (const key of this.authored) {
      if (!Object.prototype.hasOwnProperty.call(quick, key)) this.authored.delete(key);
    }
    const explicit: Record<string, unknown> = {};
    for (const [key, value] of Object.entries(quick)) {
      this.paramKeys.add(key);
      // Programmatic controls also edit the form, without onValuesChange.
      if (
        !Object.prototype.hasOwnProperty.call(this.displayed, key) ||
        JSON.stringify(value) !== JSON.stringify(this.displayed[key])
      ) {
        this.authored.add(key);
        this.touched.add(key);
      }
      if (this.authored.has(key)) explicit[key] = value;
    }
    const request = { ...payload };
    if (Object.keys(explicit).length) request.quick_params = explicit;
    else delete request.quick_params;
    return request;
  }

  accept(response: EvaluateResponse, engine: string) {
    if (!response.form_values) return;
    // Known from the answer itself, so an engine switch made before the next
    // request still recognizes the displayed parameters.
    for (const key of Object.keys(response.effective_params ?? {})) this.paramKeys.add(key);
    // Do this before updating the form, and only after the request's stale
    // guards pass. Its programmatic update must not look like a user edit.
    this.displayed = structuredClone(response.form_values);
    this.displayedEngine = engine;
    // Once an explicit extra_args flag moves into its field, the field owns
    // that choice. Editing the remaining argument text must not erase it.
    for (const moved of response.moved ?? []) {
      if (moved.source === "extra_args") {
        this.authored.add(moved.key);
        this.touched.add(moved.key);
      }
    }
  }

  // A displayed value is that engine's dictionary default or projection, not
  // the deployer's choice. Once the draft names another engine it describes
  // nothing: vLLM's automatic context (-1) is no value at all to SGLang. The
  // keys returned still hold such a value on the form and nobody stated them;
  // the caller clears them so the next answer derives them for the new
  // engine, or leaves them unstated where a manual command owns the launch.
  // Authored values remain for server validation, except negative automatic
  // sentinels: automatic intent carries across engines, its numeric spelling
  // does not. Read both dictionaries rather than maintaining an engine list.
  // Taking the stale keys also forgets the display, so a switch is answered once.
  takeStaleFor(
    engine: string,
    draft: Record<string, unknown>,
    profiles?: EngineProfile[],
  ): string[] {
    if (!engine || !this.displayedEngine || engine === this.displayedEngine) return [];
    const source = profiles?.find((profile) => profile.name === this.displayedEngine);
    const target = profiles?.find((profile) => profile.name === engine);
    // A dictionary still loading establishes no compatibility claim. Keep the
    // old display until both profiles arrive; the hook reevaluates then.
    if (profiles && (!source || !target)) return [];
    const automatic = (source?.param_specs ?? [])
      .filter((spec) => {
        const next = target?.param_specs?.find((item) => item.key === spec.key);
        return spec.automatic && typeof spec.default === "number" && spec.default < 0 &&
          draft[spec.key] === spec.default && next &&
          !(next.automatic && next.default === spec.default);
      })
      .map((spec) => spec.key);
    for (const key of automatic) {
      this.authored.delete(key);
      this.touched.delete(key);
    }
    const stale = Object.entries(this.displayed)
      .filter(([key, value]) =>
        this.paramKeys.has(key) &&
        !this.authored.has(key) &&
        !this.touched.has(key) &&
        draft[key] != null &&
        draft[key] !== "" &&
        JSON.stringify(draft[key]) === JSON.stringify(value))
      .map(([key]) => key);
    this.displayed = {};
    this.displayedEngine = "";
    return [...new Set([...stale, ...automatic])];
  }
}
