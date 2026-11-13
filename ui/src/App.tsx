import { FeedbackHost } from "./ui-kit/feedback";
import { Button } from "@/components/ui/button";
import { Tooltip } from "@/components/ui/tooltip";
import { SidebarProvider } from "@/components/ui/sidebar";
import AppSidebar from "./app/AppSidebar";
import { ConsoleIcon } from "./ui-kit/consoleIcons";

const AssistantIcon = ConsoleIcon.assistant;
import UpdateNotice from "./components/UpdateNotice";
import NoticeBell from "./components/NoticeBell";
import AssistantPanel from "./assistant/AssistantPanel";
import { AssistantProvider, useAssistant } from "./assistant/AssistantContext";
import { Navigate, Route, Routes, useLocation } from "react-router-dom";
import { useState } from "react";
import { useTranslation } from "react-i18next";
import NodesPage from "./pages/NodesPage";
import DashboardPage from "./pages/DashboardPage";
import MonitorPage from "./pages/monitor/MonitorPage";
import ServiceMonitorPage from "./pages/monitor/ServiceMonitorPage";
import AnalyticsPage from "./pages/analytics/AnalyticsPage";
import ModelsPage from "./pages/ModelsPage";
import InferenceTemplatesPage from "./pages/InferenceTemplatesPage";
import InferenceTemplateDetailPage from "./pages/InferenceTemplateDetailPage";
import DeployPage from "./pages/DeployPage";
import DeploymentsPage from "./pages/DeploymentsPage";
import SchedulingPage from "./pages/SchedulingPage";
import BatchesPage from "./pages/BatchesPage";
import EvaluationsPage from "./pages/EvaluationsPage";
import EvaluationDetailPage from "./pages/evaluations/EvaluationDetailPage";
import EvaluationComparePage from "./pages/evaluations/EvaluationComparePage";
import APIKeysPage from "./pages/APIKeysPage";
import CloudProvidersPage from "./pages/CloudProvidersPage";
import KubernetesPage from "./pages/KubernetesPage";
import SettingsPage from "./pages/SettingsPage";
import LogsPage from "./pages/LogsPage";
import SessionBoundary from "./components/SessionBoundary";
import UserMenu from "./components/UserMenu";
import NotFoundPage from "./pages/NotFoundPage";
import ImagesPage from "./pages/ImagesPage";
import StoragePage from "./pages/StoragePage";
import { StorageDetailRedirect } from "./pages/storage/storageLinks";
import NetworksPage from "./pages/networks/NetworksPage";
import { NetworkDetailRedirect } from "./pages/networks/networkLinks";
import GPUPage from "./pages/GPUPage";
import TerminalPage from "./pages/TerminalPage";
import AppErrorBoundary from "./components/AppErrorBoundary";
import PlaygroundPage from "./pages/playground/PlaygroundPage";
import { signOutPlayground } from "./pages/playground/conversations/useConversations";
import LanguageSwitcher from "./components/LanguageSwitcher";
import { BrandMark, useBranding, useDocumentBranding } from "./branding";
import "./pages/evaluations/agenticWorkload.css";
import "./pages/inferenceTemplateFlat.css";
import "./pages/networks/topology/networkTopology.css";
import "./pages/inferenceTemplateOrganizationCatalog.css";
import "./components/detail/detailDrawer.css";
import "./pages/nodes/nodeDetail.css";
import "./pages/dashboard.css";
import "./pages/scheduling/scheduling.css";
import "./pages/batches/batches.css";
import "./pages/scheduling/heatmap.css";
import "./pages/playground/playground.css";


export default function App() {
  return (
    <SessionBoundary onLogout={signOutPlayground}>
      {(logout) => <AppErrorBoundary>
        <FeedbackHost />
        <Routes>
          <Route path="/login" element={<Navigate to="/dashboard" replace />} />
          {/* The terminal uses the full viewport and shares the session gate. */}
          <Route path="/nodes/:uuid/terminal" element={<TerminalPage />} />
          <Route path="*" element={<AuthenticatedShell onLogout={logout} />} />
        </Routes>
      </AppErrorBoundary>}
    </SessionBoundary>
  );
}

/** Sends the old /ai-store/ai-model address to /models, keeping its query (?detail=). */
function ModelsAlias() {
  const { search } = useLocation();
  return <Navigate replace to={`/models${search}`} />;
}

function AuthenticatedShell({ onLogout }: { onLogout: () => Promise<void> }) {
  const branding = useBranding();
  useDocumentBranding(branding);
  const [navCollapsed, setNavCollapsed] = useState(readNavCollapsed);
  const setNavOpen = (open: boolean) => {
    setNavCollapsed(!open);
    writeNavCollapsed(!open);
  };

  return (
    <AssistantProvider>
    <div className="app-shell">
      <header className="app-header">
        <div className="app-header-start">
          <Brand title={branding.consoleTitle} />
        </div>
        <div className="app-header-actions">
          <UpdateNotice />
          <NoticeBell />
          <AssistantHeaderButton />
          <LanguageSwitcher light />
          <UserMenu onLogout={onLogout} />
        </div>
      </header>
      <SidebarProvider open={!navCollapsed} onOpenChange={setNavOpen} breakpointBehavior="collapse">
      <div className="app-body">
        <AppSidebar />
        <main className="app-content"><div className="app-page">
          <Routes>
            <Route path="/" element={<Navigate to="/dashboard" replace />} />
            <Route path="/login" element={<Navigate to="/dashboard" replace />} />
            <Route path="/dashboard" element={<DashboardPage />} />
            <Route path="/monitor" element={<MonitorPage />} />
            <Route path="/monitor/d/:id" element={<ServiceMonitorPage />} />
            <Route path="/analytics/:tab?" element={<AnalyticsPage />} />
            <Route path="/nodes" element={<NodesPage />} />
            <Route path="/gpus" element={<GPUPage />} />
            <Route path="/providers" element={<CloudProvidersPage />} />
            {/* The private cloud page became the external platforms list, opened on private clouds. */}
            <Route path="/zstack" element={<Navigate to="/providers?category=private_cloud" replace />} />
            <Route path="/kubernetes" element={<KubernetesPage />} />
            <Route path="/models" element={<ModelsPage />} />
			<Route path="/inference-templates" element={<InferenceTemplatesPage />} />
			<Route path="/inference-templates/*" element={<InferenceTemplateDetailPage />} />
            <Route path="/images" element={<ImagesPage />} />
            <Route path="/storage" element={<StoragePage />} />
            <Route path="/storage/:id" element={<StorageDetailRedirect />} />
            <Route path="/networks" element={<NetworksPage />} />
            <Route path="/networks/:id" element={<NetworkDetailRedirect />} />
            {/* The old models address: same page, so the menu and its breadcrumb follow /models. */}
            <Route path="/ai-store/ai-model" element={<ModelsAlias />} />
            <Route path="/deploy" element={<DeployPage />} />
            <Route path="/deployments" element={<DeploymentsPage />} />
            <Route path="/scheduling" element={<SchedulingPage />} />
            <Route path="/batches" element={<BatchesPage />} />
            <Route path="/playground/*" element={<PlaygroundPage />} />
            <Route path="/evaluations" element={<EvaluationsPage />} />
			<Route path="/evaluations/compare" element={<EvaluationComparePage />} />
			<Route path="/evaluations/:id" element={<EvaluationDetailPage />} />
            <Route path="/apikeys" element={<APIKeysPage />} />
            <Route path="/logs" element={<LogsPage />} />
            <Route path="/settings" element={<SettingsPage />} />
            <Route path="*" element={<NotFoundPage />} />
          </Routes>
        </div></main>
      </div>
      </SidebarProvider>
      <AssistantPanel />
    </div>
    </AssistantProvider>
  );
}

// AssistantHeaderButton opens the resident assistant from any page (also ⌘I / Ctrl+I).
function AssistantHeaderButton() {
  const { t } = useTranslation("ai");
  const { open, setOpen } = useAssistant();
  return (
    <Tooltip content={t("panel.headerTooltip")}>
      {/* The one header button with a label and a hairline border (ui-guidelines 9.1b). */}
      <Button variant="secondary" size="md" leadingIcon={AssistantIcon} onClick={() => setOpen(!open)} aria-label={t("title")} aria-pressed={open}>
        <span className="app-header-user">{t("title")}</span>
      </Button>
    </Tooltip>
  );
}

// Brand is the logo, followed by the top bar title when one is set.
function Brand({ title = "" }: { title?: string }) {
  const { productName, logo } = useBranding();
  return (
    <div className="app-brand">
      <BrandMark logo={logo} label={productName} />
      {title && <span className="app-brand-sep" aria-hidden="true" />}
      {title && <span className="app-brand-name">{title}</span>}
    </div>
  );
}

// The collapsed menu is a per-browser preference, like the language.
const navCollapsedKey = "gpuworks.navCollapsed";

function readNavCollapsed() {
  try {
    return localStorage.getItem(navCollapsedKey) === "1";
  } catch {
    return false;
  }
}

function writeNavCollapsed(collapsed: boolean) {
  try {
    localStorage.setItem(navCollapsedKey, collapsed ? "1" : "0");
  } catch {
    // Storage blocked: the choice lasts until reload.
  }
}
