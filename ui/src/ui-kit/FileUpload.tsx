import { forwardRef, useImperativeHandle, useRef, type InputHTMLAttributes, type ReactNode } from "react";
import { Button } from "../components/ui/button";
import { useTranslation } from "react-i18next";

/**
 * FileUpload picks one file. The native input stays for the form label, focus
 * and tests, but is visually hidden: its own button and "no file chosen" text
 * follow the browser's language, not the console's, and kept saying nothing
 * was chosen after a pick. A console button opens it; the chosen name sits beside.
 */
export const FileUpload = forwardRef<HTMLInputElement, Omit<InputHTMLAttributes<HTMLInputElement>, "type" | "value" | "onChange" | "size"> & {
  value: File | null; onChange: (file: File | null) => void;
  /** Shown after the chosen file's name, e.g. its size. */
  meta?: ReactNode;
}>(function FileUpload({ value, onChange, meta, disabled, className, ...props }, ref) {
  const { t } = useTranslation("common");
  const input = useRef<HTMLInputElement>(null);
  const button = useRef<HTMLButtonElement>(null);
  useImperativeHandle(ref, () => input.current as HTMLInputElement);
  return <div className={`flex min-w-0 flex-wrap items-center gap-2${className ? ` ${className}` : ""}`}>
    <input {...props} ref={input} type="file" disabled={disabled} className="sr-only" tabIndex={-1}
      // Form validation focuses the field; the visible button takes it.
      onFocus={() => button.current?.focus()}
      onChange={(event) => {
        const file = event.target.files?.[0];
        if (file) onChange(file);
        // Reset the native picker so selecting the same file fires again.
        event.target.value = "";
      }} />
    <Button ref={button} variant="secondary" size="md" disabled={disabled} aria-invalid={props["aria-invalid"]} onClick={() => input.current?.click()}>
      {t(value ? "upload.replace" : "upload.choose")}
    </Button>
    {value
      ? <><span className="min-w-0 break-all text-fg-default">{value.name}{meta != null && <span className="ml-1.5 text-fg-subtle">{meta}</span>}</span>
        <Button variant="ghost" size="sm" disabled={disabled} onClick={() => onChange(null)}>{t("actions.remove")}</Button></>
      : <span className="text-fg-subtle">{t("upload.none")}</span>}
  </div>;
});
