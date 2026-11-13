import type { List, ListQuery, Model, ModelPage, TemplateWeightModels, WeightSource } from "../../src/api";

const presence = { ready_nodes: 0, cached_nodes: 0, ready_storages: 0, in_flight: 0, in_flight_percent: 0, failed: 0 };

// A fixture model needs only the fields a test cares about.
export type FixtureModel = Partial<Model> & { id: number; presence?: Partial<typeof presence> };

/**
 * fakeModels answers the model endpoints as the server does: one page of
 * list rows (GET /models?page, with q a case-insensitive substring of the
 * name, source id or revision, source alternatives, and source_id matching
 * whole ids ignoring case), one model by id (an unknown id fails as a 404
 * does), and listing every model fails the test. `models` is the live
 * registry: a test may push to it, as a registration does.
 */
export function fakeModels(models: FixtureModel[]) {
  const pages: ListQuery[] = [];
  const reads: number[] = [];
  const fake = {
    models,
    pages,
    reads,
    /** How often every model was listed: a caller may swallow the failure. */
    listedAll: 0,
    models_all: async (): Promise<List<Model>> => {
      fake.listedAll++;
      throw new Error("the page fetched every model");
    },
    modelsPage: async (query: ListQuery): Promise<ModelPage> => {
      pages.push(query);
      const q = query.q?.trim().toLowerCase() ?? "";
      const sources = query.extra?.source ?? [];
      const ids = (query.extra?.source_id ?? []).map((id) => id.toLowerCase());
      const matched = models.filter((m) =>
        (!q || [m.name ?? "", m.source_id ?? "", m.revision ?? ""].some((name) => name.toLowerCase().includes(q))) &&
        (!sources.length || sources.includes(m.source ?? "")) &&
        (!ids.length || ids.includes((m.source_id ?? "").toLowerCase())),
      );
      const start = (query.page - 1) * query.page_size;
      return {
        items: matched.slice(start, start + query.page_size).map(({ nodes: _n, storages: _s, readme: _r, presence: own, ...row }) => ({ ...row, presence: { ...presence, ...own } })) as never,
        page: query.page,
        page_size: query.page_size,
        total: matched.length,
        summary: { registered: models.length, ready_files: 0, sources: 0, weight_gib: 0 },
      };
    },
    weightLookups: [] as Array<[number, string, string]>,
    /**
     * GET /inference-templates/weight-models as the server answers it, for
     * the spec `specs` resolves (an unknown spec fails as a 404 does): a model
     * serves a source when its source id is the source's repo id ignoring case
     * and it holds the requested subfolder or the whole repository; models come
     * newest first, each once; a source reuses the newest serving model from
     * its own provider.
     */
    templateWeightModels: (specs: (templateID: number, source: string, name: string) => WeightSource[] | undefined) =>
      async (templateID: number, specSource: string, specName: string): Promise<TemplateWeightModels> => {
        fake.weightLookups.push([templateID, specSource, specName]);
        const sources = specs(templateID, specSource, specName);
        if (!sources) throw new Error("inference spec not found");
        const newest = [...models].sort((a, b) => b.id - a.id);
        const serves = (m: FixtureModel, source: WeightSource) => {
          const held = (m.artifact_subfolder ?? "").trim();
          const asked = (source.subfolder ?? "").trim();
          return (source.provider.toLowerCase() !== "local" || m.source === "local") &&
            (m.source_id ?? "").trim().toLowerCase() === source.repo_id.trim().toLowerCase() &&
            (held === asked || (held === "" && asked !== ""));
        };
        const serving = newest.filter((m) => sources.some((source) => serves(m, source)));
        return {
          models: serving.map(({ nodes: _n, storages: _s, readme: _r, presence: own, ...row }) => ({ ...row, presence: { ...presence, ...own } })) as never,
          sources: sources.map((source) => {
            const reuse = serving.find((m) => (m.source ?? "").toLowerCase() === source.provider.toLowerCase() && serves(m, source));
            return reuse ? { ...source, reuse_model_id: reuse.id } : { ...source };
          }),
        };
      },
    model: async (id: number): Promise<Model> => {
      reads.push(id);
      const found = models.find((m) => m.id === id);
      if (!found) throw new Error("model not found");
      return found as Model;
    },
  };
  return fake;
}

/** installModels puts fakeModels on the api object for the test's duration. */
export function installModels(api: Record<string, unknown>, models: FixtureModel[]) {
  const fake = fakeModels(models);
  api.models = fake.models_all;
  api.modelsPage = fake.modelsPage;
  api.model = fake.model;
  return fake;
}
