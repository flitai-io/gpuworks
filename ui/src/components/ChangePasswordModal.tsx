import { useRef, useState } from "react";
import { useForm } from "react-hook-form";
import { z } from "zod";
import { zodResolver } from "@hookform/resolvers/zod";
import { useTranslation } from "react-i18next";
import { Button } from "./ui/button";
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from "./ui/dialog";
import { FormField } from "../ui-kit/form/FormField";
import { formSubmit } from "../ui-kit/form/submit";
import { Notice } from "../ui-kit/Notice";
import { PasswordInput } from "../ui-kit/PasswordInput";
import { api, type AuthUser } from "../api";
import { APIError, apiErrorText } from "../apiError";

type Values = { current: string; next: string; confirm: string };
const EMPTY: Values = { current: "", next: "", confirm: "" };

/** Errors that belong to one field; anything else is shown above the form. */
const FIELD_ERRORS: Record<string, keyof Values> = {
  CURRENT_PASSWORD_INVALID: "current",
  PASSWORD_LENGTH: "next",
  PASSWORD_CHARACTERS: "next",
  PASSWORD_GUESSABLE: "next",
  PASSWORD_USERNAME: "next",
  PASSWORD_UNCHANGED: "next",
};

/**
 * ChangePasswordModal replaces the signed-in user's password. The server
 * judges both passwords on each submission and ends every session of the user
 * on success, so onChanged signs this tab out.
 */
export default function ChangePasswordModal({ open, policy, onClose, onChanged }: {
  open: boolean;
  policy?: AuthUser["password_policy"];
  onClose: () => void;
  onChanged: () => void;
}) {
  const { t } = useTranslation("auth");
  const { t: tCommon } = useTranslation("common");
  // Only presence and the confirmation are checked here; the policy is the server's.
  const schema = z.object({
    current: z.string().min(1, t("changePassword.currentRequired")),
    next: z.string().min(1, t("changePassword.nextRequired")),
    confirm: z.string().min(1, t("changePassword.confirmRequired")),
  }).refine((values) => values.confirm === values.next, { path: ["confirm"], message: t("changePassword.mismatch") });
  const formRef = useRef<HTMLFormElement>(null);
  const form = useForm<Values>({ resolver: zodResolver(schema), shouldFocusError: false, defaultValues: EMPTY });
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState("");

  const close = () => {
    if (saving) return;
    form.reset(EMPTY);
    setError("");
    onClose();
  };

  async function submit(values: Values) {
    setSaving(true);
    setError("");
    try {
      await api.changePassword(values.current, values.next);
      form.reset(EMPTY);
      onChanged();
    } catch (err) {
      const field = err instanceof APIError ? FIELD_ERRORS[err.code] : undefined;
      if (!(err instanceof APIError) || !field) {
        setError(err instanceof APIError && err.code === "LOGIN_THROTTLED" ? t("changePassword.throttled") : apiErrorText(tCommon, err));
        return;
      }
      form.setError(field, { message: apiErrorText(tCommon, err) });
      // A wrong current password carries the new password's own rejection, if any.
      const alsoNew = err.params?.new_password;
      if (typeof alsoNew === "string") {
        form.setError("next", { message: apiErrorText(tCommon, new APIError(alsoNew, err.params, alsoNew, err.status)) });
      }
    } finally {
      setSaving(false);
    }
  }

  return (
    <Dialog open={open} onOpenChange={(next) => { if (!next) close(); }} disablePointerDismissal>
      <DialogContent closeButtonLabel={tCommon("actions.close")} closeButtonDisabled={saving} style={{ width: "min(440px, calc(100vw - 32px))", maxWidth: "calc(100vw - 32px)" }}>
        <DialogHeader>
          <DialogTitle>{t("changePassword.title")}</DialogTitle>
          <DialogDescription>{t("changePassword.description")}</DialogDescription>
        </DialogHeader>
        <form ref={formRef} noValidate onSubmit={formSubmit(form, () => formRef.current, submit)} className="flex flex-col gap-4">
          {error && <Notice tone="danger" title={error} />}
          <FormField control={form.control} name="current" label={t("changePassword.current")}>
            {(binding) => <PasswordInput {...binding} autoComplete="current-password" disabled={saving}
              onChange={(event) => { binding.onChange(event); setError(""); }} />}
          </FormField>
          <FormField control={form.control} name="next" label={t("changePassword.next")}
            description={policy ? t("changePassword.rule", { min: policy.min_length, max: policy.max_length }) : undefined}>
            {(binding) => <PasswordInput {...binding} autoComplete="new-password" disabled={saving} />}
          </FormField>
          <FormField control={form.control} name="confirm" label={t("changePassword.confirm")}>
            {(binding) => <PasswordInput {...binding} autoComplete="new-password" disabled={saving} />}
          </FormField>
          <DialogFooter>
            <Button variant="ghost" onClick={close} disabled={saving}>{tCommon("actions.cancel")}</Button>
            <Button type="submit" variant="primary" loading={saving}>{t("changePassword.submit")}</Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}
