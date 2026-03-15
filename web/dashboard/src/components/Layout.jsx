import { NavLink, Outlet } from 'react-router-dom';
import { useData } from '../context/DataContext';
import { useTheme } from '../context/ThemeContext';
import { deriveSatState } from '../api';
import {
  LayoutDashboard, Satellite, Boxes, GitBranch, Database,
  Activity, Settings, RefreshCw, Sun, Moon, Monitor, Archive
} from 'lucide-react';

const NAV = [
  { to: '/',                  label: 'Overview',          icon: LayoutDashboard },
  { to: '/satellites',        label: 'Satellites',        icon: Satellite },
  { to: '/profiles',          label: 'Profiles',          icon: Boxes },
  { to: '/deployment-rules',  label: 'Deployment Rules',  icon: GitBranch },
  { to: '/backup-rules',     label: 'Backup Rules',      icon: Archive },
  { to: '/clusters',          label: 'Clusters',          icon: Database },
  { to: '/events',            label: 'Events',            icon: Activity },
  { to: '/admin',             label: 'Admin',             icon: Settings },
];

function StatusDot({ satellites }) {
  const online = satellites.filter(s => deriveSatState(s) === 'connected').length;
  const total  = satellites.length;

  let color, text;
  if (total === 0)        { color = 'dot-gray';  text = 'No satellites'; }
  else if (online === total) { color = 'dot-green'; text = `${online}/${total} online`; }
  else if (online > 0)      { color = 'dot-amber'; text = `${online}/${total} online`; }
  else                       { color = 'dot-red';   text = 'All offline'; }

  return <span className="status-pill"><span className={`online-dot ${color}`} />{text}</span>;
}

export default function Layout() {
  const { satellites, lastRefresh, refresh } = useData();
  const { theme, setTheme } = useTheme();
  const ThemeIcon = { light: Sun, dark: Moon, system: Monitor }[theme];
  function cycleTheme() {
    const cycle = ['light', 'dark', 'system'];
    setTheme(cycle[(cycle.indexOf(theme) + 1) % cycle.length]);
  }

  return (
    <>
      <header className="topbar">
        <div className="brand">
          <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round">
            <ellipse cx="12" cy="5" rx="9" ry="3"/>
            <path d="M3 5V19C3 20.5 7 22 12 22S21 20.5 21 19V5"/>
            <path d="M3 12C3 13.5 7 15 12 15S21 13.5 21 12"/>
          </svg>
          pg-swarm
        </div>
        <div className="topbar-right">
          <StatusDot satellites={satellites} />
          <button className="theme-toggle" onClick={cycleTheme} title={`Theme: ${theme}`}>
            <ThemeIcon size={13} />
            {theme}
          </button>
        </div>
      </header>

      <nav className="nav">
        {NAV.map(n => {
          const Icon = n.icon;
          return (
            <NavLink
              key={n.to}
              to={n.to}
              end={n.to === '/'}
              className={({ isActive }) => 'nav-item' + (isActive ? ' active' : '')}
            >
              <Icon size={15} />
              {n.label}
            </NavLink>
          );
        })}
        <div className="nav-spacer" />
        <div className="refresh">
          <span>{lastRefresh ? lastRefresh.toLocaleTimeString() : '-'}</span>
          <button onClick={refresh}><RefreshCw size={12} /> Refresh</button>
        </div>
      </nav>

      <div className="container">
        <Outlet />
      </div>
    </>
  );
}
