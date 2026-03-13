import { useState } from 'react';
import { useData } from '../context/DataContext';
import { useToast } from '../context/ToastContext';
import { api, parseSpec, timeAgo } from '../api';

export default function DeploymentRules() {
  const { deploymentRules, profiles, clusters, refresh } = useData();
  const toast = useToast();
  const [creating, setCreating] = useState(false);
  const [editing, setEditing] = useState(null);
  const [expanded, setExpanded] = useState(null);
  const [ruleClusters, setRuleClusters] = useState([]);

  const [form, setForm] = useState({ name: '', profile_id: '', label_selector: {}, namespace: 'default', cluster_name: '' });
  const [selectorKey, setSelectorKey] = useState('');
  const [selectorVal, setSelectorVal] = useState('');

  function profileName(pid) {
    const p = profiles.find(p => p.id === pid);
    return p ? p.name : pid?.slice(0, 8) || '-';
  }

  function clusterCount(ruleId) {
    return clusters.filter(c => c.deployment_rule_id === ruleId).length;
  }

  function renderSelector(sel) {
    const entries = Object.entries(sel || {});
    if (entries.length === 0) return <span className="muted">all satellites</span>;
    return entries.map(([k, v]) => (
      <span key={k} className="tag" style={{ marginRight: 4 }}>{k}={v}</span>
    ));
  }

  function startCreate() {
    setForm({ name: '', profile_id: profiles[0]?.id || '', label_selector: {}, namespace: 'default', cluster_name: '' });
    setSelectorKey('');
    setSelectorVal('');
    setCreating(true);
    setEditing(null);
  }

  function startEdit(rule) {
    setForm({ name: rule.name, profile_id: rule.profile_id, label_selector: { ...(rule.label_selector || {}) }, namespace: rule.namespace, cluster_name: rule.cluster_name });
    setSelectorKey('');
    setSelectorVal('');
    setEditing(rule.id);
    setCreating(false);
  }

  function addSelectorEntry() {
    if (!selectorKey.trim()) return;
    setForm(f => ({ ...f, label_selector: { ...f.label_selector, [selectorKey.trim()]: selectorVal.trim() } }));
    setSelectorKey('');
    setSelectorVal('');
  }

  function removeSelectorEntry(key) {
    setForm(f => {
      const next = { ...f.label_selector };
      delete next[key];
      return { ...f, label_selector: next };
    });
  }

  async function save() {
    const payload = { name: form.name, profile_id: form.profile_id, label_selector: form.label_selector, namespace: form.namespace, cluster_name: form.cluster_name };
    try {
      if (editing) {
        await api.updateDeploymentRule(editing, payload);
        toast('Deployment rule updated');
      } else {
        await api.createDeploymentRule(payload);
        toast('Deployment rule created');
      }
      setCreating(false);
      setEditing(null);
      refresh();
    } catch (e) {
      toast('Save failed: ' + e.message, true);
    }
  }

  async function remove(id) {
    try {
      await api.deleteDeploymentRule(id);
      toast('Deployment rule deleted');
      if (expanded === id) setExpanded(null);
      refresh();
    } catch (e) {
      toast('Delete failed: ' + e.message, true);
    }
  }

  async function toggleExpand(id) {
    if (expanded === id) {
      setExpanded(null);
      return;
    }
    try {
      const cls = await api.deploymentRuleClusters(id);
      setRuleClusters(cls || []);
      setExpanded(id);
    } catch (e) {
      toast('Failed to load clusters: ' + e.message, true);
    }
  }

  const showForm = creating || editing;
  const canSave = form.name.trim() && form.profile_id && form.cluster_name.trim();

  return (
    <>
      <div className="card-head-bar">
        <span className="card-head-title">Deployment Rules</span>
        <button className="btn btn-approve" onClick={startCreate}>+ New Rule</button>
      </div>

      <p className="muted sm" style={{ marginBottom: 16 }}>
        A deployment rule maps a profile (WHAT) to satellites matching a label selector (WHERE). A cluster config is created for each matching satellite. Empty selector matches all satellites.
      </p>

      {/* Create / Edit form */}
      {showForm && (
        <div className="dg-form-card">
          <h4>{editing ? 'Edit Rule' : 'Create Rule'}</h4>
          <div className="form-grid">
            <div className="form-row">
              <label>Name</label>
              <input className="input" value={form.name} onChange={e => setForm(f => ({ ...f, name: e.target.value }))} placeholder="e.g. prod-analytics-db" />
            </div>
            <div className="form-row">
              <label>Profile</label>
              <select className="input" value={form.profile_id} onChange={e => setForm(f => ({ ...f, profile_id: e.target.value }))}>
                <option value="">Select a profile...</option>
                {profiles.map(p => (
                  <option key={p.id} value={p.id}>{p.name}{p.locked ? ' (locked)' : ''}</option>
                ))}
              </select>
            </div>
            <div className="form-row">
              <label>Label Selector</label>
              <div>
                <div style={{ marginBottom: 4 }}>
                  {Object.entries(form.label_selector).map(([k, v]) => (
                    <span key={k} className="tag" style={{ marginRight: 4 }}>
                      {k}={v}
                      <button
                        style={{ marginLeft: 4, cursor: 'pointer', background: 'none', border: 'none', color: 'inherit', padding: 0, fontSize: '0.85em' }}
                        onClick={() => removeSelectorEntry(k)}
                      >x</button>
                    </span>
                  ))}
                  {Object.keys(form.label_selector).length === 0 && <span className="muted sm">Empty = matches all satellites</span>}
                </div>
                <div style={{ display: 'flex', gap: 4, alignItems: 'center' }}>
                  <input className="input" placeholder="key" value={selectorKey} onChange={e => setSelectorKey(e.target.value)} style={{ width: 120 }} />
                  <input className="input" placeholder="value" value={selectorVal} onChange={e => setSelectorVal(e.target.value)} style={{ width: 120 }} />
                  <button className="btn btn-sm" onClick={addSelectorEntry}>+</button>
                </div>
              </div>
            </div>
            <div className="form-row">
              <label>Namespace</label>
              <input className="input" value={form.namespace} onChange={e => setForm(f => ({ ...f, namespace: e.target.value }))} placeholder="default" />
            </div>
            <div className="form-row">
              <label>Cluster Name</label>
              <input className="input" value={form.cluster_name} onChange={e => setForm(f => ({ ...f, cluster_name: e.target.value }))} placeholder="e.g. analytics" />
            </div>
          </div>
          <div className="actions" style={{ marginTop: 12 }}>
            <button className="btn btn-approve" onClick={save} disabled={!canSave}>Save</button>
            <button className="btn btn-reject" onClick={() => { setCreating(false); setEditing(null); }}>Cancel</button>
          </div>
        </div>
      )}

      {/* List */}
      {deploymentRules.length === 0 && !showForm ? (
        <div className="empty">No deployment rules yet. Create one to deploy a profile to matching satellites.</div>
      ) : (
        <div className="dg-list">
          {deploymentRules.map(rule => {
            const count = clusterCount(rule.id);
            const isExpanded = expanded === rule.id;
            const profile = profiles.find(p => p.id === rule.profile_id);
            const spec = profile ? parseSpec(profile.config) : {};

            return (
              <div className={'dg-card' + (isExpanded ? ' dg-expanded' : '')} key={rule.id}>
                <div className="dg-header" onClick={() => toggleExpand(rule.id)}>
                  <div className="dg-header-left">
                    <span className="dg-expand-icon">{isExpanded ? '\u25bc' : '\u25b6'}</span>
                    <div>
                      <h3 className="dg-name">{rule.name}</h3>
                      <p className="muted sm">{rule.namespace}/{rule.cluster_name}</p>
                    </div>
                  </div>
                  <div className="dg-header-right">
                    <div className="dg-meta">
                      <span className="dg-meta-item">
                        <span className="dg-meta-icon">{'\u2630'}</span>
                        {profileName(rule.profile_id)}
                      </span>
                      <span className="dg-meta-item">
                        <span className="dg-meta-icon">{'\u21d2'}</span>
                        {renderSelector(rule.label_selector)}
                      </span>
                      <span className="dg-meta-item">
                        <span className="dg-meta-icon">{'\u26C1'}</span>
                        {count} cluster{count !== 1 ? 's' : ''}
                      </span>
                      {spec.replicas && (
                        <span className="dg-meta-item">
                          <span className="dg-meta-icon">{'\u2261'}</span>
                          {spec.replicas} replicas
                        </span>
                      )}
                    </div>
                    <div className="actions" onClick={e => e.stopPropagation()}>
                      <button className="btn btn-sm" onClick={() => startEdit(rule)}>Edit</button>
                      <button className="btn btn-sm btn-reject" onClick={() => remove(rule.id)}>Delete</button>
                    </div>
                  </div>
                </div>

                {isExpanded && (
                  <div className="dg-detail">
                    {/* Profile summary */}
                    {profile && (
                      <div className="dg-profile-summary">
                        <h5>Profile: {profile.name}</h5>
                        <div className="dg-profile-tags">
                          <span className="tag">PG {spec.postgres?.version || '?'}</span>
                          <span className="tag">{spec.storage?.size || '?'} data</span>
                          {spec.wal_storage && <span className="tag">{spec.wal_storage.size} WAL</span>}
                          <span className="tag">{spec.resources?.cpu_request || '?'} / {spec.resources?.cpu_limit || '?'} CPU</span>
                          <span className="tag">{spec.resources?.memory_request || '?'} / {spec.resources?.memory_limit || '?'} mem</span>
                          {spec.failover?.enabled && <span className="tag">failover</span>}
                          {spec.archive?.mode && <span className="tag">archive:{spec.archive.mode}</span>}
                          {(spec.databases || []).length > 0 && <span className="tag">{spec.databases.length} db(s)</span>}
                        </div>
                      </div>
                    )}

                    {/* Clusters created by this rule */}
                    <div className="dg-clusters">
                      <h5>Clusters ({ruleClusters.length})</h5>
                      {ruleClusters.length === 0 ? (
                        <p className="muted sm">No clusters yet. Satellites matching the label selector will receive cluster configs automatically.</p>
                      ) : (
                        <table className="dg-cluster-table">
                          <thead>
                            <tr><th>Name</th><th>Namespace</th><th>Satellite</th><th>State</th><th>Version</th><th>Updated</th></tr>
                          </thead>
                          <tbody>
                            {ruleClusters.map(cl => (
                              <tr key={cl.id}>
                                <td className="mono">{cl.name}</td>
                                <td className="muted">{cl.namespace}</td>
                                <td className="muted">{cl.satellite_id?.slice(0, 8) || '-'}</td>
                                <td><ClusterStateBadge state={cl.state} /></td>
                                <td className="muted">v{cl.config_version}</td>
                                <td className="muted">{timeAgo(cl.updated_at)}</td>
                              </tr>
                            ))}
                          </tbody>
                        </table>
                      )}
                    </div>
                  </div>
                )}
              </div>
            );
          })}
        </div>
      )}
    </>
  );
}

function ClusterStateBadge({ state }) {
  const colors = {
    running: 'badge-green',
    creating: 'badge-amber',
    degraded: 'badge-amber',
    failed: 'badge-red',
    deleting: 'badge-gray',
  };
  return (
    <span className={'badge ' + (colors[state] || 'badge-gray')}>
      <span className="dot" />{state}
    </span>
  );
}
