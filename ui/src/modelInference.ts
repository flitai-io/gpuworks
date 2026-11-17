import type { InferenceAPIOperation, OperationType } from "./inferenceApi";

export const operationTypes: readonly OperationType[] = [
  "openai.chat",
  "openai.completions",
  "openai.embeddings",
  "openai.rerank",
  "openai.audio.transcribe",
  "openai.audio.speech",
  "openai.image.generate",
  "openai.image.edit",
  "openai.video.generate",
  "openai.realtime",
  "systemone.decide",
];

export const operationLabelKey: Record<OperationType, string> = {
  "openai.chat": "common:operation.chat",
  "openai.completions": "common:operation.completions",
  "openai.embeddings": "common:operation.embeddings",
  "openai.rerank": "common:operation.rerank",
  "openai.audio.transcribe": "common:operation.audioTranscribe",
  "openai.audio.speech": "common:operation.audioSpeech",
  "openai.image.generate": "common:operation.imageGenerate",
  "openai.image.edit": "common:operation.imageEdit",
  "openai.video.generate": "common:operation.videoGenerate",
  "openai.realtime": "common:operation.realtime",
  "systemone.decide": "common:operation.decide",
};
export const allOperationsLabelKey = "common:operation.all";

export function operationTypesOf(operations?: Pick<InferenceAPIOperation, "type">[]): OperationType[] {
  return [...new Set((operations ?? []).map((operation) => operation.type).filter((type): type is OperationType => operationTypes.includes(type)))];
}

export function hasOperation(operations: Pick<InferenceAPIOperation, "type">[] | undefined, type: OperationType): boolean {
  return operationTypesOf(operations).includes(type);
}

export const inputModalityTypes = ["text", "image", "audio", "video"] as const;
export type InputModality = typeof inputModalityTypes[number];
export const inputModalityLabelKey: Record<InputModality, string> = {
  text: "common:inputModality.text",
  image: "common:inputModality.image",
  audio: "common:inputModality.audio",
  video: "common:inputModality.video",
};

export function inputModalitiesOf(values?: string[]): InputModality[] {
  return [...new Set((values ?? []).map((value) => value.trim().toLowerCase()).filter((value): value is InputModality => inputModalityTypes.includes(value as InputModality)))];
}
