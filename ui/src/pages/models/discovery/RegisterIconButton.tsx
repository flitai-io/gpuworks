import { useTranslation } from "react-i18next";
import { HugeiconsIcon } from "@hugeicons/react";
import Tick02Icon from "@hugeicons/core-free-icons/Tick02Icon";
import { Button } from "../../../components/ui/button";
import { ConsoleIcon } from "../../../ui-kit/consoleIcons";

export function RegisterIconButton({
  registered,
  onClick,
}: {
  registered: boolean;
  onClick: () => void;
}) {
  const {t} = useTranslation("models");
  const label = registered ? t("catalog.registered") : t("catalog.register");
  return (
    <Button
      type="button"
      variant="ghost"
      size="sm"
      iconOnly
      title={label}
      aria-label={label}
      disabled={registered}
      onClick={onClick}
    >
      {registered ? (
        <HugeiconsIcon icon={Tick02Icon} aria-hidden />
      ) : (
        <ConsoleIcon.add aria-hidden />
      )}
    </Button>
  );
}
