import type { AIImageLimit } from "../api";

/** IMAGE_TYPES are the formats the server takes in an ask. */
export const IMAGE_TYPES = ["image/png", "image/jpeg", "image/webp", "image/gif"];

/** ImagePrepError says why a file cannot be sent: not an image the server takes, or too large even after shrinking. */
export class ImagePrepError extends Error {
  constructor(readonly reason: "format" | "too_large", readonly name: string) {
    super(`${name}: ${reason}`);
  }
}

/** A decoded image the canvas can redraw. */
export interface Decoded {
  width: number;
  height: number;
  /** encode draws the image at width x height and answers it as a JPEG data: URL. */
  encode(width: number, height: number, quality: number): Promise<string>;
}

/** Decoder turns a file into something to redraw, or null where the browser cannot (no canvas). */
export type Decoder = (file: Blob) => Promise<Decoded | null>;

// Re-encoded screenshots stay legible at this quality; past this size an
// image within the side limit is re-encoded anyway, to keep the ask small.
const QUALITY = 0.85;
const REENCODE_ABOVE_BYTES = 1 << 20;

export function dataURLBytes(url: string): number {
  const payload = url.slice(url.indexOf(",") + 1);
  return Math.floor((payload.length * 3) / 4) - (payload.endsWith("==") ? 2 : payload.endsWith("=") ? 1 : 0);
}

function readDataURL(file: Blob): Promise<string> {
  return new Promise((resolve, reject) => {
    const reader = new FileReader();
    reader.onload = () => resolve(String(reader.result));
    reader.onerror = () => reject(reader.error ?? new Error("read failed"));
    reader.readAsDataURL(file);
  });
}

/** canvasDecoder decodes with createImageBitmap and redraws on a canvas; it answers null without them. */
export const canvasDecoder: Decoder = async (file) => {
  if (typeof createImageBitmap !== "function" || typeof document === "undefined") return null;
  const canvas = document.createElement("canvas");
  if (!canvas.getContext?.("2d")) return null;
  const bitmap = await createImageBitmap(file);
  return {
    width: bitmap.width,
    height: bitmap.height,
    async encode(width, height, quality) {
      canvas.width = width;
      canvas.height = height;
      const ctx = canvas.getContext("2d")!;
      // JPEG has no transparency: a screenshot's transparent parts turn white, not black.
      ctx.fillStyle = "#fff";
      ctx.fillRect(0, 0, width, height);
      ctx.drawImage(bitmap, 0, 0, width, height);
      return canvas.toDataURL("image/jpeg", quality);
    },
  };
};

/**
 * prepareImage turns a pasted, dropped or picked file into the data: URL an
 * ask sends: an image larger than the server's longest side, or heavier than
 * 1 MiB, is shrunk and re-encoded as JPEG; one that is still over the server's
 * byte limit is refused. Where the browser cannot redraw, the file is sent as
 * it is when it fits.
 */
export async function prepareImage(file: File, limit: AIImageLimit, decode: Decoder = canvasDecoder): Promise<string> {
  if (!IMAGE_TYPES.includes(file.type)) throw new ImagePrepError("format", file.name);
  const original = await readDataURL(file);
  const decoded = await decode(file).catch(() => null);
  let url = original;
  if (decoded) {
    const scale = Math.min(1, limit.max_side / Math.max(decoded.width, decoded.height));
    if (scale < 1 || file.size > REENCODE_ABOVE_BYTES) {
      url = await decoded.encode(Math.max(1, Math.round(decoded.width * scale)), Math.max(1, Math.round(decoded.height * scale)), QUALITY);
    }
  }
  if (dataURLBytes(url) > limit.max_bytes) throw new ImagePrepError("too_large", file.name);
  return url;
}

/**
 * keepNewestImages trims a thread's images to what one ask may carry: the
 * question's own first, then the newest earlier turns', dropping the oldest.
 * images lists each user turn's images, oldest turn first.
 */
export function keepNewestImages(images: string[][], question: number, perRequest: number): string[][] {
  let left = Math.max(0, perRequest - question);
  const out = images.map(() => [] as string[]);
  for (let i = images.length - 1; i >= 0; i--) {
    const take = Math.min(left, images[i].length);
    out[i] = images[i].slice(images[i].length - take);
    left -= take;
  }
  return out;
}
