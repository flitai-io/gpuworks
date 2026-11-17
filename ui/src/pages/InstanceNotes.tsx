import { useTranslation } from "react-i18next";
import type { ProductWarning } from "../api";
import { Alert, AlertTitle } from "../components/ui/alert";

/**
 * InstanceNotes shows the non-blocking facts the server derives about an
 * instance when it is read (instance.notes: a code and its params), such as
 * a card it shares with processes outside the platform, which can explain an
 * inference slower than expected. The text is the locale's for the code
 * (deployments:instanceNote.<code>); list params read as comma-separated.
 */
export default function InstanceNotes({ notes }: { notes?: ProductWarning[] }) {
  const { t } = useTranslation("deployments");
  if (!notes?.length) return null;
  return (
    <div className="flex flex-col gap-1">
      {notes.map((note, index) => {
        const params: Record<string, unknown> = {};
        for (const [key, value] of Object.entries(note.params ?? {})) {
          params[key] = Array.isArray(value) ? value.join(", ") : value;
        }
        return (
          <Alert key={`${note.code}:${index}`} status="info">
            <AlertTitle>{t(`instanceNote.${note.code}`, { ...params, defaultValue: note.code })}</AlertTitle>
          </Alert>
        );
      })}
    </div>
  );
}
