import { useTranslation } from "react-i18next";
import { Button } from "./ui/button";
import { Popover, PopoverContent, PopoverTrigger } from "./ui/popover";

/**
 * ForgotPasswordHint tells how a password is reset when nobody can sign in:
 * an administrator runs reset-password on the server.
 */
export default function ForgotPasswordHint() {
  const { t } = useTranslation("auth");
  return (
    <div className="auth-forgot">
      <Popover>
        <PopoverTrigger render={<Button variant="link" size="sm">{t("forgotPassword")}</Button>} />
        <PopoverContent align="end" className="flex max-w-[480px] flex-col gap-2">
          <p className="text-body text-fg-default">{t("forgotPasswordHelp")}</p>
          <code className="w-fit whitespace-nowrap rounded-md bg-emphasis px-2 py-1 font-mono text-label text-fg-default">{t("forgotPasswordCommand")}</code>
        </PopoverContent>
      </Popover>
    </div>
  );
}
