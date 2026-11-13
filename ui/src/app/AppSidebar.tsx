import { ConsoleIcon } from "../ui-kit/consoleIcons";
import { useTranslation } from "react-i18next";
import { Link, useLocation } from "react-router-dom";
import { Button } from "@/components/ui/button";
import { NavMenu } from "@/components/ui/nav-menu";
import { NavItem, NavItemContent, NavItemLabel, NavItemLeading, NavItemTrigger } from "@/components/ui/nav-item";
import {
  Sidebar, SidebarContent, SidebarFooter, SidebarGroup, SidebarGroupContent, SidebarGroupLabel, useSidebar,
} from "@/components/ui/sidebar";
import { navGroups, selectedNavKey } from "../navigation";

const ToggleIcon = ConsoleIcon.sidebar;

/**
 * AppSidebar is the side menu under the full-width top bar, built from
 * navigation.tsx. Below Zeron's compact breakpoint (1280px) it folds to icons
 * on its own; above it, the user's collapse choice holds (SidebarProvider in
 * App.tsx keeps it in localStorage).
 */
export default function AppSidebar() {
  const { t } = useTranslation();
  const { pathname } = useLocation();
  const { state, toggle } = useSidebar();
  const selected = selectedNavKey(pathname);
  const collapsed = state === "collapsed";
  const toggleLabel = t(collapsed ? "header.expandNavigation" : "header.collapseNavigation");
  return (
    <Sidebar
      collapsible="icon"
      width="200px"
      ariaLabel={t("header.navigation")}
      // Same grey as the top bar and the frame, with no rule against the page:
      // the white page panel is the boundary (ui-guidelines 9.2).
      className="top-(--app-header-height) h-[calc(100svh-var(--app-header-height))] bg-surface-base [&_[data-slot=sidebar-panel]]:border-e-0"
    >
      {/* When the menu outgrows the window it scrolls on its own; the last 28px fade out to say there is more. */}
      <SidebarContent contentClassName="gap-3 px-2 pt-1 pb-6" viewportClassName="[mask-image:linear-gradient(#000_calc(100%-28px),transparent)]">
        {navGroups.map((group) => {
          const label = group.labelKey ? t(group.labelKey) : t("navigation.dashboard");
          return (
            <SidebarGroup key={group.labelKey ?? "overview"} aria-label={label}>
              {group.labelKey && <SidebarGroupLabel className="text-fg-subtle">{label}</SidebarGroupLabel>}
              <SidebarGroupContent>
                <NavMenu activeValue={selected} aria-label={label} keyboardNavigation="roving">
                  {group.items.map((item) => {
                    const name = t(item.labelKey);
                    return (
                      // Icon-collapsible Sidebars size items for the 40px rail; expanded, rows
                      // keep the console's 32px items 36px apart (ui-guidelines 9.2).
                      <NavItem key={item.key} value={item.key} className="group-data-[state=expanded]/sidebar:h-auto! group-data-[state=expanded]/sidebar:py-px!">
                        <NavItemTrigger render={<Link to={item.key} />} tooltip={collapsed ? name : undefined} tooltipSide="right" className="group-data-[state=expanded]/sidebar:h-control-md!">
                          {/* Decorative: antd icons name themselves ("heart"), which would leak into the link name. */}
                          <NavItemLeading aria-hidden="true">{item.icon}</NavItemLeading>
                          <NavItemContent><NavItemLabel>{name}</NavItemLabel></NavItemContent>
                        </NavItemTrigger>
                      </NavItem>
                    );
                  })}
                </NavMenu>
              </SidebarGroupContent>
            </SidebarGroup>
          );
        })}
      </SidebarContent>
      {/* The whole footer row toggles, not only the icon; its icon sits in the
          menu icons' column at their size (ui-guidelines 9.2). */}
      <SidebarFooter className="mx-2 border-t border-border px-0 pt-2 pb-(--app-frame-gap)">
        <Button
          variant="ghost"
          size="md"
          iconOnly={collapsed}
          leadingIcon={collapsed ? undefined : ToggleIcon}
          contentClassName="w-full justify-start gap-2 group-data-[state=collapsed]/sidebar:justify-center"
          className="w-full px-2.5 text-fg-muted [&_svg]:size-4! group-data-[state=collapsed]/sidebar:px-0"
          aria-label={toggleLabel}
          aria-expanded={!collapsed}
          onClick={toggle}
        >
          {collapsed ? <ToggleIcon /> : toggleLabel}
        </Button>
      </SidebarFooter>
    </Sidebar>
  );
}
