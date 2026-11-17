import { useEffect, useState } from "react";
import { api, type PriorityClasses } from "../../api";

/**
 * The request priority classes the playground offers, read once for the page.
 * null while they load; when they cannot be read the page offers only the
 * default (an empty list, failed set), so a reply can still be sent.
 */
export interface PlaygroundPriorityClasses {
  classes: PriorityClasses | null;
  failed: boolean;
}

const NONE: PriorityClasses = { items: [], min_value: 0, max_value: 0, policies: [], default_policy: "", ceiling_policies: [] };

export function usePlaygroundPriorityClasses(): PlaygroundPriorityClasses {
  const [state, setState] = useState<PlaygroundPriorityClasses>({ classes: null, failed: false });
  useEffect(() => {
    let live = true;
    api.priorityClasses().then(
      (classes) => { if (live) setState({ classes, failed: false }); },
      () => { if (live) setState({ classes: NONE, failed: true }); },
    );
    return () => { live = false; };
  }, []);
  return state;
}

/**
 * knownClass is the class a reply should ask for: the chosen one while the
 * classes say it exists, "" (the default) once they say it does not. A
 * conversation saved before its class was deleted then sends again.
 */
export function knownClass(chosen: string | undefined, loaded: PlaygroundPriorityClasses): string | undefined {
  if (!chosen || !loaded.classes || loaded.failed) return chosen;
  return loaded.classes.items.some((c) => c.name === chosen) ? chosen : "";
}
