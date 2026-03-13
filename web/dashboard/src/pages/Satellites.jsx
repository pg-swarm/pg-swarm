import { useData } from '../context/DataContext';
import { useToast } from '../context/ToastContext';
import { api, deriveSatState, timeAgo } from '../api';
import { SatBadge } from '../components/Badge';

export default function Satellites() {
  const { satellites, refresh } = useData();
  const toast = useToast();

  async function approve(id) {
    try {
      await api.approve(id);
      toast('Satellite approved');
      refresh();
    } catch (e) {
      toast('Approve failed: ' + e.message, true);
    }
  }

  async function reject(id) {
    try {
      await api.reject(id);
      toast('Satellite rejected');
      refresh();
    } catch (e) {
      toast('Reject failed: ' + e.message, true);
    }
  }

  return (
    <div className="card">
      <div className="card-head">Satellites</div>
      <table>
        <thead>
          <tr>
            <th>Hostname</th>
            <th>K8s Cluster</th>
            <th>Region</th>
            <th>State</th>
            <th>Last Heartbeat</th>
            <th>ID</th>
            <th style={{ width: 150 }}>Actions</th>
          </tr>
        </thead>
        <tbody>
          {satellites.length === 0 ? (
            <tr><td colSpan={7} className="empty">No satellites registered</td></tr>
          ) : satellites.map(s => {
            const state = deriveSatState(s);
            return (
              <tr key={s.id}>
                <td className="mono sm">{s.hostname}</td>
                <td>{s.k8s_cluster_name}</td>
                <td>{s.region || '-'}</td>
                <td><SatBadge state={state} /></td>
                <td className="sm muted">{timeAgo(s.last_heartbeat)}</td>
                <td className="mono sm muted">{s.id.substring(0, 8)}</td>
                <td>
                  {state === 'pending' && (
                    <div className="actions">
                      <button className="btn btn-approve" onClick={() => approve(s.id)}>Approve</button>
                      <button className="btn btn-reject" onClick={() => reject(s.id)}>Reject</button>
                    </div>
                  )}
                </td>
              </tr>
            );
          })}
        </tbody>
      </table>
    </div>
  );
}
