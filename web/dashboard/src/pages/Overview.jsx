import { useData } from '../context/DataContext';
import { deriveSatState, timeAgo } from '../api';

export default function Overview() {
  const { satellites, clusters, events } = useData();

  const connected = satellites.filter(s => deriveSatState(s) === 'connected').length;
  const pending   = satellites.filter(s => deriveSatState(s) === 'pending').length;
  const running   = clusters.filter(c => c.state === 'running').length;
  const unhealthy = clusters.filter(c => c.state === 'degraded' || c.state === 'failed').length;

  return (
    <>
      <div className="stats">
        <StatCard label="Satellites" value={satellites.length}
          detail={`${connected} connected${pending ? `, ${pending} pending` : ''}`} />
        <StatCard label="Clusters" value={clusters.length}
          detail={`${running} running${unhealthy ? `, ${unhealthy} unhealthy` : ''}`} />
        <StatCard label="Healthy" value={running} accent
          detail={clusters.length ? `${Math.round(running / clusters.length * 100)}% of clusters` : 'no clusters'} />
        <StatCard label="Recent Events" value={events.length}
          detail={events.length ? `latest ${timeAgo(events[0].created_at)}` : 'none'} />
      </div>

      <div className="card">
        <div className="card-head">Recent Activity</div>
        <table>
          <thead>
            <tr>
              <th style={{ width: 90 }}>When</th>
              <th style={{ width: 76 }}>Severity</th>
              <th style={{ width: 150 }}>Cluster</th>
              <th>Message</th>
            </tr>
          </thead>
          <tbody>
            {events.length === 0 ? (
              <tr><td colSpan={4} className="empty">No events yet</td></tr>
            ) : events.slice(0, 8).map(e => (
              <tr key={e.id}>
                <td className="sm muted">{timeAgo(e.created_at)}</td>
                <td className={`sev-${e.severity}`}>{e.severity}</td>
                <td className="mono">{e.cluster_name}</td>
                <td className="sm">{e.message}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </>
  );
}

function StatCard({ label, value, detail, accent }) {
  return (
    <div className="stat-card">
      <div className="label">{label}</div>
      <div className={`number${accent ? ' accent' : ''}`}>{value}</div>
      <div className="detail">{detail}</div>
    </div>
  );
}
