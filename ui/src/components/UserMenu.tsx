import { useState } from "react";
import { useTranslation } from "react-i18next";
import { Button } from "./ui/button";
import { DropdownContent, DropdownMenu, DropdownSeparator, DropdownTrigger } from "./ui/dropdown";
import { MenuItem } from "./ui/menu-item";
import { ConsoleIcon } from "../ui-kit/consoleIcons";
import { message } from "../ui-kit/feedback";
import ChangePasswordModal from "./ChangePasswordModal";
import { useSessionUser } from "./sessionUser";

const AccountIcon = ConsoleIcon.account;
const ChevronIcon = ConsoleIcon.chevronDown;

/** UserMenu is the header's account entry: who is signed in, and what they can do with their own account. */
export default function UserMenu({ onLogout }: { onLogout: () => void }) {
  const { t } = useTranslation();
  const { t: tAuth } = useTranslation("auth");
  const user = useSessionUser();
  const [changing, setChanging] = useState(false);

  const name = user?.username ?? t("header.administrator");
  return (
    <>
      <DropdownMenu>
        <DropdownTrigger
          render={(
            <Button variant="ghost" size="md" leadingIcon={AccountIcon} trailingIcon={ChevronIcon} aria-label={t("header.account", { name })}>
              <span className="app-header-user">{name}</span>
            </Button>
          )}
        />
        <DropdownContent align="end">
          <MenuItem index={0} label={t("header.changePassword")} onSelect={() => setChanging(true)} />
          <DropdownSeparator />
          <MenuItem index={1} label={t("header.logout")} onSelect={onLogout} />
        </DropdownContent>
      </DropdownMenu>
      <ChangePasswordModal
        open={changing}
        policy={user?.password_policy}
        onClose={() => setChanging(false)}
        onChanged={() => {
          setChanging(false);
          message.success(tAuth("changePassword.done"));
          onLogout();
        }}
      />
    </>
  );
}
