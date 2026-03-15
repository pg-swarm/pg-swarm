import { useEffect } from 'react';
import { useData } from '../context/DataContext';
import { deriveSatState, timeAgo } from '../api';
import {
  Satellite, Database, HeartPulse, Bell,
  Info, AlertTriangle, AlertCircle, Flame
} from 'lucide-react';

const STAT_ICONS = [
  { icon: Satellite, color: 'var(--blue)' },
  { icon: Database, color: 'var(--green)' },
  { icon: HeartPulse, color: 'var(--green)' },
  { icon: Bell, color: 'var(--amber)' },
];

const SEV_ICONS = {
  info: Info,
  warning: AlertTriangle,
  error: AlertCircle,
  critical: Flame,
};

export default function Overview() {
  const { satellites, clusters, events } = useData();

  useEffect(() => { document.title = 'Overview - pg-swarm'; }, []);

  const connected = satellites.filter(s => deriveSatState(s) === 'connected').length;
  const pending   = satellites.filter(s => deriveSatState(s) === 'pending').length;
  const running   = clusters.filter(c => c.state === 'running').length;
  const unhealthy = clusters.filter(c => c.state === 'degraded' || c.state === 'failed').length;

  const stats = [
    { label: 'Satellites', value: satellites.length, detail: `${connected} connected${pending ? `, ${pending} pending` : ''}` },
    { label: 'Clusters', value: clusters.length, detail: `${running} running${unhealthy ? `, ${unhealthy} unhealthy` : ''}` },
    { label: 'Healthy', value: running, accent: true, detail: clusters.length ? `${Math.round(running / clusters.length * 100)}% of clusters` : 'no clusters' },
    { label: 'Recent Events', value: events.length, detail: events.length ? `latest ${timeAgo(events[0].created_at)}` : 'none' },
  ];

  return (
    <>
      <div className="stats">
        {stats.map((s, i) => {
          const Icon = STAT_ICONS[i].icon;
          return (
            <div className="stat-card" key={s.label}>
              <div className="stat-header">
                <div className="label">{s.label}</div>
                <div className="stat-icon" style={{ color: STAT_ICONS[i].color }}>
                  <Icon size={20} />
                </div>
              </div>
              <div className={`number${s.accent ? ' accent' : ''}`}>{s.value}</div>
              <div className="detail">{s.detail}</div>
            </div>
          );
        })}
      </div>

      <div className="card">
        <div className="card-head">Recent Activity</div>
        <table>
          <thead>
            <tr>
              <th style={{ width: 90 }}>When</th>
              <th style={{ width: 100 }}>Severity</th>
              <th style={{ width: 150 }}>Cluster</th>
              <th>Message</th>
            </tr>
          </thead>
          <tbody>
            {events.length === 0 ? (
              <tr><td colSpan={4} className="empty">No events yet</td></tr>
            ) : events.slice(0, 8).map(e => {
              const SevIcon = SEV_ICONS[e.severity] || Info;
              return (
                <tr key={e.id}>
                  <td className="sm muted">{timeAgo(e.created_at)}</td>
                  <td>
                    <span className={`sev-pill sev-${e.severity}`}>
                      <SevIcon size={12} />
                      {e.severity}
                    </span>
                  </td>
                  <td className="mono">{e.cluster_name}</td>
                  <td className="sm">{e.message}</td>
                </tr>
              );
            })}
          </tbody>
        </table>
      </div>
    </>
  );
}
