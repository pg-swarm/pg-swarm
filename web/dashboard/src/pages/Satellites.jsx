import { useState, useEffect } from 'react';
import { useData } from '../context/DataContext';
import { useToast } from '../context/ToastContext';
import { api, deriveSatState, timeAgo } from '../api';
import { SatBadge } from '../components/Badge';
import {
  Check, X, Tag, Plus, Save, Satellite, Terminal, AlertTriangle, Layers
} from 'lucide-react';

export default function Satellites() {
  const { satellites, storageTiers, refresh } = useData();
  const toast = useToast();

  useEffect(() => { document.title = 'Satellites - PG-Swarm'; }, []);
  const [editingLabels, setEditingLabels] = useState(null);
  const [labelKey, setLabelKey] = useState('');
  const [labelVal, setLabelVal] = useState('');
  const [pendingLabels, setPendingLabels] = useState({});
  const [replaceConfirm, setReplaceConfirm] = useState(null); // { id, old }
  const [tierModal, setTierModal] = useState(null); // full satellite object
  const [pendingTiers, setPendingTiers] = useState({});
  const [tierKey, setTierKey] = useState('');
  const [tierVal, setTierVal] = useState('');

  async function approve(id, replace = false) {
    try {
      await api.approve(id, replace);
      toast(replace ? 'Satellite approved (replaced previous)' : 'Satellite approved');
      setReplaceConfirm(null);
      refresh();
    } catch (e) {
      if (e.status === 409 && e.body?.conflicting_satellite) {
        setReplaceConfirm({ id, old: e.body.conflicting_satellite });
        return;
      }
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

  function startEditTiers(sat) {
    setTierModal(sat);
    setPendingTiers({ ...(sat.tier_mappings || {}) });
    setTierKey('');
    setTierVal('');
  }

  function addTier() {
    if (!tierKey.trim()) return;
    setPendingTiers(prev => ({ ...prev, [tierKey.trim()]: tierVal.trim() }));
    setTierKey('');
    setTierVal('');
  }

  function removeTier(key) {
    setPendingTiers(prev => {
      const next = { ...prev };
      delete next[key];
      return next;
    });
  }

  async function saveTiers() {
    try {
      await api.updateSatelliteTierMappings(tierModal.id, pendingTiers);
      toast('Tier mappings updated');
      setTierModal(null);
      refresh();
    } catch (e) {
      toast('Failed to update tier mappings: ' + e.message, true);
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
            const wouldReplace = state === 'pending' && satellites.some(
              o => o.id !== s.id && o.k8s_cluster_name === s.k8s_cluster_name &&
                   (o.state === 'approved' || o.state === 'connected' || deriveSatState(o) === 'offline')
            );
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
                        {wouldReplace && (
                          <span className="badge badge-amber" title="Approving will replace the existing satellite on this cluster">
                            <AlertTriangle size={11} /> replaces existing
                          </span>
                        )}
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
                        <button className="btn btn-sm" onClick={() => startEditTiers(s)}><Layers size={11} /> Tiers</button>
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

      {tierModal && (
        <div className="confirm-overlay" onClick={() => setTierModal(null)}>
          <div className="confirm-modal" onClick={e => e.stopPropagation()} style={{ maxWidth: 520 }}>
            <div className="confirm-header">
              <h3><Layers size={18} /> Tier Mappings — {tierModal.hostname}</h3>
              <button className="modal-close" onClick={() => setTierModal(null)}><X size={18} /></button>
            </div>
            <div className="confirm-body">
              <p className="sm muted" style={{ margin: '0 0 12px' }}>Map admin-defined storage tiers to this satellite's concrete storage classes. Profiles reference tiers; the mapping is resolved at deployment time.</p>

              {Object.keys(pendingTiers).length > 0 && (
                <table style={{ width: '100%', marginBottom: 12 }}>
                  <thead>
                    <tr>
                      <th>Tier</th>
                      <th>Storage Class</th>
                      <th style={{ width: 40 }}></th>
                    </tr>
                  </thead>
                  <tbody>
                    {Object.entries(pendingTiers).map(([k, v]) => (
                      <tr key={k}>
                        <td className="mono">{k}</td>
                        <td className="mono">{v}</td>
                        <td>
                          <button className="btn btn-sm btn-reject" onClick={() => removeTier(k)} style={{ padding: '2px 4px' }}><X size={10} /></button>
                        </td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              )}
              {Object.keys(pendingTiers).length === 0 && (
                <p className="muted" style={{ textAlign: 'center', padding: '12px 0' }}>No mappings configured</p>
              )}

              <div style={{ display: 'flex', gap: 4, alignItems: 'center' }}>
                <select className="input" value={tierKey} onChange={e => setTierKey(e.target.value)} style={{ flex: 1 }}>
                  <option value="">select tier...</option>
                  {(storageTiers || []).filter(t => !(t.name in pendingTiers)).map(t => (
                    <option key={t.name} value={t.name}>{t.name}{t.description ? ' — ' + t.description : ''}</option>
                  ))}
                </select>
                <select className="input" value={tierVal} onChange={e => setTierVal(e.target.value)} style={{ flex: 1 }}>
                  <option value="">select class...</option>
                  {(tierModal.storage_classes || []).map(sc => (
                    <option key={sc.name} value={sc.name}>{sc.name}{sc.provisioner ? ' — ' + sc.provisioner : ''}</option>
                  ))}
                </select>
                <button className="btn btn-sm" onClick={addTier}><Plus size={12} /> Add</button>
              </div>
            </div>
            <div className="confirm-footer">
              <button className="btn-sm" onClick={() => setTierModal(null)}>Cancel</button>
              <button className="btn-sm btn-approve" onClick={saveTiers}><Save size={11} /> Save</button>
            </div>
          </div>
        </div>
      )}

      {replaceConfirm && (
        <div className="confirm-overlay" onClick={() => setReplaceConfirm(null)}>
          <div className="confirm-modal" onClick={e => e.stopPropagation()}>
            <div className="confirm-header">
              <h3><AlertTriangle size={18} style={{ color: 'var(--amber)' }} /> Replace Satellite</h3>
              <button className="modal-close" onClick={() => setReplaceConfirm(null)}><X size={18} /></button>
            </div>
            <div className="confirm-body">
              <p>This will replace the existing satellite <strong>{replaceConfirm.old.hostname}</strong> on cluster <strong>{replaceConfirm.old.k8s_cluster_name}</strong>.</p>
              <p>All cluster configs will be transferred to the new satellite and the old one will be deactivated.</p>
            </div>
            <div className="confirm-footer">
              <button className="btn-sm" onClick={() => setReplaceConfirm(null)}>Cancel</button>
              <button className="btn-sm btn-danger" onClick={() => approve(replaceConfirm.id, true)}>Replace Satellite</button>
            </div>
          </div>
        </div>
      )}
    </div>
  );
}
