import { useState } from 'react';
import { useData } from '../context/DataContext';
import { ClusterBadge } from '../components/Badge';
import { api, parseSpec, timeAgo } from '../api';

export default function Clusters() {
  const { clusters, satellites, health, events, refresh } = useData();
  const [busy, setBusy] = useState(null);
  const [expanded, setExpanded] = useState({});

  if (clusters.length === 0) {
    return <div className="empty">No clusters configured</div>;
  }

  async function togglePause(c) {
    setBusy(c.id);
    try {
      if (c.paused) {
        await api.resumeCluster(c.id);
      } else {
        await api.pauseCluster(c.id);
      }
      refresh();
    } catch (e) {
      alert(e.message);
    } finally {
      setBusy(null);
    }
  }

  async function doSwitchover(clusterId, targetPod) {
    if (!confirm(`Promote ${targetPod} to primary? This will demote the current primary.`)) return;
    setBusy(clusterId);
    try {
      await api.switchover(clusterId, targetPod);
      refresh();
    } catch (e) {
      alert('Switchover failed: ' + e.message);
    } finally {
      setBusy(null);
    }
  }

  function toggleExpand(id) {
    setExpanded(prev => ({ ...prev, [id]: !prev[id] }));
  }

  return (
    <div className="cluster-grid">
      {clusters.map(c => {
        const s = parseSpec(c.config);
        const sat = satellites.find(x => x.id === c.satellite_id);
        const h = health.find(x => x.cluster_name === c.name && x.satellite_id === c.satellite_id);
        const clusterEvents = (events || [])
          .filter(e => e.cluster_name === c.name && e.satellite_id === c.satellite_id)
          .slice(0, 3);

        const tags = [];
        if (c.paused) tags.push('paused');
        if (s.failover?.enabled) tags.push('failover');
        if (s.archive?.mode) tags.push('archive:' + s.archive.mode);
        if (s.databases?.length) tags.push(s.databases.length + ' db' + (s.databases.length > 1 ? 's' : ''));

        const instances = h?.instances || [];
        const isExpanded = expanded[c.id];
        const hasFailover = s.failover?.enabled;

        return (
          <div className={`cl-card${c.paused ? ' cl-paused' : ''}`} key={c.id}>
            <div className="cl-head">
              <h3>{c.name}</h3>
              <div className="badges">
                {h && <ClusterBadge state={h.state} />}
                {(!h || h.state !== c.state) && <ClusterBadge state={c.state} />}
              </div>
            </div>
            <div className="cl-body">
              <dl className="cl-grid">
                <KV label="Namespace" value={c.namespace || 'default'} />
                <KV label="Replicas" value={s.replicas || '-'} />
                <KV label="PostgreSQL" value={s.postgres?.version || '-'} />
                <KV label="Storage" value={s.storage?.size || '-'} />
                <KV label="CPU" value={`${s.resources?.cpu_request || '-'} / ${s.resources?.cpu_limit || '-'}`} />
                <KV label="Memory" value={`${s.resources?.memory_request || '-'} / ${s.resources?.memory_limit || '-'}`} />
              </dl>
              {tags.length > 0 && (
                <div className="cl-tags">
                  {tags.map(t => <span className={`tag${t === 'paused' ? ' tag-warn' : ''}`} key={t}>{t}</span>)}
                </div>
              )}
            </div>

            {instances.length > 0 && (
              <div className="cl-instances">
                <div className="cl-instances-toggle" onClick={() => toggleExpand(c.id)}>
                  <span className="cl-instances-arrow">{isExpanded ? '\u25BC' : '\u25B6'}</span>
                  <span>{instances.length} member{instances.length !== 1 ? 's' : ''}</span>
                  <InstanceSummary instances={instances} />
                </div>
                {isExpanded && (
                  <table className="instance-table">
                    <thead>
                      <tr>
                        <th>Pod</th>
                        <th>Role</th>
                        <th>Status</th>
                        <th>Lag</th>
                        <th>Conns</th>
                        <th>Disk</th>
                        <th>TL</th>
                        {hasFailover && <th></th>}
                      </tr>
                    </thead>
                    <tbody>
                      {instances.map(inst => (
                        <tr key={inst.pod_name} className={inst.error_message ? 'inst-error' : ''}>
                          <td className="mono">{inst.pod_name}</td>
                          <td><RoleBadge role={inst.role} /></td>
                          <td>
                            <ReadyDot ready={inst.ready} />
                            {inst.role === 'replica' && (
                              <WalDot active={inst.wal_receiver_active} />
                            )}
                          </td>
                          <td className="mono">
                            {inst.role === 'replica' && inst.replication_lag_seconds > 0
                              ? formatLagTime(inst.replication_lag_seconds)
                              : formatLag(inst.replication_lag_bytes)}
                          </td>
                          <td>
                            {inst.connections_max > 0
                              ? <ConnBar used={inst.connections_used} max={inst.connections_max} />
                              : '-'}
                          </td>
                          <td className="mono">{formatDisk(inst.disk_used_bytes)}</td>
                          <td className="mono muted">{inst.timeline_id || '-'}</td>
                          {hasFailover && (
                            <td>
                              {inst.role === 'replica' && inst.ready && (
                                <button
                                  className="btn-sm btn-switchover"
                                  onClick={() => doSwitchover(c.id, inst.pod_name)}
                                  disabled={busy === c.id}
                                  title="Promote to primary"
                                >
                                  {busy === c.id ? '...' : 'Promote'}
                                </button>
                              )}
                            </td>
                          )}
                        </tr>
                      ))}
                    </tbody>
                    {instances.some(i => i.error_message) && (
                      <tfoot>
                        {instances.filter(i => i.error_message).map(i => (
                          <tr key={i.pod_name + '-err'}>
                            <td colSpan={hasFailover ? 8 : 7} className="inst-error-msg">
                              <span className="mono">{i.pod_name}</span>: {i.error_message}
                            </td>
                          </tr>
                        ))}
                      </tfoot>
                    )}
                  </table>
                )}
              </div>
            )}

            {clusterEvents.length > 0 && (
              <div className="cl-events">
                {clusterEvents.map((evt, i) => (
                  <div className="cl-event-row" key={evt.id || i}>
                    <span className={`sev-${evt.severity}`}>{evt.severity}</span>
                    <span className="cl-event-msg">{evt.message}</span>
                    <span className="cl-event-time muted">{timeAgo(evt.created_at)}</span>
                  </div>
                ))}
              </div>
            )}

            <div className="cl-foot">
              <span>{sat ? sat.hostname : (c.satellite_id ? c.satellite_id.substring(0, 8) : 'unassigned')}</span>
              <span>v{c.config_version}</span>
              <span>{timeAgo(c.updated_at)}</span>
              <button
                className={`btn-sm ${c.paused ? 'btn-resume' : 'btn-pause'}`}
                onClick={() => togglePause(c)}
                disabled={busy === c.id}
              >
                {busy === c.id ? '...' : (c.paused ? 'Resume' : 'Pause')}
              </button>
            </div>
          </div>
        );
      })}
    </div>
  );
}

function KV({ label, value }) {
  return (
    <div>
      <dt>{label}</dt>
      <dd>{String(value)}</dd>
    </div>
  );
}

function InstanceSummary({ instances }) {
  const primary = instances.filter(i => i.role === 'primary' && i.ready).length;
  const replicas = instances.filter(i => i.role === 'replica' && i.ready).length;
  const down = instances.filter(i => !i.ready).length;
  return (
    <span className="inst-summary">
      {primary > 0 && <span className="inst-sum-ok">{primary}P</span>}
      {replicas > 0 && <span className="inst-sum-ok">{replicas}R</span>}
      {down > 0 && <span className="inst-sum-down">{down} down</span>}
    </span>
  );
}

function RoleBadge({ role }) {
  const colors = { primary: 'badge-green', replica: 'badge-gray', failed_primary: 'badge-red' };
  const cls = colors[role] || 'badge-gray';
  return <span className={`badge ${cls}`}><span className="dot"></span>{role || 'unknown'}</span>;
}

function ReadyDot({ ready }) {
  return <span className={`online-dot ${ready ? 'dot-green' : 'dot-red'}`} title={ready ? 'Ready' : 'Not ready'}></span>;
}

function WalDot({ active }) {
  return <span className={`online-dot ${active ? 'dot-green' : 'dot-amber'}`} title={active ? 'WAL streaming' : 'WAL disconnected'} style={{ marginLeft: 2 }}></span>;
}

function ConnBar({ used, max }) {
  const pct = max > 0 ? (used / max) * 100 : 0;
  const color = pct > 90 ? 'var(--red)' : pct > 75 ? 'var(--amber)' : 'var(--green)';
  return (
    <span className="conn-bar" title={`${used} / ${max} connections`}>
      <span className="conn-fill" style={{ width: Math.min(pct, 100) + '%', background: color }}></span>
      <span className="conn-label">{used}/{max}</span>
    </span>
  );
}

function formatLag(bytes) {
  if (!bytes && bytes !== 0) return '-';
  if (bytes === 0) return '0 B';
  if (bytes < 1024) return bytes + ' B';
  if (bytes < 1024 * 1024) return (bytes / 1024).toFixed(1) + ' KB';
  return (bytes / (1024 * 1024)).toFixed(1) + ' MB';
}

function formatLagTime(seconds) {
  if (!seconds || seconds <= 0) return '0s';
  if (seconds < 60) return seconds.toFixed(1) + 's';
  if (seconds < 3600) return (seconds / 60).toFixed(1) + 'm';
  return (seconds / 3600).toFixed(1) + 'h';
}

function formatDisk(bytes) {
  if (!bytes) return '-';
  if (bytes < 1024 * 1024) return (bytes / 1024).toFixed(0) + ' KB';
  if (bytes < 1024 * 1024 * 1024) return (bytes / (1024 * 1024)).toFixed(1) + ' MB';
  return (bytes / (1024 * 1024 * 1024)).toFixed(2) + ' GB';
}
