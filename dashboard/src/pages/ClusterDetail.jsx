import { useState, useEffect, useCallback, useRef } from 'react';
import { useParams, useNavigate } from 'react-router-dom';
import MiniHeader from '../components/MiniHeader';
import SwitchoverProgressModal from '../components/SwitchoverProgressModal';
import { useData } from '../context/DataContext';
import { useToast } from '../context/ToastContext';
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
  Loader, Trash2, Plus, Save, Pencil, Globe,
} from 'lucide-react';

const SEV_ICONS = { info: Info, warning: AlertTriangle, error: AlertCircle, critical: Flame };

const TABS = [
  { id: 'instances', label: 'Instances' },
  { id: 'databases', label: 'Databases' },
  { id: 'events', label: 'Events' },
];

export default function ClusterDetail() {
  const { id } = useParams();
  const navigate = useNavigate();
  const toast = useToast();
  const [clusters, setClusters] = useState([]);
  const [satellites, setSatellites] = useState([]);
  const [health, setHealth] = useState([]);
  const [events, setEvents] = useState([]);
  const [deploymentRules, setDeploymentRules] = useState([]);
  const [busy, setBusy] = useState(false);
  const [detailInstId, setDetailInstId] = useState(null);
  const [switchoverOpId, setSwitchoverOpId] = useState(null);
  const [switchoverOpLocal, setSwitchoverOpLocal] = useState(null);
  const [progressModalVisible, setProgressModalVisible] = useState(false);
  const [activeTab, setActiveTab] = useState('instances');
  const [showDeleteConfirm, setShowDeleteConfirm] = useState(false);
  const { activeOperations } = useData();
  const mockTimerRef = useRef(null);

  // Merge local switchover state with live WS data.
  // Local state (mock) is the baseline; WS data (live) enriches it.
  // steps and log are merged per-key so neither source can blank the other.
  const switchoverOp = switchoverOpId
    ? (() => {
        const local = switchoverOpLocal || {};
        const ws = activeOperations?.[switchoverOpId] || {};
        const localSteps = local.steps || {};
        const wsSteps = ws.steps || {};
        const mergedSteps = { ...localSteps };
        for (const k of Object.keys(wsSteps)) {
          mergedSteps[k] = { ...localSteps[k], ...wsSteps[k] };
        }
        const localLog = Array.isArray(local.log) ? local.log : [];
        const wsLog = Array.isArray(ws.log) ? ws.log : [];
        const mergedLog = wsLog.length > 0 ? wsLog : localLog;
        return {
          ...local,
          ...ws,
          primary_pod: ws.primary_pod || local.primary_pod,
          target_pod: ws.target_pod || local.target_pod,
          started: local.started || ws.started,
          steps: mergedSteps,
          log: mergedLog,
        };
      })()
    : null;

  // Clean up mock timer on unmount
  useEffect(() => {
    return () => { if (mockTimerRef.current) clearInterval(mockTimerRef.current); };
  }, []);

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
  }, [id]);

  useEffect(() => {
    refresh();
    const timer = setInterval(refresh, 10000);
    return () => clearInterval(timer);
  }, [refresh]);

  // Set document title when cluster data loads
  useEffect(() => {
    const c = clusters.find(c => c.id === id);
    if (c) {
      document.title = `${c.name} (${c.namespace || 'default'}) - PG-Swarm`;
    }
    return () => { document.title = 'PG-Swarm'; };
  }, [clusters, id]);

  const cluster = clusters.find(c => c.id === id);
  if (!cluster) {
    return (
      <div className="cluster-detail-page">
        <MiniHeader />
        <div className="cd-header-bar">
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

  async function deleteCluster() {
    setBusy(true);
    try {
      await api.deleteCluster(cluster.id);
      navigate('/clusters');
    } catch (e) {
      alert('Delete failed: ' + e.message);
      setBusy(false);
    }
  }

  function requestSwitchover(targetPod) {
    const currentPrimary = instances.find(i => i.role === 'primary');
    const opLocal = {
      operation_id: null,
      cluster_name: cluster.name,
      primary_pod: currentPrimary?.pod_name,
      target_pod: targetPod,
      done: false, success: false, error: null,
      started: false, steps: {}, log: [],
    };
    setSwitchoverOpLocal(opLocal);
    setSwitchoverOpId('pending-' + Date.now());
    setProgressModalVisible(true);
  }

  function startMockSimulation(opLocal) {
    const stepDefs = [
      { step: 1, name: 'verify_target', pod: opLocal.target_pod, detail: 'role=replica, pod ready' },
      { step: 2, name: 'find_primary', pod: opLocal.primary_pod, detail: `primary: ${opLocal.primary_pod}` },
      { step: 3, name: 'check_status', pod: opLocal.target_pod, detail: 'in_recovery=true' },
      { step: 4, name: 'fence_primary', pod: opLocal.primary_pod, detail: 'fenced, connections drained' },
      { step: 5, name: 'checkpoint', pod: opLocal.primary_pod, detail: 'checkpoint completed' },
      { step: 6, name: 'transfer_lease', pod: opLocal.target_pod, detail: `lease ${opLocal.cluster_name}-leader transferred` },
      { step: 7, name: 'promote', pod: opLocal.target_pod, detail: 'pg_promote() succeeded, exited recovery' },
      { step: 8, name: 'label_pods', pod: opLocal.target_pod, detail: `${opLocal.target_pod}=primary, ${opLocal.primary_pod}=replica` },
      { step: 9, name: 'renew_lease', pod: opLocal.target_pod, detail: `lease renewed for ${opLocal.target_pod}` },
    ];
    let idx = 0;
    let phase = 'starting';

    mockTimerRef.current = setInterval(() => {
      const def = stepDefs[idx];
      const ts = new Date().toISOString();
      setSwitchoverOpLocal(prev => {
        if (!prev) return prev;
        const newSteps = { ...prev.steps };
        newSteps[def.step] = {
          step: def.step, step_name: def.name, status: phase,
          target_pod: def.pod, error_message: phase === 'completed' ? def.detail : '',
          ponr: def.step >= 7,
        };
        const logEntry = {
          step: def.step, step_name: def.name, status: phase,
          target_pod: def.pod, detail: phase === 'completed' ? def.detail : '',
          ponr: def.step >= 7, timestamp: ts,
        };
        const newLog = [...(prev.log || []), logEntry];
        if (phase === 'completed' && idx === stepDefs.length - 1) {
          clearInterval(mockTimerRef.current);
          mockTimerRef.current = null;
          return { ...prev, steps: newSteps, log: newLog, done: true, success: true };
        }
        return { ...prev, steps: newSteps, log: newLog };
      });
      if (phase === 'starting') {
        phase = 'completed';
      } else {
        phase = 'starting';
        idx++;
      }
    }, 600);
  }

  async function startSwitchover() {
    if (!switchoverOpLocal || switchoverOpLocal.started) return;
    const targetPod = switchoverOpLocal.target_pod;

    const updated = { ...switchoverOpLocal, started: true };
    let useMock = false;
    try {
      const resp = await api.switchover(cluster.id, targetPod);
      updated.operation_id = resp.operation_id || ('mock-' + Date.now());
      useMock = !resp.operation_id;
    } catch {
      updated.operation_id = 'mock-' + Date.now();
      useMock = true;
    }

    setSwitchoverOpLocal(updated);
    setSwitchoverOpId(updated.operation_id);

    if (useMock) {
      startMockSimulation(updated);
    }
  }

  const detailInst = detailInstId ? instances.find(i => i.pod_name === detailInstId) : null;

  const tags = [];
  if (cluster.paused) tags.push('paused');
  if (s.failover?.enabled) tags.push('failover');
  if (s.archive?.mode) tags.push('archive:' + s.archive.mode);
  if (s.databases?.length) tags.push(s.databases.length + ' db' + (s.databases.length > 1 ? 's' : ''));

  return (
    <div className="cluster-detail-page">
      <MiniHeader />
      {/* Header: name + state + actions */}
      <div className="cd-header-bar">
        <div style={{ display: 'flex', alignItems: 'center', gap: 12, flexWrap: 'wrap' }}>
          <Database size={18} style={{ color: 'var(--green)', flexShrink: 0 }} />
          <span style={{ fontWeight: 700, fontSize: 16 }}>{cluster.name}</span>
          <span style={{ color: 'var(--text-secondary)', fontSize: 13 }}>{cluster.namespace || 'default'}</span>
          {h && <span className={'cd-state-badge cd-state-' + h.state}>{h.state}</span>}
          {(!h || h.state !== cluster.state) && <span className={'cd-state-badge cd-state-' + cluster.state}>{cluster.state}</span>}
          {cluster.paused && <span className="cd-state-badge cd-state-paused">paused</span>}
          <div style={{ marginLeft: 'auto', display: 'flex', gap: 8 }}>
            <button onClick={togglePause} disabled={busy} className={'cd-pause-btn' + (cluster.paused ? ' cd-pause-resume' : '')}>
              {cluster.paused ? <Play size={12} /> : <Pause size={12} />}
              {busy ? '...' : (cluster.paused ? 'Resume' : 'Pause')}
            </button>
            <button onClick={() => setShowDeleteConfirm(true)} disabled={busy} className="cd-pause-btn" style={{ color: 'var(--red)' }}>
              <Trash2 size={12} />
              Delete
            </button>
          </div>
        </div>
      </div>

      {/* Scrollable content area with gutters */}
      <div className="cd-content">
        <div className="cd-content-inner">

          {/* Summary card */}
          <div className="cd-card cd-summary-card">
            <div className="cd-summary-grid">
              <div className="cd-summary-item">
                <span className="cd-summary-label">Satellite</span>
                <span className="cd-summary-value">
                  <Server size={12} style={{ color: 'var(--text-secondary)' }} />
                  {sat ? sat.hostname : (cluster.satellite_id ? cluster.satellite_id.substring(0, 8) : 'unassigned')}
                </span>
              </div>
              {sat?.region && (
                <div className="cd-summary-item">
                  <span className="cd-summary-label">Region</span>
                  <span className="cd-summary-value">{sat.region}</span>
                </div>
              )}
              <div className="cd-summary-item">
                <span className="cd-summary-label">PostgreSQL</span>
                <span className="cd-summary-value">{s.postgres?.version || '-'}</span>
              </div>
              <div className="cd-summary-item">
                <span className="cd-summary-label">Replicas</span>
                <span className="cd-summary-value">{s.replicas || '-'}</span>
              </div>
              <div className="cd-summary-item">
                <span className="cd-summary-label">Storage</span>
                <span className="cd-summary-value">{s.storage?.size || '-'}</span>
              </div>
              <div className="cd-summary-item">
                <span className="cd-summary-label">Config</span>
                <span className="cd-summary-value">v{cluster.config_version}</span>
              </div>
              <div className="cd-summary-item">
                <span className="cd-summary-label">Created</span>
                <span className="cd-summary-value" title={cluster.created_at ? new Date(cluster.created_at).toLocaleString() : ''}>{timeAgo(cluster.created_at)}</span>
              </div>
              <div className="cd-summary-item">
                <span className="cd-summary-label">Updated</span>
                <span className="cd-summary-value">{timeAgo(cluster.updated_at)}</span>
              </div>
            </div>
            {(labelEntries.length > 0 || tags.length > 0) && (
              <div className="cd-summary-tags">
                {labelEntries.map(([k, v]) => (
                  <span key={k} className="cd-label-pill">{k}={v}</span>
                ))}
                {tags.map(t => (
                  <span key={t} className={'cd-tag' + (t === 'paused' ? ' cd-tag-amber' : '')}>{t}</span>
                ))}
              </div>
            )}
          </div>

          {/* Tab bar — inside content area, same width as cards */}
          <div className="tab-bar" style={{ marginBottom: 16, borderRadius: 8, overflow: 'hidden' }}>
            {TABS.map(tab => {
              const count = tab.id === 'instances' ? instances.length
                : tab.id === 'events' ? clusterEvents.length : 0;
              return (
                <button
                  key={tab.id}
                  className={'tab-item' + (activeTab === tab.id ? ' active' : '')}
                  onClick={() => setActiveTab(tab.id)}
                >
                  {tab.label}
                  {count > 0 && <span className="tab-badge">{count}</span>}
                </button>
              );
            })}
          </div>

          {/* ── Instances Tab ── */}
          {activeTab === 'instances' && (
            <>
              {instances.length > 0 && (
                <div className="cd-card">
                  <div className="cd-card-header">
                    <Server size={14} />
                    Instances ({instances.length})
                    <span style={{ marginLeft: 8 }}><InstanceSummary instances={instances} /></span>
                  </div>
                  <div className="cd-card-body" style={{ padding: 0 }}>
                    <div style={{ overflowX: 'auto' }}>
                      <table className="cd-table">
                        <thead>
                          <tr>
                            {['Instance', 'Role', 'Status', 'Lag', 'Conns', 'Disk', 'TL', ...(hasFailover ? [''] : [])].map((col, i) => (
                              <th key={i}>{col}</th>
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
                                className={inst.error_message ? 'cd-row-error' : isSelected ? 'cd-row-selected' : ''}
                              >
                                <td className="mono">{inst.pod_name}</td>
                                <td><RoleBadge role={inst.role} /></td>
                                <td>
                                  <PgStatusDot inst={inst} />
                                  {inst.role === 'replica' && <LagDot inst={inst} />}
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
                                    {inst.role === 'replica' && inst.ready && (() => {
                                      const isActiveTarget = switchoverOp && !switchoverOp.done && switchoverOp.target_pod === inst.pod_name;
                                      const switchoverBusy = switchoverOp && !switchoverOp.done;
                                      if (isActiveTarget) {
                                        const isRunning = switchoverOpLocal?.started;
                                        return (
                                          <button
                                            onClick={(e) => { e.stopPropagation(); setProgressModalVisible(true); }}
                                            className="cd-promote-btn cd-progress-btn"
                                          >
                                            {isRunning
                                              ? <><Loader size={11} className="so-spin" /> See Progress</>
                                              : <><ArrowUpRight size={11} /> Pending</>
                                            }
                                          </button>
                                        );
                                      }
                                      return (
                                        <button
                                          onClick={(e) => { e.stopPropagation(); requestSwitchover(inst.pod_name); }}
                                          disabled={busy || switchoverBusy}
                                          title="Promote to primary"
                                          className="cd-promote-btn"
                                        >
                                          <ArrowUpRight size={11} />
                                          {busy ? '...' : 'Promote'}
                                        </button>
                                      );
                                    })()}
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
                      <div className="cd-table-errors">
                        {instances.filter(i => i.error_message).map(i => (
                          <div key={i.pod_name + '-err'} style={{ fontSize: 11, color: 'var(--red)', marginBottom: 4 }}>
                            <span className="mono">{i.pod_name}</span>: {i.error_message}
                          </div>
                        ))}
                      </div>
                    )}
                  </div>
                </div>
              )}

              {instances.length === 0 && (
                <div className="cd-card">
                  <div className="cd-card-body cd-empty">No instances found for this cluster.</div>
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
            </>
          )}

          {/* ── Databases Tab ── */}
          {activeTab === 'databases' && (
            <ClusterDatabasesTab clusterId={id} toast={toast} />
          )}

          {/* ── Events Tab ── */}
          {activeTab === 'events' && (
            <div className="cd-card">
              <div className="cd-card-header">
                <Activity size={14} />
                Events ({clusterEvents.length})
              </div>
              <div className="cd-card-body" style={{ padding: 0 }}>
                {clusterEvents.length === 0 ? (
                  <div className="cd-empty">No events recorded for this cluster.</div>
                ) : (
                  clusterEvents.map((evt, i) => {
                    const SevIcon = SEV_ICONS[evt.severity] || Info;
                    return (
                      <div key={evt.id || i} className="cd-event-row">
                        <span className={'cd-sev cd-sev-' + evt.severity}>
                          <SevIcon size={11} />
                          {evt.severity}
                        </span>
                        <span style={{ flex: 1, color: 'var(--text)' }}>{evt.message}</span>
                        <span className="cd-event-time">{timeAgo(evt.created_at)}</span>
                      </div>
                    );
                  })
                )}
              </div>
            </div>
          )}

        </div>
      </div>

      {/* Switchover progress modal */}
      {switchoverOp && progressModalVisible && (
        <SwitchoverProgressModal
          operation={switchoverOp}
          instances={instances}
          onStart={startSwitchover}
          onClose={() => {
            setProgressModalVisible(false);
            if (!switchoverOpLocal?.started || switchoverOp.done) {
              setSwitchoverOpId(null);
              setSwitchoverOpLocal(null);
            }
          }}
        />
      )}

      {/* Delete confirmation modal */}
      {showDeleteConfirm && (
        <div className="confirm-overlay" onClick={() => setShowDeleteConfirm(false)}>
          <div className="confirm-modal" onClick={e => e.stopPropagation()} style={{ maxWidth: 460 }}>
            <div className="confirm-header" style={{ borderBottom: '1px solid var(--border)' }}>
              <h3><Trash2 size={18} style={{ color: 'var(--red)' }} /> Delete Cluster</h3>
              <button className="modal-close" onClick={() => setShowDeleteConfirm(false)}><X size={18} /></button>
            </div>
            <div className="confirm-body">
              <p>Are you sure you want to delete <strong>{cluster.name}</strong>?</p>
              <p className="muted" style={{ fontSize: 12.5, marginTop: 8 }}>
                Namespace: <code>{cluster.namespace || 'default'}</code>
                {' '}&middot;{' '}
                Satellite: <code>{sat ? sat.hostname : 'unknown'}</code>
              </p>
              <p className="muted" style={{ fontSize: 12.5, marginTop: 8 }}>
                This will remove the cluster configuration and notify the satellite to tear down all resources (StatefulSet, services, secrets, PVCs).
              </p>
            </div>
            <div className="confirm-footer">
              <button className="btn-sm" onClick={() => setShowDeleteConfirm(false)}>Cancel</button>
              <button className="btn-sm btn-danger" onClick={deleteCluster} disabled={busy}>
                {busy ? 'Deleting...' : 'Delete Cluster'}
              </button>
            </div>
          </div>
        </div>
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
    <div className="cd-card">
      {/* Section header */}
      <div className="cd-card-header" style={{ justifyContent: 'space-between' }}>
        <div style={{ display: 'flex', alignItems: 'center', gap: 8 }}>
          <Server size={14} style={{ color: 'var(--green)' }} />
          <span style={{ fontWeight: 600 }}>{inst.pod_name}</span>
          <RoleBadge role={inst.role} />
        </div>
        <button onClick={onClose} style={{
          background: 'none', border: 'none', color: 'var(--text-secondary)', cursor: 'pointer',
          padding: 4, borderRadius: 4, display: 'flex', alignItems: 'center',
        }}>
          <X size={16} />
        </button>
      </div>

      <div className="cd-card-body">
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
                  <span className="mono" style={{ fontSize: 12 }}>{formatDisk(volumeBytes)}</span>
                  <span className="mono muted" style={{ fontSize: 11 }}>{((totalDisk / volumeBytes) * 100).toFixed(1)}% used</span>
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
              <table className="cd-table cd-table-inner">
                <thead>
                  <tr>
                    {['Database', 'Size', '% of Data', 'Cache Hit', ''].map((col, i) => (
                      <th key={i}>{col}</th>
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
                        style={{ cursor: hasTables ? 'pointer' : 'default' }}
                      >
                        <td className="mono">{db.database_name}</td>
                        <td className="mono">{formatDisk(db.size_bytes)}</td>
                        <td>
                          <span style={{ display: 'inline-flex', alignItems: 'center', gap: 4, minWidth: 80 }}>
                            <span style={{ height: 6, borderRadius: 3, background: 'var(--green)', minWidth: 1, width: Math.min(pct, 100) + '%' }}></span>
                            <span className="mono muted" style={{ fontSize: 11 }}>{pct.toFixed(1)}%</span>
                          </span>
                        </td>
                        <td>
                          {hitPct !== null ? <CacheHitBadge pct={hitPct} /> : <span className="muted">-</span>}
                        </td>
                        <td className="muted">
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
                <table className="cd-table cd-table-inner">
                  <thead>
                    <tr>
                      {['Table', 'Size', 'Live', 'Dead', 'Seq', 'Idx', 'Ins', 'Upd', 'Del', 'Last Vacuum'].map((col, i) => (
                        <th key={i}>{col}</th>
                      ))}
                    </tr>
                  </thead>
                  <tbody>
                    {dbTables.map(t => (
                      <tr key={t.schema_name + '.' + t.table_name}>
                        <td className="mono">{t.schema_name}.{t.table_name}</td>
                        <td className="mono">{formatDisk(t.table_size_bytes)}</td>
                        <td className="mono">{formatNumber(t.live_tuples)}</td>
                        <td className="mono" style={{
                          color: t.dead_tuples > t.live_tuples * 0.1 && t.dead_tuples > 100 ? 'var(--red)' : undefined,
                        }}>{formatNumber(t.dead_tuples)}</td>
                        <td className="mono">{formatNumber(t.seq_scan)}</td>
                        <td className="mono">{formatNumber(t.idx_scan)}</td>
                        <td className="mono">{formatNumber(t.n_tup_ins)}</td>
                        <td className="mono">{formatNumber(t.n_tup_upd)}</td>
                        <td className="mono">{formatNumber(t.n_tup_del)}</td>
                        <td className="mono muted">
                          {t.last_autovacuum ? timeAgo(t.last_autovacuum) : (t.last_vacuum ? timeAgo(t.last_vacuum) : '-')}
                        </td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </div>
            ) : (
              <div className="cd-empty">No user tables in this database</div>
            )}
          </DetailSection>
        )}

        {/* Slow Queries */}
        {slowQueries.length > 0 && !selectedDb && (
          <DetailSection title={`Slow Queries (top ${slowQueries.length} by avg time)`} icon={<SearchCode size={13} />}>
            <div style={{ overflowX: 'auto' }}>
              <table className="cd-table cd-table-inner">
                <thead>
                  <tr>
                    {['Query', 'Database', 'Calls', 'Avg', 'Max', 'Total', 'Rows'].map((col, i) => (
                      <th key={i}>{col}</th>
                    ))}
                  </tr>
                </thead>
                <tbody>
                  {slowQueries.map((sq, i) => (
                    <tr key={i} title={sq.query}>
                      <td className="mono" style={{
                        maxWidth: 280, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap',
                      }}>{truncateQuery(sq.query)}</td>
                      <td className="mono muted">{sq.database_name}</td>
                      <td className="mono">{formatNumber(sq.calls)}</td>
                      <td>
                        <span className="mono" style={{ color: sq.mean_exec_time_ms > 1000 ? 'var(--red)' : sq.mean_exec_time_ms > 100 ? 'var(--amber)' : undefined, fontWeight: sq.mean_exec_time_ms > 100 ? 600 : 400 }}>
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
          </DetailSection>
        )}
      </div>
    </div>
  );
}

/* ── Helper components ────────────────────────────────── */

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

// --- Cluster Databases Tab ---

function ClusterDatabasesTab({ clusterId, toast }) {
  const [dbs, setDbs] = useState([]);
  const [editing, setEditing] = useState(null);
  const [saving, setSaving] = useState(false);
  const [form, setForm] = useState({ db_name: '', db_user: '', password: '', allowed_cidrs: '' });

  async function load() {
    try {
      const data = await api.clusterDatabases(clusterId);
      setDbs(data || []);
    } catch (e) {
      toast('Failed to load databases: ' + e.message, true);
    }
  }

  useEffect(() => { load(); }, [clusterId]);

  // Auto-refresh while any database is pending
  useEffect(() => {
    const hasPending = dbs.some(db => !db.status || db.status === 'pending');
    if (!hasPending) return;
    const iv = setInterval(load, 5000);
    return () => clearInterval(iv);
  }, [dbs]);

  function startCreate() {
    setForm({ db_name: '', db_user: '', password: '', allowed_cidrs: '' });
    setEditing('new');
  }

  function startEdit(db) {
    setForm({
      db_name: db.db_name,
      db_user: db.db_user,
      password: '',
      allowed_cidrs: (db.allowed_cidrs || []).join(', '),
    });
    setEditing(db.id);
  }

  async function save() {
    const cidrs = form.allowed_cidrs.split(',').map(s => s.trim()).filter(Boolean);
    setSaving(true);
    try {
      if (editing === 'new') {
        await api.createClusterDatabase(clusterId, {
          db_name: form.db_name,
          db_user: form.db_user,
          password: form.password,
          allowed_cidrs: cidrs,
        });
        toast('Database creation requested — provisioning in progress');
      } else {
        await api.updateClusterDatabase(clusterId, editing, {
          db_user: form.db_user,
          password: form.password || undefined,
          allowed_cidrs: cidrs,
        });
        toast('Database updated successfully');
      }
      setEditing(null);
      await load();
    } catch (e) {
      toast('Failed: ' + e.message, true);
    } finally {
      setSaving(false);
    }
  }

  async function remove(db) {
    if (!confirm(`Delete database "${db.db_name}" and user "${db.db_user}"? This does not drop the actual PostgreSQL database — it removes the management record and HBA access rules.`)) return;
    try {
      await api.deleteClusterDatabase(clusterId, db.id);
      toast('Database record removed');
      load();
    } catch (e) {
      toast('Delete failed: ' + e.message, true);
    }
  }

  return (
    <div className="cd-card">
      <div className="cd-card-header" style={{ justifyContent: 'space-between' }}>
        <span style={{ display: 'flex', alignItems: 'center', gap: 6 }}><Database size={14} /> Databases</span>
        <button className="btn btn-sm btn-approve" onClick={startCreate}><Plus size={12} /> Add Database</button>
      </div>
      <div className="cd-card-body" style={{ padding: 0 }}>
        {editing && (
          <div className="admin-form-bar">
            <input className="input" placeholder="Database name" value={form.db_name}
              onChange={e => setForm(f => ({ ...f, db_name: e.target.value }))}
              disabled={editing !== 'new'} style={{ width: 160 }} />
            <input className="input" placeholder="Username" value={form.db_user}
              onChange={e => setForm(f => ({ ...f, db_user: e.target.value }))} style={{ width: 140 }} />
            <input className="input" type="password" placeholder={editing === 'new' ? 'Password' : 'New password (leave blank to keep)'}
              value={form.password}
              onChange={e => setForm(f => ({ ...f, password: e.target.value }))} style={{ width: 180 }} />
            <input className="input" placeholder="Allowed CIDRs (e.g. 10.0.0.0/8, 192.168.0.0/16)" value={form.allowed_cidrs}
              onChange={e => setForm(f => ({ ...f, allowed_cidrs: e.target.value }))} style={{ flex: 1 }} />
            <button className="btn btn-approve" onClick={save} disabled={saving}>
              {saving ? <><Loader size={13} className="spin" /> Saving...</> : <><Save size={13} /> Save</>}
            </button>
            <button className="btn btn-reject" onClick={() => setEditing(null)} disabled={saving}><X size={13} /></button>
          </div>
        )}

        {dbs.length === 0 && !editing ? (
          <div className="cd-empty">No databases managed at the cluster level. Click "Add Database" to create one.</div>
        ) : (
          <table>
            <thead>
              <tr>
                <th>Database</th>
                <th>User</th>
                <th>Status</th>
                <th>Allowed CIDRs</th>
                <th>Created</th>
                <th style={{ width: 150 }}>Actions</th>
              </tr>
            </thead>
            <tbody>
              {dbs.map(db => (
                <tr key={db.id}>
                  <td className="mono">{db.db_name}</td>
                  <td className="mono">{db.db_user}</td>
                  <td>
                    {db.status === 'created' && <span className="badge badge-green"><span className="dot" />Created</span>}
                    {db.status === 'failed' && <span className="badge badge-red" title={db.error_message || ''}><span className="dot" />Failed</span>}
                    {(!db.status || db.status === 'pending') && <span className="badge badge-amber"><span className="dot" />Pending</span>}
                  </td>
                  <td>
                    {(db.allowed_cidrs || []).length === 0
                      ? <span className="sm muted">All (0.0.0.0/0)</span>
                      : (db.allowed_cidrs || []).map((cidr, i) => (
                          <span key={i} className="badge badge-blue" style={{ marginRight: 4 }}>
                            <Globe size={10} /> {cidr}
                          </span>
                        ))
                    }
                  </td>
                  <td className="sm muted">{timeAgo(db.created_at)}</td>
                  <td>
                    <div className="actions">
                      <button className="btn btn-sm" onClick={() => startEdit(db)}><Pencil size={12} /> Edit</button>
                      <button className="btn btn-sm btn-reject" onClick={() => remove(db)}><Trash2 size={12} /></button>
                    </div>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </div>
    </div>
  );
}

