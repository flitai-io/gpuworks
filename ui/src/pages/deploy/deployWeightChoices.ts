import type { AcquisitionCandidate, AcquisitionResult } from "../../api";

// The two weight questions of the deploy form. Every value originates in the
// evaluate response; the client only decides whether a question has a real
// choice behind it and how to hold the answer.
//
// Where a node gets the files from (a node of its sync group, a shared
// storage, a download) is not asked: the server decides when the replica
// starts. What the request can say is which of the template's public weight
// sources to run (model_source.provider) and which storage a node saves the
// model into when it has to fetch it (model_source.storage_id).

/** The recommended candidate, else the first one. */
export function recommended(
  candidates?: AcquisitionCandidate[],
): AcquisitionCandidate | undefined {
  return (
    candidates?.find((candidate) => candidate.recommended) || candidates?.[0]
  );
}

/**
 * What the user reads for a candidate: the server-supplied display name when
 * the label is an identifier.
 */
export function candidateName(candidate: AcquisitionCandidate): string {
  return candidate.display_name?.trim() || candidate.label;
}

/**
 * The public weight source a model registered from a given source locks: such
 * a model's weights are that repository's, so the provider is not a choice.
 */
export function lockedProvider(
  acquisition: AcquisitionResult,
  modelSource?: string,
): AcquisitionCandidate | undefined {
  const wanted = modelSource?.trim().toLowerCase();
  if (!wanted) return undefined;
  return (acquisition.providers || []).find(
    (candidate) => candidate.label.trim().toLowerCase() === wanted,
  );
}

/** Whether the person is asked which public weight source to run. */
export function askProvider(
  acquisition: AcquisitionResult,
  modelSource?: string,
): boolean {
  return (
    (acquisition.providers || []).length >= 2 &&
    !lockedProvider(acquisition, modelSource)
  );
}

/** Whether the person is asked which storage a fetched model is saved into. */
export function askSaveTo(acquisition: AcquisitionResult): boolean {
  return (acquisition.targets || []).length >= 2;
}

/**
 * The provider label the form should hold: the locked one; otherwise the
 * current one while the latest evaluation still offers it, else the
 * recommended one; undefined when nothing is asked.
 */
export function resolveProvider(
  acquisition: AcquisitionResult,
  modelSource: string | undefined,
  current: unknown,
): string | undefined {
  const locked = lockedProvider(acquisition, modelSource);
  if (locked) return locked.label;
  if (!askProvider(acquisition, modelSource)) return undefined;
  const providers = acquisition.providers || [];
  const label = typeof current === "string" ? current : "";
  if (providers.some((candidate) => candidate.label === label)) return label;
  return recommended(providers)?.label;
}

function asStorageId(raw: unknown): number | undefined {
  const id = Number(raw);
  return raw !== undefined && raw !== null && raw !== "" && Number.isFinite(id) && id > 0
    ? id
    : undefined;
}

function storageDefault(targets: AcquisitionCandidate[]): number | undefined {
  const chosen =
    targets.find((candidate) => candidate.recommended) ||
    targets.find((candidate) => candidate.default) ||
    targets[0];
  return chosen?.storage_id;
}

/**
 * Keep a picked save target; otherwise keep an offered one or follow the
 * recommendation while a choice is asked. An automatic value is absent when
 * there is no question.
 *
 * A storage the person picked is never replaced: when the evaluation stops
 * offering it, it stays and weightStorageInvalid reports it, so the form asks
 * for a new choice instead of silently making one.
 */
export function resolveSaveTo(
  acquisition: AcquisitionResult,
  current: unknown,
  picked = false,
): number | undefined {
  const id = asStorageId(current);
  if (picked && id !== undefined) return id;
  if (!askSaveTo(acquisition)) return undefined;
  const targets = acquisition.targets || [];
  if (id !== undefined && targets.some((candidate) => candidate.storage_id === id))
    return id;
  return storageDefault(targets);
}

/**
 * Whether the storage the person picked is no longer offered. `selection` is
 * the form's model_source value.
 */
export function weightStorageInvalid(
  acquisition: AcquisitionResult,
  selection: unknown,
): boolean {
  const source = (selection && typeof selection === "object" ? selection : {}) as Record<string, unknown>;
  const id = asStorageId(source.storage_id);
  if (source.storage_picked !== true || id === undefined) return false;
  return !(acquisition.targets || []).some((candidate) => candidate.storage_id === id) &&
    !(acquisition.present || []).some((candidate) => candidate.storage_id === id && !candidate.unavailable);
}
