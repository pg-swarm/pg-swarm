import { useData } from '../context/DataContext';

export default function Events() {
  const { events, satellites } = useData();

  return (
    <div className="card">
      <div className="card-head">Events</div>
      <table>
        <thead>
          <tr>
            <th style={{ width: 160 }}>Time</th>
            <th style={{ width: 76 }}>Severity</th>
            <th style={{ width: 150 }}>Cluster</th>
            <th style={{ width: 130 }}>Satellite</th>
            <th>Message</th>
          </tr>
        </thead>
        <tbody>
          {events.length === 0 ? (
            <tr><td colSpan={5} className="empty">No events recorded</td></tr>
          ) : events.map(e => {
            const sat = satellites.find(s => s.id === e.satellite_id);
            return (
              <tr key={e.id}>
                <td className="sm muted">{new Date(e.created_at).toLocaleString()}</td>
                <td className={`sev-${e.severity}`}>{e.severity}</td>
                <td className="mono">{e.cluster_name}</td>
                <td className="sm">{sat ? sat.hostname : e.satellite_id.substring(0, 8)}</td>
                <td className="sm">{e.message}</td>
              </tr>
            );
          })}
        </tbody>
      </table>
    </div>
  );
}
