import { useState } from 'react';
import { useData } from '../context/DataContext';
import { useToast } from '../context/ToastContext';
import { api, deriveSatState, timeAgo } from '../api';
import { SatBadge } from '../components/Badge';

export default function Satellites() {
  const { satellites, refresh } = useData();
  const toast = useToast();
  const [editingLabels, setEditingLabels] = useState(null);
  const [labelKey, setLabelKey] = useState('');
  const [labelVal, setLabelVal] = useState('');
  const [pendingLabels, setPendingLabels] = useState({});

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

  function startEditLabels(sat) {
    setEditingLabels(sat.id);
    setPendingLabels({ ...(sat.labels || {}) });
    setLabelKey('');
    setLabelVal('');
  }

  function addLabel() {
    if (!labelKey.trim()) return;
    setPendingLabels(prev => ({ ...prev, [labelKey.trim()]: labelVal.trim() }));
    setLabelKey('');
    setLabelVal('');
  }

  function removeLabel(key) {
    setPendingLabels(prev => {
      const next = { ...prev };
      delete next[key];
      return next;
    });
  }

  async function saveLabels() {
    try {
      await api.updateSatelliteLabels(editingLabels, pendingLabels);
      toast('Labels updated');
      setEditingLabels(null);
      refresh();
    } catch (e) {
      toast('Failed to update labels: ' + e.message, true);
    }
  }

  function renderLabels(labels) {
    const entries = Object.entries(labels || {});
    if (entries.length === 0) return <span className="muted">none</span>;
    return entries.map(([k, v]) => (
      <span key={k} className="tag" style={{ marginRight: 4 }}>{k}={v}</span>
    ));
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
            <th>Labels</th>
            <th>State</th>
            <th>Last Heartbeat</th>
            <th style={{ width: 200 }}>Actions</th>
          </tr>
        </thead>
        <tbody>
          {satellites.length === 0 ? (
            <tr><td colSpan={7} className="empty">No satellites registered</td></tr>
          ) : satellites.map(s => {
            const state = deriveSatState(s);
            const isEditingThis = editingLabels === s.id;
            return (
              <tr key={s.id}>
                <td className="mono sm">{s.hostname}</td>
                <td>{s.k8s_cluster_name}</td>
                <td>{s.region || '-'}</td>
                <td>
                  {isEditingThis ? (
                    <div>
                      <div style={{ marginBottom: 4 }}>
                        {Object.entries(pendingLabels).map(([k, v]) => (
                          <span key={k} className="tag" style={{ marginRight: 4 }}>
                            {k}={v}
                            <button
                              style={{ marginLeft: 4, cursor: 'pointer', background: 'none', border: 'none', color: 'inherit', padding: 0, fontSize: '0.85em' }}
                              onClick={() => removeLabel(k)}
                            >x</button>
                          </span>
                        ))}
                      </div>
                      <div style={{ display: 'flex', gap: 4, alignItems: 'center' }}>
                        <input className="input" placeholder="key" value={labelKey} onChange={e => setLabelKey(e.target.value)} style={{ width: 80 }} />
                        <input className="input" placeholder="value" value={labelVal} onChange={e => setLabelVal(e.target.value)} style={{ width: 80 }} />
                        <button className="btn btn-sm" onClick={addLabel}>+</button>
                      </div>
                      <div className="actions" style={{ marginTop: 4 }}>
                        <button className="btn btn-sm btn-approve" onClick={saveLabels}>Save</button>
                        <button className="btn btn-sm btn-reject" onClick={() => setEditingLabels(null)}>Cancel</button>
                      </div>
                    </div>
                  ) : renderLabels(s.labels)}
                </td>
                <td><SatBadge state={state} /></td>
                <td className="sm muted">{timeAgo(s.last_heartbeat)}</td>
                <td>
                  <div className="actions">
                    {state === 'pending' && (
                      <>
                        <button className="btn btn-approve" onClick={() => approve(s.id)}>Approve</button>
                        <button className="btn btn-reject" onClick={() => reject(s.id)}>Reject</button>
                      </>
                    )}
                    {state !== 'pending' && !isEditingThis && (
                      <button className="btn btn-sm" onClick={() => startEditLabels(s)}>Labels</button>
                    )}
                  </div>
                </td>
              </tr>
            );
          })}
        </tbody>
      </table>
    </div>
  );
}
