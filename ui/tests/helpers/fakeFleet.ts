import type { List, ListQuery, Node, NodeFacets, NodeFilter, NodePage } from "../../src/api";

// A fixture node needs only the fields a test cares about.
export type FleetNode = Partial<Node> & { uuid: string };

const SYSTEM_PREFIX = "gpuworks.ai/";

// requirements reads label=key=value pairs the way the server does: values of
// one key are alternatives, different keys must all hold.
function requirements(pairs: string[] = []): Map<string, Set<string>> {
  const out = new Map<string, Set<string>>();
  for (const pair of pairs) {
    const at = pair.indexOf("=");
    const key = pair.slice(0, at);
    out.set(key, (out.get(key) ?? new Set()).add(pair.slice(at + 1)));
  }
  return out;
}

// satisfies is planning.MatchRequirements over a fixture: a gpuworks.ai/
// key is matched against the node's system labels, any other against its
// written labels (a missing key never matches, an empty value is a value).
function satisfies(node: FleetNode, reqs: Map<string, Set<string>>): boolean {
  for (const [key, values] of reqs) {
    if (key.startsWith(SYSTEM_PREFIX)) {
      if (!(node.system_labels ?? []).some((label) => label.key === key && values.has(label.value))) return false;
    } else {
      const labels = node.labels ?? {};
      if (!Object.prototype.hasOwnProperty.call(labels, key) || !values.has(labels[key])) return false;
    }
  }
  return true;
}

function matching(fleet: FleetNode[], filter: NodeFilter): FleetNode[] {
  const q = filter.q?.trim().toLowerCase() ?? "";
  const reqs = requirements(filter.label);
  return fleet.filter((node) =>
    (!filter.state?.length || filter.state.includes(node.state ?? "")) &&
    (!q || [node.name ?? "", node.hostname ?? "", node.uuid].some((name) => name.toLowerCase().includes(q))) &&
    satisfies(node, reqs));
}

/**
 * fakeFleet answers the node endpoints as the server does - GET /nodes with
 * page parameters (one filtered page), GET /nodes/{uuid}, GET /nodes/facets -
 * and refuses GET /nodes without them: the pages under test must not list
 * the fleet. Install it on the api object (or a stub helper) by name.
 */
export function fakeFleet(fleet: FleetNode[]) {
  const pages: ListQuery[] = [];
  const facetCalls: NodeFilter[] = [];
  const lookups: string[] = [];
  return {
    pages,
    facetCalls,
    lookups,
    nodes: async (): Promise<List<Node>> => {
      throw new Error("the page fetched every node");
    },
    nodesPage: async (query: ListQuery): Promise<NodePage> => {
      pages.push(query);
      const matched = matching(fleet, query);
      const start = (query.page - 1) * query.page_size;
      const facets = new Map<string, Set<string>>();
      for (const node of fleet) for (const [key, value] of Object.entries(node.labels ?? {})) facets.set(key, (facets.get(key) ?? new Set()).add(value));
      return {
        items: matched.slice(start, start + query.page_size) as Node[],
        page: query.page, page_size: query.page_size, total: matched.length, all: fleet.length,
        labels: [...facets].sort(([a], [b]) => a.localeCompare(b)).map(([key, values]) => ({ key, values: [...values].sort() })),
        // The fixture nodes were all added by hand: the one source the fleet has, when it has nodes.
        sources: fleet.length ? [{ value: "manual", nodes: fleet.length }] : [],
      };
    },
    node: async (uuid: string): Promise<Node> => {
      lookups.push(uuid);
      const node = fleet.find((item) => item.uuid === uuid);
      if (!node) throw new Error(`node ${uuid} not found`);
      return node as Node;
    },
    nodeFacets: async (filter: NodeFilter = {}): Promise<NodeFacets> => {
      facetCalls.push(filter);
      const matched = matching(fleet, filter);
      const written = new Map<string, Map<string, number>>();
      const system = new Map<string, Map<string, { value: string; display: string; display_code?: string; nodes: number }>>();
      const gpus = new Map<string, { vendor: string; name: string; cc: string; model: string; nodes: number; cards: number; memory_bytes: number }>();
      for (const node of matched) {
        for (const [key, value] of Object.entries(node.labels ?? {})) {
          const values = written.get(key) ?? new Map<string, number>();
          values.set(value, (values.get(value) ?? 0) + 1);
          written.set(key, values);
        }
        for (const label of node.system_labels ?? []) {
          if (label.key === `${SYSTEM_PREFIX}node`) continue;
          const values = system.get(label.key) ?? new Map();
          const entry = values.get(label.value) ?? { value: label.value, display: label.display, display_code: label.display_code, nodes: 0 };
          entry.nodes += 1;
          values.set(label.value, entry);
          system.set(label.key, values);
        }
        // As the server counts: a node once per chip, every card, the largest card's memory.
        const seen = new Set<string>();
        for (const gpu of node.gpus ?? []) {
          const key = `${gpu.vendor ?? ""}/${gpu.name}/${gpu.compute_capability}`;
          const entry = gpus.get(key) ?? { vendor: gpu.vendor ?? "", name: gpu.name, cc: gpu.compute_capability, model: gpu.name, nodes: 0, cards: 0, memory_bytes: 0 };
          entry.cards += 1;
          entry.memory_bytes = Math.max(entry.memory_bytes, gpu.memory_total_bytes ?? 0);
          if (!seen.has(key)) {
            seen.add(key);
            entry.nodes += 1;
          }
          gpus.set(key, entry);
        }
      }
      const sorted = <T,>(entries: Iterable<[string, T]>) => [...entries].sort(([a], [b]) => a.localeCompare(b));
      return {
        total: matched.length,
        labels: sorted(written).map(([key, values]) => ({ key, values: sorted(values).map(([value, nodes]) => ({ value, nodes })) })),
        system_labels: sorted(system).map(([key, values]) => ({ key, values: sorted(values).map(([, entry]) => entry) })),
        gpus: [...gpus.values()],
      };
    },
  };
}

/** installFleet puts a fake fleet's endpoints on an api object. */
export function installFleet(api: Record<string, unknown>, fleet: FleetNode[]) {
  const fake = fakeFleet(fleet);
  api.nodes = fake.nodes;
  api.nodesPage = fake.nodesPage;
  api.node = fake.node;
  api.nodeFacets = fake.nodeFacets;
  return fake;
}
