import { Typography } from "antd";
import type { ReactNode } from "react";

// DetailSection is one titled block inside a detail tab. Tables inside it
// keep cells on one line and scroll sideways (detailDrawer.css).
export default function DetailSection({ title, extra, controls, children }: { title: string; extra?: ReactNode; controls?: ReactNode; children: ReactNode }) {
  const heading = (
      <Typography.Title level={5}>
        {title}
        {extra && <span className="detail-section-extra">{extra}</span>}
      </Typography.Title>
  );
  return (
    <section className="detail-section">
      {controls ? <header className="deployment-card-header">{heading}{controls}</header> : heading}
      {children}
    </section>
  );
}
