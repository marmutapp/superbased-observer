import { Route, Routes } from "react-router-dom";
import { Layout } from "@/components/Layout";
import { OverviewPage } from "@/pages/Overview";
import { TeamsPage } from "@/pages/Teams";
import { TeamDetailPage } from "@/pages/TeamDetail";
import { ProjectsPage } from "@/pages/Projects";
import { ProjectDetailPage } from "@/pages/ProjectDetail";
import { AuditPage } from "@/pages/Audit";
import { InvitePage } from "@/pages/Invite";
import { PolicyPage } from "@/pages/Policy";
import { SecurityPage } from "@/pages/Security";
import { SettingsPage } from "@/pages/Settings";

export default function App() {
  return (
    <Layout>
      <Routes>
        <Route path="/" element={<OverviewPage />} />
        <Route path="/teams" element={<TeamsPage />} />
        <Route path="/teams/:id" element={<TeamDetailPage />} />
        <Route path="/projects" element={<ProjectsPage />} />
        <Route path="/projects/:id" element={<ProjectDetailPage />} />
        <Route path="/security" element={<SecurityPage />} />
        <Route path="/policy" element={<PolicyPage />} />
        <Route path="/invite" element={<InvitePage />} />
        <Route path="/audit" element={<AuditPage />} />
        <Route path="/settings" element={<SettingsPage />} />
        <Route path="*" element={<OverviewPage />} />
      </Routes>
    </Layout>
  );
}
