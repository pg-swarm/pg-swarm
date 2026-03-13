import { NavLink, Outlet } from 'react-router-dom';
import { useData } from '../context/DataContext';
import { deriveSatState } from '../api';

const NAV = [
  { to: '/',                  label: 'Overview' },
  { to: '/satellites',        label: 'Satellites' },
  { to: '/profiles',          label: 'Profiles' },
  { to: '/deployment-rules',  label: 'Deployment Rules' },
  { to: '/clusters',          label: 'Clusters' },
  { to: '/events',            label: 'Events' },
];

function StatusDot({ satellites }) {
  const online = satellites.filter(s => deriveSatState(s) === 'connected').length;
  const total  = satellites.length;

  let color, text;
  if (total === 0)        { color = 'dot-gray';  text = 'No satellites'; }
  else if (online === total) { color = 'dot-green'; text = `${online}/${total} online`; }
  else if (online > 0)      { color = 'dot-amber'; text = `${online}/${total} online`; }
  else                       { color = 'dot-red';   text = 'All offline'; }

  return <span><span className={`online-dot ${color}`} />{text}</span>;
}

export default function Layout() {
  const { satellites, lastRefresh, refresh } = useData();

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
        </div>
      </header>

      <nav className="nav">
        {NAV.map(n => (
          <NavLink
            key={n.to}
            to={n.to}
            end={n.to === '/'}
            className={({ isActive }) => 'nav-item' + (isActive ? ' active' : '')}
          >
            {n.label}
          </NavLink>
        ))}
        <div className="nav-spacer" />
        <div className="refresh">
          <span>{lastRefresh ? lastRefresh.toLocaleTimeString() : '-'}</span>
          <button onClick={refresh}>Refresh</button>
        </div>
      </nav>

      <div className="container">
        <Outlet />
      </div>
    </>
  );
}
