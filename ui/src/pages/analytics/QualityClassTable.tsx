import { useTranslation } from "react-i18next";

import type { QualityClassRow } from "../../analyticsApi";
import { Container, ContainerBody, ContainerHeader } from "../../components/ui/container";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "../../components/ui/table";
import { HelpTip } from "../../ui-kit/HelpTip";
import { fmtInt, fmtPct, fmtSeconds } from "./format";
import { successRate } from "./QualityTab";

// QualityClassTable splits the service quality by the priority class the
// gateway ran the requests at, so a low class starved behind a high one shows
// even when the totals look fine. The class "" holds requests recorded before
// classes existed: their class is unknown, not none.
export default function QualityClassTable({ rows }: { rows: QualityClassRow[] }) {
  const { t } = useTranslation("analytics");
  if (rows.length === 0) return null;
  return (
    <Container className="mb-4" data-testid="quality-by-class">
      <ContainerHeader>
        <span className="inline-flex items-center gap-1 font-semibold">
          {t("quality.byClass.title")}
          <HelpTip content={t("quality.byClass.help")} />
        </span>
      </ContainerHeader>
      <ContainerBody>
        <Table aria-label={t("quality.byClass.title")}>
          <TableHeader>
            <TableRow>
              <TableHead>{t("quality.byClass.class")}</TableHead>
              <TableHead className="text-right">{t("quality.byClass.requests")}</TableHead>
              <TableHead className="text-right">{t("quality.byClass.success")}</TableHead>
              <TableHead className="text-right">{t("quality.byClass.ttftP50")}</TableHead>
              <TableHead className="text-right">{t("quality.byClass.ttftP95")}</TableHead>
              <TableHead className="text-right">{t("quality.byClass.e2eP50")}</TableHead>
              <TableHead className="text-right">{t("quality.byClass.e2eP95")}</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {rows.map((r) => (
              <TableRow key={r.priority_class || "__unknown__"}>
                <TableCell>{r.priority_class || t("quality.byClass.unknown")}</TableCell>
                <TableCell className="text-right">{fmtInt(r.requests)}</TableCell>
                <TableCell className="text-right">{fmtPct(successRate(r))}</TableCell>
                <TableCell className="text-right">{fmtSeconds(r.ttft_p50_ms)}</TableCell>
                <TableCell className="text-right">{fmtSeconds(r.ttft_p95_ms)}</TableCell>
                <TableCell className="text-right">{fmtSeconds(r.e2e_p50_ms)}</TableCell>
                <TableCell className="text-right">{fmtSeconds(r.e2e_p95_ms)}</TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      </ContainerBody>
    </Container>
  );
}
