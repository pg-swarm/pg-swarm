import { useState, useEffect, useCallback } from 'react';
import { useParams } from 'react-router-dom';
import { ClusterBadge } from '../components/Badge';
import MiniHeader from '../components/MiniHeader';
import { api, parseSpec, timeAgo } from '../api';
import {
  RoleBadge, PgStatusDot, LagDot, ConnBar, InstanceSummary, KV,
  CacheHitBadge, DiskBar, ReportRow,
  formatLag, formatLagTime, formatDisk, formatMs, formatNumber,
  truncateQuery, parseStorageSize,
} from './Clusters';
import {
  Server, Crown, Copy,
  ArrowUpRight, Pause, Play, HardDrive, BarChart3,
  Table2, SearchCode, ArrowLeft, X, Info, AlertTriangle,
  AlertCircle, Flame, Database, Activity, ChevronDown, ChevronRight,
} from 'lucide-react';

const SEV_ICONS = { info: Info, warning: AlertTriangle, error: AlertCircle, critical: Flame };

export default function ClusterDetail() {
  const { id } = useParams();
  const [clusters, setClusters] = useState([]);
  const [satellites, setSatellites] = useState([]);
  const [health, setHealth] = useState([]);
  const [events, setEvents] = useState([]);
  const [deploymentRules, setDeploymentRules] = useState([]);
  const [busy, setBusy] = useState(false);
  const [detailInstId, setDetailInstId] = useState(null);
  const [switchoverTarget, setSwitchoverTarget] = useState(null);
  const [eventsExpanded, setEventsExpanded] = useState(true);

  const refresh = useCallback(async () => {
    try {
      const [c, s, h, e, dr] = await Promise.all([
        api.clusters(), api.satellites(), api.health(), api.events(50), api.deploymentRules(),
      ]);
      setClusters(c || []);
      setSatellites(s || []);
      setHealth(h || []);
      setEvents(e || []);
      setDeploymentRules(dr || []);
    } catch (err) {
      console.error('ClusterDetail refresh failed:', err);
    }
  }, []);

  useEffect(() => {
    refresh();
    const timer = setInterval(refresh, 10000);
    return () => clearInterval(timer);
  }, [refresh]);

  // Set document title when cluster data loads
  useEffect(() => {
    const c = clusters.find(c => c.id === id);
    if (c) {
      document.title = `${c.name} (${c.namespace || 'default'}) - pg-swarm`;
    }
    return () => { document.title = 'pg-swarm'; };
  }, [clusters, id]);

  const cluster = clusters.find(c => c.id === id);
  if (!cluster) {
    return (
      <div style={{ display: 'flex', flexDirection: 'column', height: '100vh', background: 'var(--bg)', color: 'var(--text)' }}>
        <MiniHeader />
        <div style={{
          background: 'var(--white)', borderBottom: '1px solid var(--border)',
          padding: '16px 24px', display: 'flex', alignItems: 'center', gap: 12,
        }}>
          <Database size={18} style={{ color: 'var(--green)' }} />
          <span style={{ fontWeight: 700, fontSize: 15, color: 'var(--text)' }}>
            Loading cluster...
          </span>
        </div>
        <div style={{ flex: 1, display: 'flex', alignItems: 'center', justifyContent: 'center', color: 'var(--text-secondary)' }}>
          {clusters.length > 0 ? 'Cluster not found' : 'Fetching data...'}
        </div>
      </div>
    );
  }

  const s = parseSpec(cluster.config);
  const sat = satellites.find(x => x.id === cluster.satellite_id);
  const h = health.find(x => x.cluster_name === cluster.name && x.satellite_id === cluster.satellite_id);
  const instances = h?.instances || [];
  const hasFailover = s.failover?.enabled;

  const clusterEvents = (events || [])
    .filter(e => e.cluster_name === cluster.name && e.satellite_id === cluster.satellite_id)
    .slice(0, 20);

  const rule = cluster.deployment_rule_id
    ? deploymentRules.find(r => r.id === cluster.deployment_rule_id)
    : null;
  const labels = rule?.label_selector || {};
  const labelEntries = Object.entries(labels);

  async function togglePause() {
    setBusy(true);
    try {
      if (cluster.paused) {
        await api.resumeCluster(cluster.id);
      } else {
        await api.pauseCluster(cluster.id);
      }
      refresh();
    } catch (e) {
      alert(e.message);
    } finally {
      setBusy(false);
    }
  }

  function requestSwitchover(targetPod) {
    const currentPrimary = instances.find(i => i.role === 'primary');
    setSwitchoverTarget({ clusterId: cluster.id, targetPod, currentPrimary: currentPrimary?.pod_name });
  }

  async function confirmSwitchover() {
    if (!switchoverTarget) return;
    const { clusterId, targetPod } = switchoverTarget;
    setSwitchoverTarget(null);
    setBusy(true);
    try {
      await api.switchover(clusterId, targetPod);
      refresh();
      setTimeout(() => { refresh(); setBusy(false); }, 12000);
    } catch (e) {
      alert('Switchover failed: ' + e.message);
      setBusy(false);
    }
  }

  const detailInst = detailInstId ? instances.find(i => i.pod_name === detailInstId) : null;

  const tags = [];
  if (cluster.paused) tags.push('paused');
  if (s.failover?.enabled) tags.push('failover');
  if (s.archive?.mode) tags.push('archive:' + s.archive.mode);
  if (s.databases?.length) tags.push(s.databases.length + ' db' + (s.databases.length > 1 ? 's' : ''));

  return (
    <div style={{ display: 'flex', flexDirection: 'column', height: '100vh', background: 'var(--bg)', color: 'var(--text)' }}>
      <MiniHeader />
      {/* Header bar */}
      <div style={{
        background: 'var(--white)', borderBottom: '1px solid var(--border)',
        padding: '12px 24px', flexShrink: 0,
        display: 'flex', flexDirection: 'column', gap: 8,
      }}>
        {/* Row 1: name + badges */}
        <div style={{ display: 'flex', alignItems: 'center', gap: 12, flexWrap: 'wrap' }}>
          <Database size={18} style={{ color: 'var(--green)', flexShrink: 0 }} />
          <span style={{ fontWeight: 700, fontSize: 16, color: 'var(--text)' }}>
            {cluster.name}
          </span>
          <span style={{ color: 'var(--text-secondary)', fontSize: 13 }}>
            {cluster.namespace || 'default'}
          </span>
          {h && (
            <span style={{
              background: h.state === 'running' ? '#046D39' : h.state === 'failed' ? '#dc2626' : '#d97706',
              color: '#fff', padding: '1px 8px', borderRadius: 3, fontSize: 11,
              fontWeight: 600, textTransform: 'uppercase',
            }}>
              {h.state}
            </span>
          )}
          {(!h || h.state !== cluster.state) && (
            <span style={{
              background: cluster.state === 'running' ? '#046D39' : cluster.state === 'failed' ? '#dc2626' : '#d97706',
              color: '#fff', padding: '1px 8px', borderRadius: 3, fontSize: 11,
              fontWeight: 600, textTransform: 'uppercase',
            }}>
              {cluster.state}
            </span>
          )}
          {cluster.paused && (
            <span style={{
              background: '#d97706', color: '#fff', padding: '1px 8px', borderRadius: 3,
              fontSize: 11, fontWeight: 600, textTransform: 'uppercase',
            }}>
              paused
            </span>
          )}
        </div>

        {/* Row 2: satellite + meta */}
        <div style={{ display: 'flex', alignItems: 'center', gap: 16, flexWrap: 'wrap', fontSize: 13 }}>
          <span style={{ display: 'flex', alignItems: 'center', gap: 4, color: 'var(--text-secondary)' }}>
            <Server size={12} />
            {sat ? sat.hostname : (cluster.satellite_id ? cluster.satellite_id.substring(0, 8) : 'unassigned')}
          </span>
          {sat?.region && (
            <span style={{ background: 'var(--gray-bg)', padding: '1px 8px', borderRadius: 3, fontSize: 12, color: 'var(--text-secondary)' }}>
              {sat.region}
            </span>
          )}
          <span style={{ color: 'var(--text-secondary)', fontSize: 12 }}>
            PG {s.postgres?.version || '-'}
          </span>
          <span style={{ color: 'var(--text-secondary)', fontSize: 12 }}>
            {s.replicas || '-'} replicas
          </span>
          <span style={{ color: 'var(--text-secondary)', fontSize: 12 }}>
            {s.storage?.size || '-'} storage
          </span>
          <span style={{ color: 'var(--text-secondary)', fontSize: 12 }}>
            v{cluster.config_version}
          </span>
          <span style={{ color: 'var(--text-secondary)', fontSize: 12 }} title={cluster.created_at ? new Date(cluster.created_at).toLocaleString() : ''}>
            created {timeAgo(cluster.created_at)}
          </span>
          <span style={{ color: 'var(--text-secondary)', fontSize: 12 }}>
            updated {timeAgo(cluster.updated_at)}
          </span>
        </div>

        {/* Row 3: labels + tags + pause button */}
        <div style={{ display: 'flex', alignItems: 'center', gap: 8, flexWrap: 'wrap' }}>
          {labelEntries.length > 0 && labelEntries.map(([k, v]) => (
            <span key={k} style={{
              background: 'var(--gray-bg)', border: '1px solid var(--border)',
              padding: '0 6px', borderRadius: 3, fontSize: 11, color: 'var(--text-secondary)',
            }}>
              {k}={v}
            </span>
          ))}
          {tags.map(t => (
            <span key={t} style={{
              background: t === 'paused' ? 'var(--amber-bg)' : 'var(--green-bg)',
              color: t === 'paused' ? 'var(--amber)' : 'var(--green)',
              padding: '0 6px', borderRadius: 3, fontSize: 11, fontWeight: 500,
            }}>
              {t}
            </span>
          ))}
          <div style={{ marginLeft: 'auto' }}>
            <button onClick={togglePause} disabled={busy} style={{
              background: 'var(--gray-bg)', color: cluster.paused ? 'var(--green)' : 'var(--amber)',
              border: '1px solid var(--border)', borderRadius: 4,
              padding: '3px 12px', fontSize: 12, cursor: 'pointer',
              display: 'flex', alignItems: 'center', gap: 4,
            }}>
              {cluster.paused ? <Play size={12} /> : <Pause size={12} />}
              {busy ? '...' : (cluster.paused ? 'Resume' : 'Pause')}
            </button>
          </div>
        </div>
      </div>

      {/* Scrollable content area */}
      <div style={{ flex: 1, overflow: 'auto', padding: '16px 24px' }}>

        {/* Instances table */}
        {instances.length > 0 && (
          <div style={{ marginBottom: 20 }}>
            <h4 style={{ color: 'var(--text)', fontSize: 14, fontWeight: 600, marginBottom: 10, display: 'flex', alignItems: 'center', gap: 6 }}>
              <Server size={14} />
              Instances ({instances.length})
              <span style={{ marginLeft: 8 }}><InstanceSummary instances={instances} /></span>
            </h4>
            <div style={{ background: 'var(--white)', border: '1px solid var(--border)', borderRadius: 8, overflow: 'hidden' }}>
              <div style={{ overflowX: 'auto' }}>
                <table style={{ width: '100%', borderCollapse: 'collapse', fontSize: 12.5 }}>
                  <thead>
                    <tr>
                      {['Instance', 'Role', 'Status', 'Lag', 'Conns', 'Disk', 'TL', ...(hasFailover ? [''] : [])].map((col, i) => (
                        <th key={i} style={{
                          textAlign: 'left', padding: '8px 12px', fontSize: 10, fontWeight: 600,
                          textTransform: 'uppercase', letterSpacing: '.4px', color: 'var(--text-secondary)',
                          background: 'var(--bg)', borderBottom: '1px solid var(--border)',
                        }}>{col}</th>
                      ))}
                    </tr>
                  </thead>
                  <tbody>
                    {instances.map(inst => {
                      const isSelected = detailInstId === inst.pod_name;
                      return (
                        <tr
                          key={inst.pod_name}
                          onClick={() => setDetailInstId(isSelected ? null : inst.pod_name)}
                          style={{
                            cursor: 'pointer',
                            background: inst.error_message ? 'var(--red-bg)' : isSelected ? 'var(--green-light)' : 'transparent',
                            borderBottom: '1px solid var(--border)',
                          }}
                        >
                          <td style={{ padding: '6px 12px', fontFamily: 'monospace', fontSize: 11.5, color: 'var(--text)' }}>
                            {inst.pod_name}
                          </td>
                          <td style={{ padding: '6px 12px' }}>
                            <RoleBadge role={inst.role} />
                          </td>
                          <td style={{ padding: '6px 12px' }}>
                            <PgStatusDot inst={inst} />
                            {inst.role === 'replica' && <LagDot inst={inst} />}
                          </td>
                          <td style={{ padding: '6px 12px', fontFamily: 'monospace', fontSize: 11.5, color: 'var(--text)' }}>
                            {inst.role === 'replica' && inst.replication_lag_seconds > 0
                              ? formatLagTime(inst.replication_lag_seconds)
                              : formatLag(inst.replication_lag_bytes)}
                          </td>
                          <td style={{ padding: '6px 12px' }}>
                            {inst.connections_max > 0
                              ? <ConnBar used={inst.connections_used} max={inst.connections_max} />
                              : '-'}
                          </td>
                          <td style={{ padding: '6px 12px', fontFamily: 'monospace', fontSize: 11.5, color: 'var(--text)' }}>
                            {formatDisk(inst.disk_used_bytes)}
                          </td>
                          <td style={{ padding: '6px 12px', fontFamily: 'monospace', fontSize: 11.5, color: 'var(--text-secondary)' }}>
                            {inst.timeline_id || '-'}
                          </td>
                          {hasFailover && (
                            <td style={{ padding: '6px 12px' }}>
                              {inst.role === 'replica' && inst.ready && (
                                <button
                                  onClick={(e) => { e.stopPropagation(); requestSwitchover(inst.pod_name); }}
                                  disabled={busy}
                                  title="Promote to primary"
                                  style={{
                                    background: 'transparent', color: 'var(--blue)',
                                    border: '1px solid var(--border)', borderRadius: 4,
                                    padding: '2px 8px', fontSize: 10.5, cursor: 'pointer',
                                    display: 'inline-flex', alignItems: 'center', gap: 3,
                                  }}
                                >
                                  <ArrowUpRight size={11} />
                                  {busy ? '...' : 'Promote'}
                                </button>
                              )}
                            </td>
                          )}
                        </tr>
                      );
                    })}
                  </tbody>
                </table>
              </div>

              {/* Instance errors in table footer */}
              {instances.some(i => i.error_message) && (
                <div style={{ borderTop: '1px solid var(--border)', padding: '8px 12px', background: 'var(--red-bg)' }}>
                  {instances.filter(i => i.error_message).map(i => (
                    <div key={i.pod_name + '-err'} style={{ fontSize: 11, color: 'var(--red)', marginBottom: 4 }}>
                      <span style={{ fontFamily: 'monospace' }}>{i.pod_name}</span>: {i.error_message}
                    </div>
                  ))}
                </div>
              )}
            </div>
          </div>
        )}

        {/* Inline Instance Detail */}
        {detailInst && (
          <InstanceDetailSection
            inst={detailInst}
            storageSpec={s.storage?.size}
            onClose={() => setDetailInstId(null)}
          />
        )}

        {/* Events */}
        {clusterEvents.length > 0 && (
          <div style={{ marginBottom: 20 }}>
            <div
              onClick={() => setEventsExpanded(prev => !prev)}
              style={{
                display: 'flex', alignItems: 'center', gap: 6,
                color: 'var(--text)', fontSize: 14, fontWeight: 600,
                marginBottom: 10, cursor: 'pointer', userSelect: 'none',
              }}
            >
              {eventsExpanded
                ? <ChevronDown size={14} style={{ color: 'var(--text-secondary)' }} />
                : <ChevronRight size={14} style={{ color: 'var(--text-secondary)' }} />}
              <Activity size={14} />
              Events ({clusterEvents.length})
            </div>
            {eventsExpanded && (
              <div style={{ background: 'var(--white)', border: '1px solid var(--border)', borderRadius: 8, overflow: 'hidden' }}>
                {clusterEvents.map((evt, i) => {
                  const SevIcon = SEV_ICONS[evt.severity] || Info;
                  return (
                    <div key={evt.id || i} style={{
                      padding: '6px 12px', display: 'flex', alignItems: 'center', gap: 8,
                      fontSize: 12, borderBottom: i < clusterEvents.length - 1 ? '1px solid var(--border)' : 'none',
                    }}>
                      <span style={{
                        display: 'inline-flex', alignItems: 'center', gap: 3,
                        fontSize: 11, fontWeight: 600, whiteSpace: 'nowrap',
                        color: evt.severity === 'info' ? 'var(--green)' : evt.severity === 'warning' ? 'var(--amber)' : 'var(--red)',
                      }}>
                        <SevIcon size={11} />
                        {evt.severity}
                      </span>
                      <span style={{ flex: 1, color: 'var(--text)' }}>{evt.message}</span>
                      <span style={{ fontSize: 11, color: 'var(--text-secondary)', flexShrink: 0 }}>{timeAgo(evt.created_at)}</span>
                    </div>
                  );
                })}
              </div>
            )}
          </div>
        )}
      </div>

      {/* Switchover confirmation modal */}
      {switchoverTarget && (
        <SwitchoverConfirmModal
          target={switchoverTarget}
          onConfirm={confirmSwitchover}
          onCancel={() => setSwitchoverTarget(null)}
        />
      )}
    </div>
  );
}

/* ── Instance Detail (inline section, not modal) ────── */

function InstanceDetailSection({ inst, storageSpec, onClose }) {
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
    <div style={{
      background: 'var(--white)', border: '1px solid var(--border)', borderRadius: 8,
      marginBottom: 20, overflow: 'hidden',
    }}>
      {/* Section header */}
      <div style={{
        padding: '10px 16px', borderBottom: '1px solid var(--border)',
        display: 'flex', alignItems: 'center', justifyContent: 'space-between',
        background: 'var(--bg)',
      }}>
        <div style={{ display: 'flex', alignItems: 'center', gap: 8 }}>
          <Server size={14} style={{ color: 'var(--green)' }} />
          <span style={{ fontWeight: 600, fontSize: 14, color: 'var(--text)' }}>{inst.pod_name}</span>
          <RoleBadge role={inst.role} />
        </div>
        <button onClick={onClose} style={{
          background: 'none', border: 'none', color: 'var(--text-secondary)', cursor: 'pointer',
          padding: 4, borderRadius: 4, display: 'flex', alignItems: 'center',
        }}>
          <X size={16} />
        </button>
      </div>

      <div style={{ padding: '14px 16px' }}>
        {/* Overview */}
        <DetailSection title="Instance Overview" icon={<Info size={13} />}>
          <div style={{ display: 'grid', gridTemplateColumns: '1fr 1fr', gap: '4px 16px' }}>
            <DetailKV label="Ready" value={inst.ready ? 'Yes' : 'No'} />
            <DetailKV label="Timeline" value={inst.timeline_id || '-'} />
            <DetailKV label="Connections" value={inst.connections_max > 0 ? `${inst.connections_used} / ${inst.connections_max} (${inst.connections_active || 0} active)` : '-'} />
            <DetailKV label="Replication Lag" value={
              inst.role === 'replica' && inst.replication_lag_seconds > 0
                ? formatLagTime(inst.replication_lag_seconds)
                : formatLag(inst.replication_lag_bytes)
            } />
            {inst.index_hit_ratio > 0 && <DetailKV label="Index Hit Ratio" value={<CacheHitBadge pct={inst.index_hit_ratio * 100} />} />}
            {inst.txn_commit_ratio > 0 && <DetailKV label="Txn Commit Ratio" value={<CacheHitBadge pct={inst.txn_commit_ratio * 100} />} />}
            {inst.pg_start_time && <DetailKV label="PG Start Time" value={new Date(inst.pg_start_time).toLocaleString()} />}
            {inst.role === 'replica' && <DetailKV label="WAL Receiver" value={inst.wal_receiver_active ? 'Streaming' : 'Disconnected'} />}
            {inst.error_message && <DetailKV label="Error" value={inst.error_message} />}
          </div>
        </DetailSection>

        {/* Disk Usage */}
        {totalDisk > 0 && (
          <DetailSection title="Disk Usage" icon={<HardDrive size={13} />}>
            <div style={{ display: 'flex', flexDirection: 'column', gap: 6 }}>
              <DiskBarDark label="Data" bytes={totalData} total={volumeBytes || totalDisk} color="#4ade80" />
              <DiskBarDark label="WAL" bytes={walDisk} total={volumeBytes || totalDisk} color="#58a6ff" />
              {volumeBytes > 0 && (
                <div style={{ display: 'flex', alignItems: 'center', gap: 10, paddingTop: 4, borderTop: '1px solid var(--border)', marginTop: 2 }}>
                  <span style={{ width: 40, fontSize: 12, color: 'var(--text-secondary)' }}>Volume</span>
                  <span style={{ fontFamily: 'monospace', fontSize: 12, color: 'var(--text)' }}>{formatDisk(volumeBytes)}</span>
                  <span style={{ fontFamily: 'monospace', fontSize: 11, color: 'var(--text-secondary)' }}>{((totalDisk / volumeBytes) * 100).toFixed(1)}% used</span>
                </div>
              )}
            </div>
          </DetailSection>
        )}

        {/* WAL Stats */}
        {(inst.wal_records > 0 || inst.wal_bytes > 0) && (
          <DetailSection title="WAL Statistics" icon={<BarChart3 size={13} />}>
            <div style={{ display: 'grid', gridTemplateColumns: '1fr 1fr', gap: '4px 16px' }}>
              <DetailKV label="WAL Records" value={formatNumber(inst.wal_records)} />
              <DetailKV label="WAL Bytes Written" value={formatDisk(inst.wal_bytes)} />
              <DetailKV label="WAL Buffers Full" value={formatNumber(inst.wal_buffers_full)} />
            </div>
          </DetailSection>
        )}

        {/* Databases */}
        {databases.length > 0 && !selectedDb && (
          <DetailSection title={`Databases (${databases.length})`} icon={<Database size={13} />}>
            <div style={{ overflowX: 'auto' }}>
              <table style={{ width: '100%', borderCollapse: 'collapse', fontSize: 11.5 }}>
                <thead>
                  <tr>
                    {['Database', 'Size', '% of Data', 'Cache Hit', ''].map((col, i) => (
                      <th key={i} style={{
                        textAlign: 'left', padding: '6px 8px', fontSize: 10, fontWeight: 600,
                        textTransform: 'uppercase', letterSpacing: '.4px', color: 'var(--text-secondary)',
                        borderBottom: '1px solid var(--border)',
                      }}>{col}</th>
                    ))}
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
                        onClick={hasTables ? () => setSelectedDb(db.database_name) : undefined}
                        style={{ cursor: hasTables ? 'pointer' : 'default', borderBottom: '1px solid var(--border)' }}
                      >
                        <td style={{ padding: '5px 8px', fontFamily: 'monospace', fontSize: 11, color: 'var(--text)' }}>{db.database_name}</td>
                        <td style={{ padding: '5px 8px', fontFamily: 'monospace', fontSize: 11, color: 'var(--text)' }}>{formatDisk(db.size_bytes)}</td>
                        <td style={{ padding: '5px 8px' }}>
                          <span style={{ display: 'inline-flex', alignItems: 'center', gap: 4, minWidth: 80 }}>
                            <span style={{ height: 6, borderRadius: 3, background: 'var(--green)', minWidth: 1, width: Math.min(pct, 100) + '%' }}></span>
                            <span style={{ fontSize: 11, color: 'var(--text-secondary)', fontFamily: 'monospace' }}>{pct.toFixed(1)}%</span>
                          </span>
                        </td>
                        <td style={{ padding: '5px 8px' }}>
                          {hitPct !== null ? <CacheHitBadge pct={hitPct} /> : <span style={{ color: 'var(--text-secondary)' }}>-</span>}
                        </td>
                        <td style={{ padding: '5px 8px', color: 'var(--text-secondary)', fontSize: 11 }}>
                          {hasTables && <ChevronRight size={13} />}
                        </td>
                      </tr>
                    );
                  })}
                </tbody>
              </table>
            </div>
          </DetailSection>
        )}

        {/* Table Stats (drill-down) */}
        {selectedDb && (
          <DetailSection
            title={
              <span style={{ display: 'flex', alignItems: 'center', gap: 6 }}>
                <button onClick={() => setSelectedDb(null)} title="Back to databases" style={{
                  cursor: 'pointer', color: 'var(--green)', background: 'none', border: 'none',
                  display: 'inline-flex', alignItems: 'center', padding: 0,
                }}>
                  <ArrowLeft size={14} />
                </button>
                Tables in {selectedDb} ({dbTables.length})
              </span>
            }
            icon={<Table2 size={13} />}
          >
            {dbTables.length > 0 ? (
              <div style={{ overflowX: 'auto' }}>
                <table style={{ width: '100%', borderCollapse: 'collapse', fontSize: 11.5 }}>
                  <thead>
                    <tr>
                      {['Table', 'Size', 'Live', 'Dead', 'Seq', 'Idx', 'Ins', 'Upd', 'Del', 'Last Vacuum'].map((col, i) => (
                        <th key={i} style={{
                          textAlign: 'left', padding: '6px 8px', fontSize: 10, fontWeight: 600,
                          textTransform: 'uppercase', letterSpacing: '.4px', color: 'var(--text-secondary)',
                          borderBottom: '1px solid var(--border)',
                        }}>{col}</th>
                      ))}
                    </tr>
                  </thead>
                  <tbody>
                    {dbTables.map(t => (
                      <tr key={t.schema_name + '.' + t.table_name} style={{ borderBottom: '1px solid var(--border)' }}>
                        <td style={{ padding: '5px 8px', fontFamily: 'monospace', fontSize: 11, color: 'var(--text)' }}>{t.schema_name}.{t.table_name}</td>
                        <td style={{ padding: '5px 8px', fontFamily: 'monospace', fontSize: 11, color: 'var(--text)' }}>{formatDisk(t.table_size_bytes)}</td>
                        <td style={{ padding: '5px 8px', fontFamily: 'monospace', fontSize: 11, color: 'var(--text)' }}>{formatNumber(t.live_tuples)}</td>
                        <td style={{
                          padding: '5px 8px', fontFamily: 'monospace', fontSize: 11,
                          color: t.dead_tuples > t.live_tuples * 0.1 && t.dead_tuples > 100 ? 'var(--red)' : 'var(--text)',
                        }}>{formatNumber(t.dead_tuples)}</td>
                        <td style={{ padding: '5px 8px', fontFamily: 'monospace', fontSize: 11, color: 'var(--text)' }}>{formatNumber(t.seq_scan)}</td>
                        <td style={{ padding: '5px 8px', fontFamily: 'monospace', fontSize: 11, color: 'var(--text)' }}>{formatNumber(t.idx_scan)}</td>
                        <td style={{ padding: '5px 8px', fontFamily: 'monospace', fontSize: 11, color: 'var(--text)' }}>{formatNumber(t.n_tup_ins)}</td>
                        <td style={{ padding: '5px 8px', fontFamily: 'monospace', fontSize: 11, color: 'var(--text)' }}>{formatNumber(t.n_tup_upd)}</td>
                        <td style={{ padding: '5px 8px', fontFamily: 'monospace', fontSize: 11, color: 'var(--text)' }}>{formatNumber(t.n_tup_del)}</td>
                        <td style={{ padding: '5px 8px', fontFamily: 'monospace', fontSize: 11, color: 'var(--text-secondary)' }}>
                          {t.last_autovacuum ? timeAgo(t.last_autovacuum) : (t.last_vacuum ? timeAgo(t.last_vacuum) : '-')}
                        </td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </div>
            ) : (
              <div style={{ padding: 16, color: 'var(--text-secondary)', textAlign: 'center' }}>No user tables in this database</div>
            )}
          </DetailSection>
        )}

        {/* Slow Queries */}
        {slowQueries.length > 0 && !selectedDb && (
          <DetailSection title={`Slow Queries (top ${slowQueries.length} by avg time)`} icon={<SearchCode size={13} />}>
            <div style={{ overflowX: 'auto' }}>
              <table style={{ width: '100%', borderCollapse: 'collapse', fontSize: 11.5 }}>
                <thead>
                  <tr>
                    {['Query', 'Database', 'Calls', 'Avg', 'Max', 'Total', 'Rows'].map((col, i) => (
                      <th key={i} style={{
                        textAlign: 'left', padding: '6px 8px', fontSize: 10, fontWeight: 600,
                        textTransform: 'uppercase', letterSpacing: '.4px', color: 'var(--text-secondary)',
                        borderBottom: '1px solid var(--border)',
                      }}>{col}</th>
                    ))}
                  </tr>
                </thead>
                <tbody>
                  {slowQueries.map((sq, i) => (
                    <tr key={i} style={{ borderBottom: '1px solid var(--border)' }} title={sq.query}>
                      <td style={{
                        padding: '5px 8px', fontFamily: 'monospace', fontSize: 11, color: 'var(--text)',
                        maxWidth: 280, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap',
                      }}>{truncateQuery(sq.query)}</td>
                      <td style={{ padding: '5px 8px', fontFamily: 'monospace', fontSize: 11, color: 'var(--text-secondary)' }}>{sq.database_name}</td>
                      <td style={{ padding: '5px 8px', fontFamily: 'monospace', fontSize: 11, color: 'var(--text)' }}>{formatNumber(sq.calls)}</td>
                      <td style={{ padding: '5px 8px', fontFamily: 'monospace', fontSize: 11 }}>
                        <span style={{ color: sq.mean_exec_time_ms > 1000 ? 'var(--red)' : sq.mean_exec_time_ms > 100 ? 'var(--amber)' : 'var(--text)', fontWeight: sq.mean_exec_time_ms > 100 ? 600 : 400 }}>
                          {formatMs(sq.mean_exec_time_ms)}
                        </span>
                      </td>
                      <td style={{ padding: '5px 8px', fontFamily: 'monospace', fontSize: 11, color: 'var(--text)' }}>{formatMs(sq.max_exec_time_ms)}</td>
                      <td style={{ padding: '5px 8px', fontFamily: 'monospace', fontSize: 11, color: 'var(--text)' }}>{formatMs(sq.total_exec_time_ms)}</td>
                      <td style={{ padding: '5px 8px', fontFamily: 'monospace', fontSize: 11, color: 'var(--text)' }}>{formatNumber(sq.rows)}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          </DetailSection>
        )}
      </div>
    </div>
  );
}

/* ── Helper components for dark-themed detail view ──── */

function DetailSection({ title, icon, children }) {
  return (
    <div style={{ marginBottom: 16 }}>
      <h5 style={{
        fontSize: 11.5, fontWeight: 600, textTransform: 'uppercase',
        letterSpacing: '.5px', color: 'var(--text-secondary)', marginBottom: 8,
        paddingBottom: 6, borderBottom: '1px solid var(--border)',
        display: 'flex', alignItems: 'center', gap: 6,
      }}>
        {icon}
        {title}
      </h5>
      {children}
    </div>
  );
}

function DetailKV({ label, value }) {
  return (
    <div style={{ display: 'flex', justifyContent: 'space-between', padding: '3px 0' }}>
      <span style={{ fontSize: 12.5, color: 'var(--text-secondary)' }}>{label}</span>
      <span style={{ fontSize: 12.5, fontWeight: 500, color: 'var(--text)' }}>{typeof value === 'string' || typeof value === 'number' ? String(value) : value}</span>
    </div>
  );
}

function DiskBarDark({ label, bytes, total, color }) {
  const pct = total > 0 ? (bytes / total) * 100 : 0;
  return (
    <div style={{ display: 'flex', alignItems: 'center', gap: 10 }}>
      <span style={{ width: 40, flexShrink: 0, fontSize: 12, color: 'var(--text-secondary)' }}>{label}</span>
      <span style={{ flex: 1, height: 8, background: 'var(--gray-bg)', borderRadius: 4, overflow: 'hidden', minWidth: 80 }}>
        <span style={{ display: 'block', height: '100%', borderRadius: 4, background: color, width: Math.min(pct, 100) + '%', transition: 'width .3s ease' }}></span>
      </span>
      <span style={{ width: 70, textAlign: 'right', fontFamily: 'monospace', fontSize: 12, color: 'var(--text)' }}>{formatDisk(bytes)}</span>
      <span style={{ width: 50, textAlign: 'right', fontFamily: 'monospace', fontSize: 11, color: 'var(--text-secondary)' }}>{pct.toFixed(1)}%</span>
    </div>
  );
}

/* ── Switchover Confirm Modal ───────────────────────── */

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
