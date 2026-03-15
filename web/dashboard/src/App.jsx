import { Routes, Route, Navigate } from 'react-router-dom';
import Layout from './components/Layout';
import Overview from './pages/Overview';
import Satellites from './pages/Satellites';
import SatelliteLogs from './pages/SatelliteLogs';
import Clusters from './pages/Clusters';
import ClusterDetail from './pages/ClusterDetail';
import Events from './pages/Events';
import Profiles from './pages/Profiles';
import DeploymentRules from './pages/DeploymentRules';
import BackupRules from './pages/BackupRules';
import Admin from './pages/Admin';

export default function App() {
  return (
    <Routes>
      {/* Full-page routes (no sidebar/nav) */}
      <Route path="/satellites/:id/logs" element={<SatelliteLogs />} />
      <Route path="/clusters/:id" element={<ClusterDetail />} />

      <Route element={<Layout />}>
        <Route path="/" element={<Overview />} />
        <Route path="/satellites" element={<Satellites />} />
        <Route path="/clusters" element={<Clusters />} />
        <Route path="/profiles" element={<Profiles />} />
        <Route path="/deployment-rules" element={<DeploymentRules />} />
        <Route path="/backup-rules" element={<BackupRules />} />
        <Route path="/events" element={<Events />} />
        <Route path="/admin" element={<Admin />} />
        <Route path="*" element={<Navigate to="/" replace />} />
      </Route>
    </Routes>
  );
}
