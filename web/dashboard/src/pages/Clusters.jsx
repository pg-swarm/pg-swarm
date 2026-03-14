import { useState } from 'react';
import { useData } from '../context/DataContext';
import { ClusterBadge } from '../components/Badge';
import { api, parseSpec, timeAgo } from '../api';
import {
  ChevronDown, ChevronRight, Server, Crown, Copy, Shield,
  ArrowUpRight, Pause, Play, HardDrive, BarChart3, Zap,
  Table2, SearchCode, ArrowLeft, X, Info, AlertTriangle,
  AlertCircle, Flame, Database, Activity
} from 'lucide-react';

export default function Clusters() {
  const { clusters, satellites, health, events, refresh } = useData();
  const [busy, setBusy] = useState(null);
  const [expanded, setExpanded] = useState({});
  const [eventsExpanded, setEventsExpanded] = useState({});
  const [detailInst, setDetailInst] = useState(null);
  const [switchoverTarget, setSwitchoverTarget] = useState(null);

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

  function requestSwitchover(clusterId, targetPod, instances) {
    const currentPrimary = instances.find(i => i.role === 'primary');
    setSwitchoverTarget({ clusterId, targetPod, currentPrimary: currentPrimary?.pod_name });
  }

  async function confirmSwitchover() {
    if (!switchoverTarget) return;
    const { clusterId, targetPod } = switchoverTarget;
    setSwitchoverTarget(null);
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

  function toggleEvents(id) {
    setEventsExpanded(prev => ({ ...prev, [id]: !prev[id] }));
  }

  const SEV_ICONS = { info: Info, warning: AlertTriangle, error: AlertCircle, critical: Flame };

  return (
    <>
    <div className="cluster-grid">
      {clusters.map(c => {
        const s = parseSpec(c.config);
        const sat = satellites.find(x => x.id === c.satellite_id);
        const h = health.find(x => x.cluster_name === c.name && x.satellite_id === c.satellite_id);
        const clusterEvents = (events || [])
          .filter(e => e.cluster_name === c.name && e.satellite_id === c.satellite_id)
          .slice(0, 5);

        const tags = [];
        if (c.paused) tags.push('paused');
        if (s.failover?.enabled) tags.push('failover');
        if (s.archive?.mode) tags.push('archive:' + s.archive.mode);
        if (s.databases?.length) tags.push(s.databases.length + ' db' + (s.databases.length > 1 ? 's' : ''));

        const instances = h?.instances || [];
        const isExpanded = expanded[c.id];
        const isEventsExpanded = eventsExpanded[c.id];
        const hasFailover = s.failover?.enabled;

        return (
          <div className={`cl-card${c.paused ? ' cl-paused' : ''}`} key={c.id}>
            <div className="cl-head">
              <div className="cl-head-left">
                <Database size={16} className="cl-head-icon" />
                <h3>{c.name}</h3>
              </div>
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
                  {isExpanded
                    ? <ChevronDown size={14} className="cl-toggle-icon" />
                    : <ChevronRight size={14} className="cl-toggle-icon" />}
                  <Server size={13} />
                  <span>{instances.length} member{instances.length !== 1 ? 's' : ''}</span>
                  <InstanceSummary instances={instances} />
                </div>
                {isExpanded && (
                  <div className="instance-table-wrap">
                    <table className="instance-table">
                      <thead>
                        <tr>
                          <th>Instance</th>
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
                          <tr
                            key={inst.pod_name}
                            className={`inst-row${inst.error_message ? ' inst-error' : ''}`}
                            onClick={() => setDetailInst({ inst, storage: s.storage?.size })}
                            title="Click for details"
                          >
                            <td className="mono">{inst.pod_name}</td>
                            <td><RoleBadge role={inst.role} /></td>
                            <td>
                              <PgStatusDot inst={inst} />
                              {inst.role === 'replica' && (
                                <LagDot inst={inst} />
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
                                    onClick={(e) => { e.stopPropagation(); requestSwitchover(c.id, inst.pod_name, instances); }}
                                    disabled={busy === c.id}
                                    title="Promote to primary"
                                  >
                                    <ArrowUpRight size={11} />
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
                  </div>
                )}
              </div>
            )}

            {clusterEvents.length > 0 && (
              <div className="cl-events">
                <div className="cl-events-toggle" onClick={() => toggleEvents(c.id)}>
                  {isEventsExpanded
                    ? <ChevronDown size={14} className="cl-toggle-icon" />
                    : <ChevronRight size={14} className="cl-toggle-icon" />}
                  <Activity size={13} />
                  <span>{clusterEvents.length} event{clusterEvents.length !== 1 ? 's' : ''}</span>
                </div>
                {isEventsExpanded && clusterEvents.map((evt, i) => {
                  const SevIcon = SEV_ICONS[evt.severity] || Info;
                  return (
                    <div className="cl-event-row" key={evt.id || i}>
                      <span className={`sev-pill sev-${evt.severity}`}>
                        <SevIcon size={11} />
                        {evt.severity}
                      </span>
                      <span className="cl-event-msg">{evt.message}</span>
                      <span className="cl-event-time muted">{timeAgo(evt.created_at)}</span>
                    </div>
                  );
                })}
              </div>
            )}

            <div className="cl-foot">
              <span className="cl-foot-sat">
                <Server size={12} />
                {sat ? sat.hostname : (c.satellite_id ? c.satellite_id.substring(0, 8) : 'unassigned')}
              </span>
              <span>v{c.config_version}</span>
              <span title={c.created_at ? new Date(c.created_at).toLocaleString() : ''}>created {timeAgo(c.created_at)}</span>
              <span>{timeAgo(c.updated_at)}</span>
              <button
                className={`btn-sm btn-icon-text ${c.paused ? 'btn-resume' : 'btn-pause'}`}
                onClick={() => togglePause(c)}
                disabled={busy === c.id}
              >
                {c.paused ? <Play size={12} /> : <Pause size={12} />}
                {busy === c.id ? '...' : (c.paused ? 'Resume' : 'Pause')}
              </button>
            </div>
          </div>
        );
      })}
    </div>
    {detailInst && <InstanceDetailModal inst={detailInst.inst} storageSpec={detailInst.storage} onClose={() => setDetailInst(null)} />}
    {switchoverTarget && (
      <SwitchoverConfirmModal
        target={switchoverTarget}
        onConfirm={confirmSwitchover}
        onCancel={() => setSwitchoverTarget(null)}
      />
    )}
    </>
  );
}

function SwitchoverConfirmModal({ target, onConfirm, onCancel }) {
  return (
    <div className="confirm-overlay" onClick={onCancel}>
      <div className="confirm-modal switchover-modal" onClick={e => e.stopPropagation()}>
        <div className="confirm-header switchover-header">
          <h3><AlertTriangle size={18} className="switchover-warn-icon" /> Planned Switchover</h3>
          <button className="modal-close" onClick={onCancel}><X size={18} /></button>
        </div>
        <div className="confirm-body">
          <div className="switchover-detail">
            <div className="switchover-flow">
              <div className="switchover-node">
                <Crown size={16} className="switchover-icon-primary" />
                <span className="switchover-label">Current Primary</span>
                <span className="mono switchover-pod">{target.currentPrimary || 'unknown'}</span>
                <span className="switchover-action">will be demoted to replica</span>
              </div>
              <div className="switchover-arrow">
                <ArrowUpRight size={20} />
              </div>
              <div className="switchover-node">
                <ArrowUpRight size={16} className="switchover-icon-promote" />
                <span className="switchover-label">New Primary</span>
                <span className="mono switchover-pod">{target.targetPod}</span>
                <span className="switchover-action">will be promoted to primary</span>
              </div>
            </div>
          </div>
          <div className="switchover-steps">
            <h5>What will happen:</h5>
            <ol>
              <li>A CHECKPOINT will be issued on the current primary to flush all WAL</li>
              <li>The current primary will be fenced (writes blocked, client connections terminated)</li>
              <li>The leader lease will be transferred to <strong>{target.targetPod}</strong></li>
              <li>The target replica will be promoted via <code>pg_promote()</code></li>
              <li>The old primary will automatically demote to a replica</li>
            </ol>
          </div>
          <div className="switchover-warning">
            <AlertTriangle size={14} />
            <span>Applications connected to the database will experience a brief downtime during the switchover. New connections will be routed to the new primary once promotion is complete.</span>
          </div>
        </div>
        <div className="confirm-footer">
          <button className="btn-sm" onClick={onCancel}>Cancel</button>
          <button className="btn-sm btn-danger" onClick={onConfirm}>Confirm Switchover</button>
        </div>
      </div>
    </div>
  );
}

function InstanceDetailModal({ inst, storageSpec, onClose }) {
  const [selectedDb, setSelectedDb] = useState(null);
  const databases = inst.database_stats || [];
  const allTables = inst.table_stats || [];
  const slowQueries = inst.slow_queries || [];
  const totalData = inst.disk_used_bytes || 0;
  const walDisk = inst.wal_disk_bytes || 0;
  const totalDisk = totalData + walDisk;
  const volumeBytes = parseStorageSize(storageSpec);

  const dbTables = selectedDb
    ? allTables.filter(t => t.database_name === selectedDb)
    : [];

  return (
    <div className="confirm-overlay" onClick={onClose}>
      <div className="confirm-modal" onClick={e => e.stopPropagation()} style={{ width: 820 }}>
        <div className="confirm-header">
          <h3>
            <Server size={18} />
            {inst.pod_name}
            <RoleBadge role={inst.role} />
          </h3>
          <button className="modal-close" onClick={onClose}><X size={18} /></button>
        </div>
        <div className="confirm-body">
          {/* Overview */}
          <div className="report-section">
            <h5><Info size={13} /> Instance Overview</h5>
            <div className="report-grid">
              <ReportRow label="Ready" value={inst.ready ? 'Yes' : 'No'} />
              <ReportRow label="Timeline" value={inst.timeline_id || '-'} />
              <ReportRow label="Connections" value={inst.connections_max > 0 ? `${inst.connections_used} / ${inst.connections_max} (${inst.connections_active || 0} active)` : '-'} />
              <ReportRow label="Replication Lag" value={
                inst.role === 'replica' && inst.replication_lag_seconds > 0
                  ? formatLagTime(inst.replication_lag_seconds)
                  : formatLag(inst.replication_lag_bytes)
              } />
              {inst.index_hit_ratio > 0 && <ReportRow label="Index Hit Ratio" value={<CacheHitBadge pct={inst.index_hit_ratio * 100} />} />}
              {inst.txn_commit_ratio > 0 && <ReportRow label="Txn Commit Ratio" value={<CacheHitBadge pct={inst.txn_commit_ratio * 100} />} />}
              {inst.pg_start_time && <ReportRow label="PG Start Time" value={new Date(inst.pg_start_time).toLocaleString()} />}
              {inst.role === 'replica' && <ReportRow label="WAL Receiver" value={inst.wal_receiver_active ? 'Streaming' : 'Disconnected'} />}
              {inst.error_message && <ReportRow label="Error" value={inst.error_message} />}
            </div>
          </div>

          {/* Disk Usage Breakdown */}
          {totalDisk > 0 && (
            <div className="report-section">
              <h5><HardDrive size={13} /> Disk Usage</h5>
              <div className="disk-breakdown">
                <DiskBar label="Data" bytes={totalData} total={volumeBytes || totalDisk} color="var(--green)" />
                <DiskBar label="WAL" bytes={walDisk} total={volumeBytes || totalDisk} color="var(--blue)" />
                {volumeBytes > 0 && (
                  <div className="disk-total-row">
                    <span className="report-label">Volume</span>
                    <span className="mono report-value">{formatDisk(volumeBytes)}</span>
                    <span className="disk-pct mono">{((totalDisk / volumeBytes) * 100).toFixed(1)}% used</span>
                  </div>
                )}
              </div>
            </div>
          )}

          {/* WAL Stats */}
          {(inst.wal_records > 0 || inst.wal_bytes > 0) && (
            <div className="report-section">
              <h5><BarChart3 size={13} /> WAL Statistics</h5>
              <div className="report-grid">
                <ReportRow label="WAL Records" value={formatNumber(inst.wal_records)} />
                <ReportRow label="WAL Bytes Written" value={formatDisk(inst.wal_bytes)} />
                <ReportRow label="WAL Buffers Full" value={formatNumber(inst.wal_buffers_full)} />
              </div>
            </div>
          )}

          {/* Database Sizes with Cache Hit Ratio */}
          {databases.length > 0 && !selectedDb && (
            <div className="report-section">
              <h5><Database size={13} /> Databases ({databases.length})</h5>
              <div style={{ overflowX: 'auto' }}>
                <table className="instance-table" style={{ fontSize: 11.5 }}>
                  <thead>
                    <tr>
                      <th>Database</th>
                      <th>Size</th>
                      <th>% of Data</th>
                      <th>Cache Hit</th>
                      <th></th>
                    </tr>
                  </thead>
                  <tbody>
                    {databases.map(db => {
                      const pct = totalData > 0 ? (db.size_bytes / totalData) * 100 : 0;
                      const hasTables = allTables.some(t => t.database_name === db.database_name);
                      const hitPct = db.cache_hit_ratio ? (db.cache_hit_ratio * 100) : null;
                      return (
                        <tr
                          key={db.database_name}
                          className={hasTables ? 'inst-row' : ''}
                          onClick={hasTables ? () => setSelectedDb(db.database_name) : undefined}
                          title={hasTables ? 'Click to view tables' : ''}
                        >
                          <td className="mono">{db.database_name}</td>
                          <td className="mono">{formatDisk(db.size_bytes)}</td>
                          <td>
                            <span className="db-pct-bar">
                              <span className="db-pct-fill" style={{ width: Math.min(pct, 100) + '%' }}></span>
                              <span className="db-pct-label">{pct.toFixed(1)}%</span>
                            </span>
                          </td>
                          <td>
                            {hitPct !== null ? (
                              <CacheHitBadge pct={hitPct} />
                            ) : <span className="muted">-</span>}
                          </td>
                          <td className="muted" style={{ fontSize: 11 }}>
                            {hasTables && <ChevronRight size={13} />}
                          </td>
                        </tr>
                      );
                    })}
                  </tbody>
                </table>
              </div>
            </div>
          )}

          {/* Table Stats (drill-down from a database) */}
          {selectedDb && (
            <div className="report-section">
              <h5>
                <button className="db-back" onClick={() => setSelectedDb(null)} title="Back to databases">
                  <ArrowLeft size={14} />
                </button>
                <Table2 size={13} /> Tables in {selectedDb} ({dbTables.length})
              </h5>
              {dbTables.length > 0 ? (
                <div style={{ overflowX: 'auto' }}>
                  <table className="instance-table" style={{ fontSize: 11.5 }}>
                    <thead>
                      <tr>
                        <th>Table</th>
                        <th>Size</th>
                        <th>Live</th>
                        <th>Dead</th>
                        <th>Seq</th>
                        <th>Idx</th>
                        <th>Ins</th>
                        <th>Upd</th>
                        <th>Del</th>
                        <th>Last Vacuum</th>
                      </tr>
                    </thead>
                    <tbody>
                      {dbTables.map(t => (
                        <tr key={t.schema_name + '.' + t.table_name}>
                          <td className="mono">{t.schema_name}.{t.table_name}</td>
                          <td className="mono">{formatDisk(t.table_size_bytes)}</td>
                          <td className="mono">{formatNumber(t.live_tuples)}</td>
                          <td className="mono" style={t.dead_tuples > t.live_tuples * 0.1 && t.dead_tuples > 100 ? { color: 'var(--red)' } : {}}>{formatNumber(t.dead_tuples)}</td>
                          <td className="mono">{formatNumber(t.seq_scan)}</td>
                          <td className="mono">{formatNumber(t.idx_scan)}</td>
                          <td className="mono">{formatNumber(t.n_tup_ins)}</td>
                          <td className="mono">{formatNumber(t.n_tup_upd)}</td>
                          <td className="mono">{formatNumber(t.n_tup_del)}</td>
                          <td className="mono muted">{t.last_autovacuum ? timeAgo(t.last_autovacuum) : (t.last_vacuum ? timeAgo(t.last_vacuum) : '-')}</td>
                        </tr>
                      ))}
                    </tbody>
                  </table>
                </div>
              ) : (
                <div className="empty" style={{ padding: 16 }}>No user tables in this database</div>
              )}
            </div>
          )}

          {/* Slow Queries */}
          {slowQueries.length > 0 && !selectedDb && (
            <div className="report-section">
              <h5><SearchCode size={13} /> Slow Queries (top {slowQueries.length} by avg time)</h5>
              <div style={{ overflowX: 'auto' }}>
                <table className="instance-table" style={{ fontSize: 11.5 }}>
                  <thead>
                    <tr>
                      <th>Query</th>
                      <th>Database</th>
                      <th>Calls</th>
                      <th>Avg</th>
                      <th>Max</th>
                      <th>Total</th>
                      <th>Rows</th>
                    </tr>
                  </thead>
                  <tbody>
                    {slowQueries.map((sq, i) => (
                      <tr key={i} className="sq-row" title={sq.query}>
                        <td className="sq-query mono">{truncateQuery(sq.query)}</td>
                        <td className="mono muted">{sq.database_name}</td>
                        <td className="mono">{formatNumber(sq.calls)}</td>
                        <td className="mono">
                          <span className={sq.mean_exec_time_ms > 1000 ? 'sq-slow' : sq.mean_exec_time_ms > 100 ? 'sq-warn' : ''}>
                            {formatMs(sq.mean_exec_time_ms)}
                          </span>
                        </td>
                        <td className="mono">{formatMs(sq.max_exec_time_ms)}</td>
                        <td className="mono">{formatMs(sq.total_exec_time_ms)}</td>
                        <td className="mono">{formatNumber(sq.rows)}</td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </div>
            </div>
          )}
        </div>
        <div className="confirm-footer">
          <button className="btn-sm" onClick={onClose}>Close</button>
        </div>
      </div>
    </div>
  );
}

function CacheHitBadge({ pct }) {
  const color = pct >= 99 ? 'var(--green)' : pct >= 95 ? 'var(--amber)' : 'var(--red)';
  return (
    <span className="cache-hit-badge" style={{ color }}>
      {pct.toFixed(1)}%
    </span>
  );
}

function DiskBar({ label, bytes, total, color }) {
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

function ReportRow({ label, value }) {
  return (
    <div className="report-row">
      <span className="report-label">{label}</span>
      <span className="report-value">{String(value)}</span>
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
      {primary > 0 && <span className="inst-sum-ok"><Crown size={11} /> {primary}P</span>}
      {replicas > 0 && <span className="inst-sum-ok"><Copy size={11} /> {replicas}R</span>}
      {down > 0 && <span className="inst-sum-down"><AlertCircle size={11} /> {down} down</span>}
    </span>
  );
}

function RoleBadge({ role }) {
  const colors = { primary: 'badge-green', replica: 'badge-gray', failed_primary: 'badge-red' };
  const icons = { primary: Crown, replica: Copy, failed_primary: AlertCircle };
  const cls = colors[role] || 'badge-gray';
  const Icon = icons[role] || Shield;
  return <span className={`badge ${cls}`}><Icon size={11} />{role || 'unknown'}</span>;
}

function PgStatusDot({ inst }) {
  if (!inst.ready) {
    return <span className="online-dot dot-red dot-blink" title="Not ready"></span>;
  }
  if (!inst.pg_start_time) {
    return <span className="online-dot dot-green" title="Ready"></span>;
  }
  return <span className="online-dot dot-green" title={`PG up since ${new Date(inst.pg_start_time).toLocaleString()}`}></span>;
}

function LagDot({ inst }) {
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

function truncateQuery(q) {
  if (!q) return '-';
  const clean = q.replace(/\s+/g, ' ').trim();
  return clean.length > 80 ? clean.substring(0, 77) + '...' : clean;
}

function formatMs(ms) {
  if (ms == null) return '-';
  if (ms < 1) return ms.toFixed(3) + ' ms';
  if (ms < 1000) return ms.toFixed(1) + ' ms';
  if (ms < 60000) return (ms / 1000).toFixed(2) + ' s';
  return (ms / 60000).toFixed(1) + ' min';
}

function parseStorageSize(spec) {
  if (!spec) return 0;
  const m = spec.match(/^(\d+(?:\.\d+)?)\s*(Ti|Gi|Mi|Ki|T|G|M|K)?/i);
  if (!m) return 0;
  const n = parseFloat(m[1]);
  const unit = (m[2] || '').toLowerCase();
  const multipliers = { ti: 1099511627776, gi: 1073741824, mi: 1048576, ki: 1024, t: 1e12, g: 1e9, m: 1e6, k: 1e3 };
  return n * (multipliers[unit] || 1);
}

function formatNumber(n) {
  if (n == null) return '-';
  if (n >= 1_000_000_000) return (n / 1_000_000_000).toFixed(1) + 'B';
  if (n >= 1_000_000) return (n / 1_000_000).toFixed(1) + 'M';
  if (n >= 10_000) return (n / 1_000).toFixed(1) + 'K';
  return n.toLocaleString();
}
