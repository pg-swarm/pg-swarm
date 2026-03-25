import { useState, useEffect } from 'react';
import { useData } from '../context/DataContext';
import { ClusterBadge } from '../components/Badge';
import { api, parseSpec, timeAgo } from '../api';
import {
  Server, Crown, Copy, Shield,
  Pause, Play, Database, AlertCircle,
  ExternalLink, Search
} from 'lucide-react';

/* ── Format helpers (shared with ClusterDetail) ──────── */

export function formatLag(bytes) {
  if (!bytes && bytes !== 0) return '-';
  if (bytes === 0) return '0 B';
  if (bytes < 1024) return bytes + ' B';
  if (bytes < 1024 * 1024) return (bytes / 1024).toFixed(1) + ' KB';
  return (bytes / (1024 * 1024)).toFixed(1) + ' MB';
}

export function formatLagTime(seconds) {
  if (!seconds || seconds <= 0) return '0s';
  if (seconds < 60) return seconds.toFixed(1) + 's';
  if (seconds < 3600) return (seconds / 60).toFixed(1) + 'm';
  return (seconds / 3600).toFixed(1) + 'h';
}

export function formatDisk(bytes) {
  if (!bytes) return '-';
  if (bytes < 1024 * 1024) return (bytes / 1024).toFixed(0) + ' KB';
  if (bytes < 1024 * 1024 * 1024) return (bytes / (1024 * 1024)).toFixed(1) + ' MB';
  return (bytes / (1024 * 1024 * 1024)).toFixed(2) + ' GB';
}

export function formatMs(ms) {
  if (ms == null) return '-';
  if (ms < 1) return ms.toFixed(3) + ' ms';
  if (ms < 1000) return ms.toFixed(1) + ' ms';
  if (ms < 60000) return (ms / 1000).toFixed(2) + ' s';
  return (ms / 60000).toFixed(1) + ' min';
}

export function formatNumber(n) {
  if (n == null) return '-';
  if (n >= 1_000_000_000) return (n / 1_000_000_000).toFixed(1) + 'B';
  if (n >= 1_000_000) return (n / 1_000_000).toFixed(1) + 'M';
  if (n >= 10_000) return (n / 1_000).toFixed(1) + 'K';
  return n.toLocaleString();
}

export function truncateQuery(q) {
  if (!q) return '-';
  const clean = q.replace(/\s+/g, ' ').trim();
  return clean.length > 80 ? clean.substring(0, 77) + '...' : clean;
}

export function parseStorageSize(spec) {
  if (!spec) return 0;
  const m = spec.match(/^(\d+(?:\.\d+)?)\s*(Ti|Gi|Mi|Ki|T|G|M|K)?/i);
  if (!m) return 0;
  const n = parseFloat(m[1]);
  const unit = (m[2] || '').toLowerCase();
  const multipliers = { ti: 1099511627776, gi: 1073741824, mi: 1048576, ki: 1024, t: 1e12, g: 1e9, m: 1e6, k: 1e3 };
  return n * (multipliers[unit] || 1);
}

/* ── Shared presentational components ────────────────── */

export function RoleBadge({ role }) {
  const colors = { primary: 'badge-green', replica: 'badge-gray', failed_primary: 'badge-red' };
  const icons = { primary: Crown, replica: Copy, failed_primary: AlertCircle };
  const cls = colors[role] || 'badge-gray';
  const Icon = icons[role] || Shield;
  return <span className={`badge ${cls}`}><Icon size={11} />{role || 'unknown'}</span>;
}

export function PgStatusDot({ inst }) {
  if (!inst.ready) {
    return <span className="online-dot dot-red dot-blink" title="Not ready"></span>;
  }
  if (!inst.pg_start_time) {
    return <span className="online-dot dot-green" title="Ready"></span>;
  }
  return <span className="online-dot dot-green" title={`PG up since ${new Date(inst.pg_start_time).toLocaleString()}`}></span>;
}

export function LagDot({ inst }) {
  const lagSec = inst.replication_lag_seconds || 0;
  if (lagSec > 180) {
    return <span className="online-dot dot-red dot-blink" title={`Lag: ${formatLagTime(lagSec)} (critical)`} style={{ marginLeft: 2 }}></span>;
  }
  if (lagSec > 60) {
    return <span className="online-dot dot-amber" title={`Lag: ${formatLagTime(lagSec)} (warning)`} style={{ marginLeft: 2 }}></span>;
  }
  if (!inst.wal_receiver_active) {
    return <span className="online-dot dot-amber" title="WAL disconnected" style={{ marginLeft: 2 }}></span>;
  }
  return <span className="online-dot dot-green" title={`Lag: ${formatLagTime(lagSec)}`} style={{ marginLeft: 2 }}></span>;
}

export function ConnBar({ used, max }) {
  const pct = max > 0 ? (used / max) * 100 : 0;
  const color = pct > 90 ? 'var(--red)' : pct > 75 ? 'var(--amber)' : 'var(--green)';
  return (
    <span className="conn-bar" title={`${used} / ${max} connections`}>
      <span className="conn-fill" style={{ width: Math.min(pct, 100) + '%', background: color }}></span>
      <span className="conn-label">{used}/{max}</span>
    </span>
  );
}

export function InstanceSummary({ instances }) {
  const primary = instances.filter(i => i.role === 'primary' && i.ready).length;
  const replicas = instances.filter(i => i.role === 'replica' && i.ready).length;
  const down = instances.filter(i => !i.ready).length;
  return (
    <span className="inst-summary">
      {primary > 0 && <span className="inst-sum-ok"><Crown size={11} /> {primary}P</span>}
      {replicas > 0 && <span className="inst-sum-ok"><Copy size={11} /> {replicas}R</span>}
      {down > 0 && <span className="inst-sum-down"><AlertCircle size={11} /> {down} down</span>}
    </span>
  );
}

export function KV({ label, value }) {
  return (
    <div>
      <dt>{label}</dt>
      <dd>{String(value)}</dd>
    </div>
  );
}

export function CacheHitBadge({ pct }) {
  const color = pct >= 99 ? 'var(--green)' : pct >= 95 ? 'var(--amber)' : 'var(--red)';
  return (
    <span className="cache-hit-badge" style={{ color }}>
      {pct.toFixed(1)}%
    </span>
  );
}

export function DiskBar({ label, bytes, total, color }) {
  const pct = total > 0 ? (bytes / total) * 100 : 0;
  return (
    <div className="disk-bar-row">
      <span className="report-label">{label}</span>
      <span className="disk-bar">
        <span className="disk-bar-fill" style={{ width: Math.min(pct, 100) + '%', background: color }}></span>
      </span>
      <span className="mono disk-bar-value">{formatDisk(bytes)}</span>
      <span className="mono disk-pct">{pct.toFixed(1)}%</span>
    </div>
  );
}

export function ReportRow({ label, value }) {
  return (
    <div className="report-row">
      <span className="report-label">{label}</span>
      <span className="report-value">{String(value)}</span>
    </div>
  );
}

/* ── Node table for cluster card ──────────────────────── */

function pctColor(pct) {
  return pct > 75 ? 'var(--red)' : pct > 50 ? 'var(--amber)' : 'var(--green)';
}
function pctBg(pct) {
  return pct > 75 ? 'var(--red-bg)' : pct > 50 ? 'var(--amber-bg)' : null;
}

function NodeTable({ instances, storageBytes }) {
  return (
    <table className="node-table">
      <thead>
        <tr>
          <th></th>
          <th>Instance</th>
          <th>Lag</th>
          <th>Disk</th>
          <th>Conn</th>
        </tr>
      </thead>
      <tbody>
        {instances.map(inst => {
          const lagSec = inst.replication_lag_seconds || 0;
          const connPct = inst.connections_max > 0 ? Math.round((inst.connections_used / inst.connections_max) * 100) : 0;
          const diskPct = storageBytes > 0 ? Math.round((inst.disk_used_bytes / storageBytes) * 100) : 0;

          const lagDot = lagSec > 180 ? 'var(--red)' : lagSec > 60 ? 'var(--amber)' : 'var(--green)';
          const RoleIcon = inst.role === 'primary' ? Crown : inst.role === 'replica' ? Copy : AlertCircle;
          const roleColor = inst.role === 'primary' ? 'var(--amber)' : inst.role === 'failed_primary' ? 'var(--red)' : 'var(--blue)';

          // Highlight row if any metric is amber or red
          const worstBg = pctBg(Math.max(diskPct, connPct));

          return (
            <tr key={inst.pod_name} style={worstBg ? { background: worstBg } : undefined}>
              <td title={inst.role === 'primary' ? 'Primary' : inst.role === 'replica' ? 'Replica' : 'Failed Primary'}><RoleIcon size={11} style={{ color: roleColor }} /></td>
              <td className="mono">{inst.pod_name}</td>
              <td>
                {inst.role === 'replica'
                  ? <span className={`online-dot${lagSec > 180 ? ' dot-blink' : ''}`} style={{ background: lagDot }} title={formatLagTime(lagSec)} />
                  : <span className="muted">-</span>}
              </td>
              <td>
                <span className="node-bar" title={`${diskPct}%`}>
                  <span className="node-bar-fill" style={{ width: Math.min(diskPct, 100) + '%', background: pctColor(diskPct) }} />
                </span>
              </td>
              <td>
                <span className="node-bar" title={`${connPct}%`}>
                  <span className="node-bar-fill" style={{ width: Math.min(connPct, 100) + '%', background: pctColor(connPct) }} />
                </span>
              </td>
            </tr>
          );
        })}
      </tbody>
    </table>
  );
}

/* ── Clusters page ───────────────────────────────────── */

export default function Clusters() {
  const { clusters, satellites, health, deploymentRules, profiles, refresh } = useData();
  const [busy, setBusy] = useState(null);
  const [search, setSearch] = useState('');
  const [profileLatestVersions, setProfileLatestVersions] = useState({});

  useEffect(() => { document.title = 'Clusters - PG-Swarm'; }, []);

  // Load latest profile version for each profile used by clusters
  useEffect(() => {
    const profileIds = [...new Set(clusters.filter(c => c.profile_id).map(c => c.profile_id))];
    Promise.all(profileIds.map(pid =>
      api.profileVersions(pid).then(vs => [pid, vs.length > 0 ? vs[0].version : 0]).catch(() => [pid, 0])
    )).then(results => {
      const map = {};
      results.forEach(([pid, v]) => { map[pid] = v; });
      setProfileLatestVersions(map);
    });
  }, [clusters]);

  if (clusters.length === 0) {
    return (
      <div className="empty-state">
        <Database size={48} strokeWidth={1.2} />
        <h3>No clusters configured</h3>
        <p>Create a deployment rule to deploy clusters to your satellites.</p>
      </div>
    );
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

  function getLabelsForCluster(c) {
    if (!c.deployment_rule_id || !deploymentRules) return {};
    const rule = deploymentRules.find(r => r.id === c.deployment_rule_id);
    return rule?.label_selector || {};
  }

  const term = search.toLowerCase().trim();

  const filtered = term
    ? clusters.filter(c => {
        const sat = satellites.find(x => x.id === c.satellite_id);
        const labels = getLabelsForCluster(c);
        const labelText = Object.entries(labels).map(([k, v]) => k + '=' + v).join(' ').toLowerCase();

        return (
          (c.name || '').toLowerCase().includes(term) ||
          (c.namespace || '').toLowerCase().includes(term) ||
          (sat?.hostname || '').toLowerCase().includes(term) ||
          labelText.includes(term)
        );
      })
    : clusters;

  return (
    <>
      <div className="search-bar-sticky">
        <Search size={15} style={{
          position: 'absolute', left: 10, top: '50%', transform: 'translateY(-50%)',
          color: 'var(--text-secondary)', pointerEvents: 'none',
        }} />
        <input
          className="input"
          type="text"
          placeholder="Search by cluster name, namespace, satellite, or label..."
          value={search}
          onChange={e => setSearch(e.target.value)}
          style={{ paddingLeft: 32, maxWidth: 480 }}
        />
      </div>

      <div className="cluster-grid" style={{ gridTemplateColumns: 'repeat(3, 1fr)' }}>
        {filtered.map(c => {
          const s = parseSpec(c.config);
          const sat = satellites.find(x => x.id === c.satellite_id);
          const h = health.find(x => x.cluster_name === c.name && x.satellite_id === c.satellite_id);
          const instances = h?.instances || [];
          const labels = getLabelsForCluster(c);
          const labelEntries = Object.entries(labels);

          const errorInstances = instances.filter(i => i.error_message);

          return (
            <div className={`cl-card${c.paused ? ' cl-paused' : ''}`} key={c.id}>
              {/* Header */}
              <div className="cl-head" style={{ flexWrap: 'wrap', gap: 6 }}>
                <div className="cl-head-left">
                  <Database size={16} className="cl-head-icon" />
                  <h3>{c.name}</h3>
                  <span className="mono muted" style={{ fontSize: 11, fontWeight: 400 }}>{c.namespace || 'default'}</span>
                  <span className="muted" style={{ fontSize: 11, fontWeight: 400 }}>@ {sat ? sat.hostname : (c.satellite_id ? c.satellite_id.substring(0, 8) : 'unassigned')}</span>
                </div>
                <div style={{ width: '100%', display: 'flex', gap: 4, flexWrap: 'wrap', alignItems: 'center', marginTop: 2 }}>
                  {labelEntries.map(([k, v]) => (
                    <span key={k} style={{
                      background: 'var(--blue-bg)', color: 'var(--blue)',
                      padding: '1px 7px', borderRadius: 4, fontSize: 10.5,
                      fontWeight: 500, whiteSpace: 'nowrap',
                    }}>
                      {k}={v}
                    </span>
                  ))}
                  <span style={{ marginLeft: 'auto' }} />
                  {c.profile_id && profileLatestVersions[c.profile_id] > (c.applied_profile_version || 0) && (
                    <span className="badge badge-amber" title="Profile has been updated. Click to view and apply changes.">
                      <span className="dot" />Update Available
                    </span>
                  )}
                  {h && <ClusterBadge state={h.state} />}
                  {(!h || h.state !== c.state) && <ClusterBadge state={c.state} />}
                </div>
              </div>

              {/* Body */}
              <div className="cl-body">
                <dl className="cl-grid cl-grid-3">
                  <KV label="Replicas" value={s.replicas || '-'} />
                  <KV label="PostgreSQL" value={s.postgres?.version || '-'} />
                  <KV label="Storage" value={s.storage?.size || '-'} />
                </dl>

                {instances.length > 0 && (
                  <div className="node-list">
                    <NodeTable instances={instances} storageBytes={parseStorageSize(s.storage?.size)} />
                  </div>
                )}

                {instances.length === 0 && c.state === 'creating' && (
                  <div style={{ fontSize: 11.5, color: 'var(--text-secondary)', marginTop: 10 }}>Waiting for pods...</div>
                )}
              </div>

              {/* Footer */}
              <div className="cl-foot">
                <span style={{ marginRight: 'auto' }} />
                <button
                  className={`btn-sm btn-icon-text ${c.paused ? 'btn-resume' : 'btn-pause'}`}
                  onClick={() => togglePause(c)}
                  disabled={busy === c.id}
                >
                  {c.paused ? <Play size={12} /> : <Pause size={12} />}
                  {busy === c.id ? '...' : (c.paused ? 'Resume' : 'Pause')}
                </button>
                <button
                  className="btn-sm"
                  onClick={() => window.open('/clusters/' + c.id, '_blank')}
                  style={{ display: 'inline-flex', alignItems: 'center', gap: 4 }}
                >
                  <ExternalLink size={11} />
                  Details
                </button>
              </div>
            </div>
          );
        })}
      </div>

      {term && filtered.length === 0 && (
        <div className="empty" style={{ padding: 40 }}>
          No clusters match "{search}"
        </div>
      )}

    </>
  );
}
