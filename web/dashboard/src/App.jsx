import { Routes, Route, Navigate } from 'react-router-dom';
import Layout from './components/Layout';
import Overview from './pages/Overview';
import Satellites from './pages/Satellites';
import Clusters from './pages/Clusters';
import Events from './pages/Events';
import Profiles from './pages/Profiles';
import DeploymentRules from './pages/DeploymentRules';
import Admin from './pages/Admin';

export default function App() {
  return (
    <Routes>
      <Route element={<Layout />}>
        <Route path="/" element={<Overview />} />
        <Route path="/satellites" element={<Satellites />} />
        <Route path="/clusters" element={<Clusters />} />
        <Route path="/profiles" element={<Profiles />} />
        <Route path="/deployment-rules" element={<DeploymentRules />} />
        <Route path="/events" element={<Events />} />
        <Route path="/admin" element={<Admin />} />
        <Route path="*" element={<Navigate to="/" replace />} />
      </Route>
    </Routes>
  );
}
