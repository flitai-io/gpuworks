import type { ReactNode } from "react";
import { Empty, EmptyActions, EmptyDescription, EmptyHeader, EmptyTitle } from "../components/ui/empty";
export function Result({ title, description, actions }: { title: ReactNode; description?: ReactNode; actions?: ReactNode }) {
  return <Empty reason="informational" scope="section"><EmptyHeader><EmptyTitle>{title}</EmptyTitle>
    {description && <EmptyDescription>{description}</EmptyDescription>}</EmptyHeader>
    {actions && <EmptyActions>{actions}</EmptyActions>}
  </Empty>;
}
