import { Fragment } from "react";
import { PageHeader as ZeronPageHeader, PageHeaderContent, PageTitle, PageDescription, PageActions } from "./ui/page-layout";
import { Breadcrumb, BreadcrumbList, BreadcrumbItem, BreadcrumbSeparator } from "./ui/breadcrumb";
import type { ReactNode } from "react";
import { useTranslation } from "react-i18next";
import { useLocation } from "react-router-dom";
import { sectionLabelKey } from "../navigation";

/**
 * PageHeader is the title block every page starts with. Pages reached from
 * the menu show no breadcrumb (the menu already says where you are); a page
 * below one of them passes `parents`, and the breadcrumb then names the menu
 * section plus those parents, so it can never disagree with the menu. The
 * description, the actions and a toolbar row are optional. Narrow, the parts
 * stack in reading order instead of drifting.
 */
export default function PageHeader({
  title,
  description,
  extra,
  toolbar,
  parents,
  className,
}: {
  title: ReactNode;
  /** One sentence on what the page is for; kept to a line. */
  description?: string;
  /** Actions beside the title, the primary one last. */
  extra?: ReactNode;
  /** Page-wide controls — filters, a time range, mode tabs — in a row under the title. */
  toolbar?: ReactNode;
  /** Pages between the menu section and this one, e.g. a link back to the list. */
  parents?: ReactNode[];
  className?: string;
}) {
  const { t } = useTranslation("common");
  const location = useLocation();
  // The menu entry decides. Comparing this title to the section text would
  // follow whichever translation happens to match.
  const sectionKey = sectionLabelKey(location.pathname);
  const section = sectionKey ? t(sectionKey) : undefined;
  const trail = parents?.length ? [section, ...parents].filter((item) => item !== undefined) : [];
  return (
    <div className={`page-header${className ? ` ${className}` : ""}`}>
      <ZeronPageHeader className="px-0">
        <PageHeaderContent className="page-header-copy">
          <div className="min-w-0">
            {trail.length > 0 && <Breadcrumb className="page-header-breadcrumb"><BreadcrumbList className="gap-1.5 text-label text-fg-subtle sm:gap-1.5">
              {trail.map((item, index) => <Fragment key={index}>
                {index > 0 && <BreadcrumbSeparator />}
                <BreadcrumbItem>{item}</BreadcrumbItem>
              </Fragment>)}
            </BreadcrumbList></Breadcrumb>}
            <PageTitle aria-level={2} className="page-header-title text-title font-semibold">{title}</PageTitle>
            {description && <PageDescription className="page-header-description" title={description}>{description}</PageDescription>}
          </div>
        </PageHeaderContent>
        {extra && <PageActions className="page-header-extra">{extra}</PageActions>}
      </ZeronPageHeader>
      {toolbar && <div className="page-header-toolbar">{toolbar}</div>}
    </div>
  );
}
