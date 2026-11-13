import { useTranslation } from "react-i18next";
import { Button } from "@/components/ui/button";
import { DropdownContent, DropdownMenu, DropdownTrigger } from "@/components/ui/dropdown";
import { MenuItem } from "@/components/ui/menu-item";
import { ConsoleIcon } from "../ui-kit/consoleIcons";

const GlobeIcon = ConsoleIcon.language;

const languages = [
  { key: "en", label: "English", compact: "EN" },
  { key: "zh", label: String.fromCodePoint(0x7b80, 0x4f53, 0x4e2d, 0x6587), compact: String.fromCodePoint(0x4e2d, 0x6587) },
] as const;

export default function LanguageSwitcher({ light = false }: { light?: boolean }) {
  const { t, i18n } = useTranslation();
  const current = i18n.resolvedLanguage === "zh" ? "zh" : "en";
  const index = languages.findIndex((language) => language.key === current);
  return (
    <DropdownMenu>
      <DropdownTrigger
        render={(
          <Button variant={light ? "ghost" : "secondary"} size="md" leadingIcon={GlobeIcon} aria-label={t("language.switch")} title={t("language.switch")}>
            {languages[index].compact}
          </Button>
        )}
      />
      <DropdownContent align="end" checkedIndex={index}>
        {languages.map((language, i) => (
          <MenuItem
            key={language.key}
            index={i}
            label={language.label}
            checked={language.key === current}
            onSelect={() => void i18n.changeLanguage(language.key)}
          />
        ))}
      </DropdownContent>
    </DropdownMenu>
  );
}
