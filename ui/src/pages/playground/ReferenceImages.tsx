import { useTranslation } from "react-i18next";
import { Badge } from "../../components/ui/badge";
import { Button } from "../../components/ui/button";

export interface ReferenceImage {
  name: string;
  dataURL: string;
}

// ReferenceImages picks the images an edit sends, in the order the request
// names them (Picture 1, Picture 2, ...). limit is the count the server
// resolved for the operation; one keeps the single-file picker. Problems
// (an unreadable file, too many files) go to onWarning for the page to show.
export default function ReferenceImages({ limit, images, onChange, onWarning }: {
  limit: number;
  images: ReferenceImage[];
  onChange: (images: ReferenceImage[]) => void;
  onWarning: (text: string) => void;
}) {
  const { t } = useTranslation(["playground", "common"]);
  const multiple = limit > 1;
  const choose = async (files: FileList | null) => {
    const picked = Array.from(files || []);
    if (!picked.length) {
      if (!multiple) onChange([]);
      return;
    }
    const read = await Promise.all(picked.map((file) => readImage(file).catch(() => {
      onWarning(t("chat.fileReadFailed", { name: file.name }));
      return undefined;
    })));
    const added = read.filter((item): item is ReferenceImage => Boolean(item));
    if (!multiple) {
      onChange(added.slice(0, 1));
      return;
    }
    const next = [...images, ...added];
    if (next.length > limit) onWarning(t("image.tooManyReferences", { max: limit }));
    onChange(next.slice(0, limit));
  };
  const moveEarlier = (index: number) => {
    const next = [...images];
    [next[index - 1], next[index]] = [next[index], next[index - 1]];
    onChange(next);
  };
  const remove = (index: number) => onChange(images.filter((_, position) => position !== index));
  const full = multiple && images.length >= limit;
  return <>
    <h5 className="playground-reference-title">{t("image.inputImage")}</h5>
    {multiple && <p className="playground-reference-hint">{t("image.referenceHint", { max: limit })}</p>}
    <input
      aria-label={t("image.inputImage")}
      type="file"
      accept="image/*"
      multiple={multiple}
      disabled={full}
      onChange={(event) => { void choose(event.target.files); event.target.value = ""; }}
    />
    {!multiple && images[0] && <img className="playground-generated-image" src={images[0].dataURL} alt={t("image.inputImage")} />}
    {multiple && images.length > 0 && <ol className="playground-reference-images" aria-label={t("image.referenceList")}>
      {images.map((image, index) => {
        const label = t("image.pictureLabel", { n: index + 1 });
        return <li key={`${index}-${image.name}`}>
          <img src={image.dataURL} alt={label} />
          <Badge size="sm">{label}</Badge>
          <span className="playground-reference-name" title={image.name}>{image.name}</span>
          <Button variant="secondary" size="xs" disabled={index === 0} aria-label={t("image.moveEarlier", { label })} onClick={() => moveEarlier(index)}>{t("image.moveEarlierShort")}</Button>
          <Button variant="secondary" size="xs" aria-label={t("image.removeReference", { label })} onClick={() => remove(index)}>{t("image.removeShort")}</Button>
        </li>;
      })}
    </ol>}
  </>;
}

function readImage(file: File): Promise<ReferenceImage> {
  return new Promise((resolve, reject) => {
    const reader = new FileReader();
    reader.onload = () => resolve({ name: file.name, dataURL: String(reader.result || "") });
    reader.onerror = () => reject(reader.error);
    reader.readAsDataURL(file);
  });
}
