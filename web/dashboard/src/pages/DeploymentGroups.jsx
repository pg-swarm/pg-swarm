import { useState, useEffect } from 'react';
import { useData } from '../context/DataContext';
import { useToast } from '../context/ToastContext';
import { api, parseSpec, timeAgo } from '../api';

export default function DeploymentGroups() {
  const { deploymentGroups, profiles, clusters, refresh } = useData();
  const toast = useToast();
  const [creating, setCreating] = useState(false);
  const [editing, setEditing] = useState(null);
  const [expanded, setExpanded] = useState(null); // deployment group id to show detail
  const [dgClusters, setDgClusters] = useState([]); // clusters for expanded group

  const [form, setForm] = useState({ name: '', description: '', profile_id: '' });

  function profileName(pid) {
    const p = profiles.find(p => p.id === pid);
    return p ? p.name : pid?.slice(0, 8) || '-';
  }

  function clusterCount(dgId) {
    return clusters.filter(c => c.deployment_group_id === dgId).length;
  }

  function startCreate() {
    setForm({ name: '', description: '', profile_id: profiles[0]?.id || '' });
    setCreating(true);
    setEditing(null);
  }

  function startEdit(dg) {
    setForm({ name: dg.name, description: dg.description, profile_id: dg.profile_id });
    setEditing(dg.id);
    setCreating(false);
  }

  async function save() {
    const payload = { name: form.name, description: form.description, profile_id: form.profile_id };
    try {
      if (editing) {
        await api.updateDeploymentGroup(editing, payload);
        toast('Deployment group updated');
      } else {
        await api.createDeploymentGroup(payload);
        toast('Deployment group created');
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
      await api.deleteDeploymentGroup(id);
      toast('Deployment group deleted');
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
      const cls = await api.deploymentGroupClusters(id);
      setDgClusters(cls || []);
      setExpanded(id);
    } catch (e) {
      toast('Failed to load clusters: ' + e.message, true);
    }
  }

  const showForm = creating || editing;

  return (
    <>
      <div className="card-head-bar">
        <span className="card-head-title">Deployment Groups</span>
        <button className="btn btn-approve" onClick={startCreate}>+ New Group</button>
      </div>

      <p className="muted sm" style={{ marginBottom: 16 }}>
        Deployment groups link clusters to a shared profile configuration. All clusters in a group use the same settings.
      </p>

      {/* Create / Edit form */}
      {showForm && (
        <div className="dg-form-card">
          <h4>{editing ? 'Edit Group' : 'Create Group'}</h4>
          <div className="form-grid">
            <div className="form-row">
              <label>Name</label>
              <input className="input" value={form.name} onChange={e => setForm(f => ({ ...f, name: e.target.value }))} placeholder="e.g. production-edge" />
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
            <div className="form-row" style={{ gridColumn: '1 / -1' }}>
              <label>Description</label>
              <input className="input" value={form.description} onChange={e => setForm(f => ({ ...f, description: e.target.value }))} placeholder="Optional description" />
            </div>
          </div>
          <div className="actions" style={{ marginTop: 12 }}>
            <button className="btn btn-approve" onClick={save} disabled={!form.name.trim() || !form.profile_id}>Save</button>
            <button className="btn btn-reject" onClick={() => { setCreating(false); setEditing(null); }}>Cancel</button>
          </div>
        </div>
      )}

      {/* List */}
      {deploymentGroups.length === 0 && !showForm ? (
        <div className="empty">No deployment groups yet. Create one to start grouping clusters by profile.</div>
      ) : (
        <div className="dg-list">
          {deploymentGroups.map(dg => {
            const count = clusterCount(dg.id);
            const isExpanded = expanded === dg.id;
            const profile = profiles.find(p => p.id === dg.profile_id);
            const spec = profile ? parseSpec(profile.config) : {};

            return (
              <div className={'dg-card' + (isExpanded ? ' dg-expanded' : '')} key={dg.id}>
                <div className="dg-header" onClick={() => toggleExpand(dg.id)}>
                  <div className="dg-header-left">
                    <span className="dg-expand-icon">{isExpanded ? '\u25bc' : '\u25b6'}</span>
                    <div>
                      <h3 className="dg-name">{dg.name}</h3>
                      {dg.description && <p className="muted sm">{dg.description}</p>}
                    </div>
                  </div>
                  <div className="dg-header-right">
                    <div className="dg-meta">
                      <span className="dg-meta-item">
                        <span className="dg-meta-icon">{'\u2630'}</span>
                        {profileName(dg.profile_id)}
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
                      <button className="btn btn-sm" onClick={() => startEdit(dg)}>Edit</button>
                      <button className="btn btn-sm btn-reject" onClick={() => remove(dg.id)}>Delete</button>
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

                    {/* Clusters in this group */}
                    <div className="dg-clusters">
                      <h5>Clusters ({dgClusters.length})</h5>
                      {dgClusters.length === 0 ? (
                        <p className="muted sm">No clusters in this group yet. Assign clusters via the Clusters page.</p>
                      ) : (
                        <table className="dg-cluster-table">
                          <thead>
                            <tr><th>Name</th><th>Namespace</th><th>State</th><th>Version</th><th>Updated</th></tr>
                          </thead>
                          <tbody>
                            {dgClusters.map(cl => (
                              <tr key={cl.id}>
                                <td className="mono">{cl.name}</td>
                                <td className="muted">{cl.namespace}</td>
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
