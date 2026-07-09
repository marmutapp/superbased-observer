import { Route, Routes, useLocation } from "react-router-dom";
import { Layout } from "@/components/Layout";
import { RouteErrorBoundary } from "@/components/RouteErrorBoundary";
import { OverviewPage } from "@/pages/Overview";
import { TeamsPage } from "@/pages/Teams";
import { TeamDetailPage } from "@/pages/TeamDetail";
import { PeoplePage } from "@/pages/People";
import { ToolsPage } from "@/pages/Tools";
import { ModelsPage } from "@/pages/Models";
import { ActivityPage } from "@/pages/Activity";
import { TelemetryPage } from "@/pages/Telemetry";
import { ObsAnalyticsPage } from "@/pages/obs/Analytics";
import { ObsTrajectoriesPage } from "@/pages/obs/Trajectories";
import { ObsTraceDetailPage } from "@/pages/obs/TraceDetail";
import { ObsEvalsPage } from "@/pages/obs/Evals";
import { ObsCostPage } from "@/pages/obs/Cost";
import { ObsAlertsPage } from "@/pages/obs/Alerts";
import { OptimizePage } from "@/pages/Optimize";
import { SessionsPage } from "@/pages/Sessions";
import { LivePage } from "@/pages/Live";
import { MoversPage } from "@/pages/Movers";
import { ReportPage } from "@/pages/Report";
import { SuggestionsPage } from "@/pages/Suggestions";
import { ProjectsPage } from "@/pages/Projects";
import { ProjectDetailPage } from "@/pages/ProjectDetail";
import { AuditPage } from "@/pages/Audit";
import { InvitePage } from "@/pages/Invite";
import { PolicyPage } from "@/pages/Policy";
import { SecurityPage } from "@/pages/Security";
import { SettingsPage } from "@/pages/Settings";

export default function App() {
  const { pathname } = useLocation();
  return (
    <Layout>
      <RouteErrorBoundary key={pathname}>
        <Routes>
        <Route path="/" element={<OverviewPage />} />
        <Route path="/teams" element={<TeamsPage />} />
        <Route path="/teams/:id" element={<TeamDetailPage />} />
        <Route path="/people" element={<PeoplePage />} />
        <Route path="/tools" element={<ToolsPage />} />
        <Route path="/models" element={<ModelsPage />} />
        <Route path="/activity" element={<ActivityPage />} />
        <Route path="/telemetry" element={<TelemetryPage />} />
        <Route path="/trajectories" element={<ObsTrajectoriesPage />} />
        <Route path="/trajectories/analytics" element={<ObsAnalyticsPage />} />
        <Route path="/trajectories/evals" element={<ObsEvalsPage />} />
        <Route path="/trajectories/cost" element={<ObsCostPage />} />
        <Route path="/trajectories/alerts" element={<ObsAlertsPage />} />
        <Route path="/trajectories/:id" element={<ObsTraceDetailPage />} />
        <Route path="/routing" element={<OptimizePage />} />
        <Route path="/suggestions" element={<SuggestionsPage />} />
        <Route path="/report" element={<ReportPage />} />
        <Route path="/movers" element={<MoversPage />} />
        <Route path="/sessions" element={<SessionsPage />} />
        <Route path="/live" element={<LivePage />} />
        <Route path="/projects" element={<ProjectsPage />} />
        <Route path="/projects/:id" element={<ProjectDetailPage />} />
        <Route path="/security" element={<SecurityPage />} />
        <Route path="/policy" element={<PolicyPage />} />
        <Route path="/invite" element={<InvitePage />} />
        <Route path="/audit" element={<AuditPage />} />
        <Route path="/settings" element={<SettingsPage />} />
        <Route path="*" element={<OverviewPage />} />
        </Routes>
      </RouteErrorBoundary>
    </Layout>
  );
}
