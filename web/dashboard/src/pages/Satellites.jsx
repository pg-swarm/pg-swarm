import { useState, useEffect } from 'react';
import { useData } from '../context/DataContext';
import { useToast } from '../context/ToastContext';
import { api, deriveSatState, timeAgo } from '../api';
import { SatBadge } from '../components/Badge';
import {
  Check, X, Tag, Plus, Save, Satellite, Terminal
} from 'lucide-react';

export default function Satellites() {
  const { satellites, refresh } = useData();
  const toast = useToast();

  useEffect(() => { document.title = 'Satellites - pg-swarm'; }, []);
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
      {satellites.length === 0 ? (
        <div className="empty-state" style={{ padding: '40px 20px' }}>
          <Satellite size={48} strokeWidth={1.2} />
          <h3>No satellites registered</h3>
          <p>Deploy a satellite agent to your edge Kubernetes clusters to get started.</p>
        </div>
      ) : (
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
          {satellites.map(s => {
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
                          <span key={k} className="tag tag-removable" style={{ marginRight: 4 }}>
                            {k}={v}
                            <button className="tag-x" onClick={() => removeLabel(k)}><X size={10} /></button>
                          </span>
                        ))}
                      </div>
                      <div style={{ display: 'flex', gap: 4, alignItems: 'center' }}>
                        <input className="input" placeholder="key" value={labelKey} onChange={e => setLabelKey(e.target.value)} style={{ width: 80 }} />
                        <input className="input" placeholder="value" value={labelVal} onChange={e => setLabelVal(e.target.value)} style={{ width: 80 }} />
                        <button className="btn btn-sm" onClick={addLabel}><Plus size={12} /></button>
                      </div>
                      <div className="actions" style={{ marginTop: 4 }}>
                        <button className="btn btn-sm btn-approve" onClick={saveLabels}><Save size={11} /> Save</button>
                        <button className="btn btn-sm btn-reject" onClick={() => setEditingLabels(null)}><X size={11} /> Cancel</button>
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
                        <button className="btn btn-approve" onClick={() => approve(s.id)}><Check size={13} /> Approve</button>
                        <button className="btn btn-reject" onClick={() => reject(s.id)}><X size={13} /> Reject</button>
                      </>
                    )}
                    {state !== 'pending' && !isEditingThis && (
                      <>
                        <button className="btn btn-sm" onClick={() => window.open('/satellites/' + s.id + '/logs', '_blank')}>
                          <Terminal size={11} /> Logs
                        </button>
                        <button className="btn btn-sm" onClick={() => startEditLabels(s)}><Tag size={11} /> Labels</button>
                      </>
                    )}
                  </div>
                </td>
              </tr>
            );
          })}
        </tbody>
      </table>
      )}
    </div>
  );
}
