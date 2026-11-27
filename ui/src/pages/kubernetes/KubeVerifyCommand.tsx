import { useState } from "react";
import { useTranslation } from "react-i18next";
import { Accordion, AccordionContent, AccordionItem, AccordionTrigger } from "../../components/ui/accordion";
import { useKubeDefaults } from "../../kubeClusters";
import ApplyCommand from "./ApplyCommand";
import KubeCheckCodes from "./KubeCheckCodes";

/**
 * The check to run after `kubectl apply`: it says, node by node, whether the
 * agent pod runs or why not. Its codes are explained under it, folded.
 */
export default function KubeVerifyCommand({ command }: { command: string }) {
  const { t } = useTranslation("kubernetes");
  const [open, setOpen] = useState(false);
  const { defaults } = useKubeDefaults(open);
  return (
    <div className="flex min-w-0 flex-col gap-1">
      <ApplyCommand command={command} title={t("check.verifyTitle")} hint={t("check.verifyHint")} />
      <Accordion type="single" collapsible value={open ? "codes" : ""} onValueChange={(next: string) => setOpen(next === "codes")}>
        <AccordionItem value="codes">
          <AccordionTrigger>{t("check.codesTitle")}</AccordionTrigger>
          <AccordionContent>{defaults && <KubeCheckCodes codes={defaults.check_codes.verify} />}</AccordionContent>
        </AccordionItem>
      </Accordion>
    </div>
  );
}
