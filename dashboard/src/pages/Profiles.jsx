import { useState, useMemo, useEffect } from 'react';
import { useData } from '../context/DataContext';
import { useToast } from '../context/ToastContext';
import { api, parseSpec, timeAgo } from '../api';
import { History, X, Tag } from 'lucide-react';

// ── PostgreSQL parameter catalog ────────────────────────────────────────────
// Organised by category. Each entry: [key, defaultValue, description].
// "mandatory" params are set by the operator and shown read-only.

const MANDATORY_PARAMS = {
  listen_addresses: { value: "'*'", desc: 'IP addresses to listen on (operator-managed)' },
  wal_level: { value: 'replica', desc: 'WAL level for replication (operator-managed)' },
  max_wal_senders: { value: '10', desc: 'Max concurrent WAL sender processes (operator-managed)' },
  max_replication_slots: { value: '10', desc: 'Max replication slots (operator-managed)' },
  hot_standby: { value: 'on', desc: 'Allow queries on standby (operator-managed)' },
  wal_log_hints: { value: 'on', desc: 'WAL-log hint bit changes (operator-managed)' },
  max_slot_wal_keep_size: { value: '-1', desc: 'Max WAL kept for replication slots (operator-managed)' },
};

const PG_PARAM_CATALOG = [
  {
    category: 'Connections',
    params: [
      ['max_connections', '100', 'Max concurrent connections'],
      ['superuser_reserved_connections', '3', 'Connections reserved for superusers'],
      ['tcp_keepalives_idle', '60', 'Seconds before sending keepalive on idle connection'],
      ['tcp_keepalives_interval', '10', 'Seconds between keepalive retransmits'],
      ['tcp_keepalives_count', '10', 'Max keepalive retransmits before dropping'],
    ],
  },
  {
    category: 'Memory',
    params: [
      ['shared_buffers', '256MB', 'Memory for shared buffer cache'],
      ['effective_cache_size', '768MB', 'Planner estimate of total cache available'],
      ['work_mem', '4MB', 'Memory per sort/hash operation'],
      ['maintenance_work_mem', '128MB', 'Memory for maintenance operations (VACUUM, CREATE INDEX)'],
      ['temp_buffers', '8MB', 'Memory for temporary table access per session'],
      ['huge_pages', 'try', 'Use huge pages (try, on, off)'],
    ],
  },
  {
    category: 'WAL',
    params: [
      ['wal_buffers', '8MB', 'Shared memory for WAL data'],
      ['min_wal_size', '1GB', 'Minimum WAL disk space retained'],
      ['max_wal_size', '4GB', 'WAL size to trigger checkpoint'],
      ['max_slot_wal_keep_size', '-1', 'Max WAL kept for replication slots (-1 = unlimited)'],
      ['wal_keep_size', '1GB', 'Minimum WAL retained for standby servers'],
      ['wal_compression', 'off', 'Compress full-page writes in WAL (off, pglz, lz4, zstd)'],
      ['wal_writer_delay', '200ms', 'Delay between WAL writer flushes'],
      ['commit_delay', '0', 'Microseconds to wait before WAL flush'],
      ['commit_siblings', '5', 'Min concurrent txns before applying commit_delay'],
    ],
  },
  {
    category: 'Checkpoints',
    params: [
      ['checkpoint_completion_target', '0.9', 'Fraction of checkpoint interval for spreading writes'],
      ['checkpoint_timeout', '5min', 'Max time between automatic checkpoints'],
      ['checkpoint_warning', '30s', 'Warn if checkpoints happen more frequently'],
    ],
  },
  {
    category: 'Query Planner',
    params: [
      ['random_page_cost', '1.1', 'Planner cost estimate for random page fetch'],
      ['seq_page_cost', '1.0', 'Planner cost estimate for sequential page fetch'],
      ['effective_io_concurrency', '200', 'Concurrent I/O operations for bitmap heap scans'],
      ['default_statistics_target', '100', 'Default statistics sample size for ANALYZE'],
      ['jit', 'on', 'Enable JIT compilation'],
    ],
  },
  {
    category: 'Replication',
    params: [
      ['track_commit_timestamp', 'on', 'Record commit timestamps for transactions'],
      ['synchronous_commit', 'on', 'Synchronous commit level (on, off, local, remote_write, remote_apply)'],
      ['wal_receiver_timeout', '60s', 'Max wait time for WAL data from primary'],
      ['wal_sender_timeout', '60s', 'Max time to wait for WAL replication'],
    ],
  },
  {
    category: 'Logging',
    params: [
      ['log_min_duration_statement', '200', 'Log statements exceeding this many ms (-1 = off)'],
      ['log_statement', 'none', 'Which SQL statements to log (none, ddl, mod, all)'],
      ['log_line_prefix', "'%m [%p] %q[user=%u,db=%d] '", 'Printf-style prefix for log lines'],
      ['log_checkpoints', 'on', 'Log checkpoint statistics'],
      ['log_connections', 'off', 'Log new connections'],
      ['log_disconnections', 'off', 'Log session disconnections'],
      ['log_lock_waits', 'off', 'Log lock waits longer than deadlock_timeout'],
      ['log_temp_files', '-1', 'Log temp file usage above this many kB (-1 = off)'],
      ['log_autovacuum_min_duration', '-1', 'Log autovacuum longer than this many ms (-1 = off)'],
    ],
  },
  {
    category: 'Autovacuum',
    params: [
      ['autovacuum', 'on', 'Enable autovacuum daemon'],
      ['autovacuum_max_workers', '3', 'Max autovacuum worker processes'],
      ['autovacuum_naptime', '1min', 'Time between autovacuum runs'],
      ['autovacuum_vacuum_threshold', '50', 'Min row updates before vacuum'],
      ['autovacuum_vacuum_scale_factor', '0.2', 'Fraction of table size to add to threshold'],
      ['autovacuum_analyze_threshold', '50', 'Min row updates before analyze'],
      ['autovacuum_analyze_scale_factor', '0.1', 'Fraction of table size to add to analyze threshold'],
    ],
  },
  {
    category: 'Client Defaults',
    params: [
      ['timezone', "'UTC'", 'Default timezone for sessions'],
      ['statement_timeout', '0', 'Max statement execution time in ms (0 = off)'],
      ['idle_in_transaction_session_timeout', '0', 'Max idle time in transaction in ms (0 = off)'],
      ['lock_timeout', '0', 'Max lock wait time in ms (0 = off)'],
      ['default_text_search_config', "'pg_catalog.english'", 'Default text search config'],
    ],
  },
];

// Build flat default map from catalog for emptySpec
const DEFAULT_PG_PARAMS = {};
for (const group of PG_PARAM_CATALOG) {
  for (const [key, defaultVal] of group.params) {
    DEFAULT_PG_PARAMS[key] = defaultVal;
  }
}

const DEFAULT_HBA_RULES = [
  'local   all   all                 trust',
  'host    all   all   127.0.0.1/32  scram-sha-256',
  'host    all   all   ::1/128       scram-sha-256',
  'host    all   all   0.0.0.0/0     scram-sha-256',
];

function emptySpec() {
  return {
    replicas: 3,
    postgres: { version: '17', variant: 'alpine', registry: '', image: '' },
    storage: { size: '10Gi', storage_class: '' },
    wal_storage: null,
    resources: { cpu_request: '250m', cpu_limit: '1000m', memory_request: '512Mi', memory_limit: '1Gi' },
    pg_params: { ...DEFAULT_PG_PARAMS },
    hba_rules: [...DEFAULT_HBA_RULES],
    failover: { enabled: true, health_check_interval_seconds: 5 },
    backup: null,
  };
}

export default function Profiles() {
  const { profiles, postgresVersions, postgresVariants, clusters, storageTiers, recoveryRuleSets, backupStores, refresh } = useData();
  const toast = useToast();

  useEffect(() => { document.title = 'Profiles - PG-Swarm'; }, []);
  const [editing, setEditing] = useState(null);

  const [cloneName, setCloneName] = useState('');
  const [cloneTarget, setCloneTarget] = useState(null);
  const [cascadeTarget, setCascadeTarget] = useState(null);
  const [profileVersions, setProfileVersions] = useState({}); // { profileId: [versions] }
  const [historyTarget, setHistoryTarget] = useState(null); // profile being viewed

  // Fetch latest version number for each profile
  useEffect(() => {
    if (!profiles.length) return;
    Promise.all(profiles.map(p =>
      api.profileVersions(p.id).then(vs => [p.id, vs]).catch(() => [p.id, []])
    )).then(results => {
      const map = {};
      results.forEach(([pid, vs]) => { map[pid] = vs; });
      setProfileVersions(map);
    });
  }, [profiles]);
  function startCreate() {
    setEditing({ name: '', description: '', spec: emptySpec(), isNew: true });
  }

  function startEdit(profile) {
    const spec = parseSpec(profile.config);
    const pg_params = { ...DEFAULT_PG_PARAMS, ...spec.pg_params };
    const hba_rules = spec.hba_rules?.length ? spec.hba_rules : [...DEFAULT_HBA_RULES];
    const backup = spec.backup || null;
    const merged = { ...emptySpec(), ...spec, pg_params, hba_rules, backup };
    setEditing({
      id: profile.id,
      name: profile.name,
      description: profile.description,
      recovery_rule_set_id: profile.recovery_rule_set_id || null,
      spec: merged,
      originalSpec: JSON.parse(JSON.stringify(merged)),
      isNew: false,
    });
  }


  function clusterCountForProfile(profileId) {
    return (clusters || []).filter(c => c.profile_id === profileId).length;
  }

  async function save() {
    const payload = {
      name: editing.name,
      description: editing.description,
      config: editing.spec,
      recovery_rule_set_id: editing.recovery_rule_set_id || null,
    };
    try {
      if (editing.isNew) {
        await api.createProfile(payload);
        toast('Profile created');
      } else {
        const result = await api.updateProfile(editing.id, payload);
        if (result.change_impact) {
          toast('Profile updated — ' + result.change_impact.apply_strategy.replace(/_/g, ' ') + ' required on ' + result.change_impact.affected_clusters.length + ' cluster(s)');
        } else {
          toast('Profile updated');
        }
      }
      setEditing(null);
      refresh();
    } catch (e) {
      // Surface immutable field errors in the confirmation modal
      if (e.body?.immutable_errors) {
        return { immutableErrors: e.body.immutable_errors, error: e.body.error };
      }
      toast('Save failed: ' + e.message, true);
    }
  }

  function remove(profile) {
    setCascadeTarget(profile);
  }

  async function confirmDelete() {
    if (!cascadeTarget) return;
    try {
      await api.deleteProfile(cascadeTarget.id);
      toast('Profile deleted');
      setCascadeTarget(null);
      refresh();
    } catch (e) {
      toast('Delete failed: ' + e.message, true);
    }
  }

  async function doClone() {
    try {
      await api.cloneProfile(cloneTarget, cloneName);
      toast('Profile cloned');
      setCloneTarget(null);
      setCloneName('');
      refresh();
    } catch (e) {
      toast('Clone failed: ' + e.message, true);
    }
  }

  if (editing) {
    return <ProfileForm state={editing} setState={setEditing} onSave={save} onCancel={() => setEditing(null)} postgresVersions={postgresVersions} postgresVariants={postgresVariants} storageTiers={storageTiers} recoveryRuleSets={recoveryRuleSets} backupStores={backupStores} />;
  }

  return (
    <>
      <div className="card-head-bar">
        <span className="card-head-title">Cluster Profiles</span>
        <button className="btn btn-approve" onClick={startCreate}>+ New Profile</button>
      </div>

      {cloneTarget && (
        <div className="clone-bar">
          <input
            className="input"
            placeholder="New profile name"
            value={cloneName}
            onChange={e => setCloneName(e.target.value)}
          />
          <button className="btn btn-approve" onClick={doClone} disabled={!cloneName.trim()}>Clone</button>
          <button className="btn btn-reject" onClick={() => setCloneTarget(null)}>Cancel</button>
        </div>
      )}

      <div className="profile-grid">
        {profiles.length === 0 ? (
          <div className="empty">No profiles created yet</div>
        ) : profiles.map(p => {
          const spec = parseSpec(p.config);
          const clusterCount = clusterCountForProfile(p.id);
          const versions = profileVersions[p.id] || [];
          const latestVersion = versions.length > 0 ? versions[0].version : 0;
          return (
            <div className="cl-card" key={p.id}>
              <div className="cl-head">
                <h3>{p.name}</h3>
                <div className="badges">
                  {latestVersion > 0 && (
                    <span className="badge badge-blue" title={`Version ${latestVersion} — ${versions.length} revision${versions.length !== 1 ? 's' : ''}`}>
                      <Tag size={10} />v{latestVersion}
                    </span>
                  )}
                  {p.locked
                    ? <span className="badge badge-amber" title="Profile is in use by active clusters or deployment rules"><span className="dot" />In Use</span>
                    : <span className="badge badge-green"><span className="dot" />Editable</span>}
                  {clusterCount > 0 && (
                    <span className="badge badge-gray">{clusterCount} cluster{clusterCount !== 1 ? 's' : ''}</span>
                  )}
                </div>
              </div>
              <div className="cl-body">
                {p.description && <p className="sm muted" style={{ marginBottom: 8 }}>{p.description}</p>}
                <dl className="cl-grid">
                  <KV label="Replicas" value={spec.replicas || '-'} />
                  <KV label="PostgreSQL" value={spec.postgres?.version ? `${spec.postgres.version} ${spec.postgres.variant || 'alpine'}` : '-'} />
                  <KV label="Storage" value={spec.storage?.size || '-'} />
                  <KV label="CPU" value={`${spec.resources?.cpu_request || '-'} / ${spec.resources?.cpu_limit || '-'}`} />
                  <KV label="Memory" value={`${spec.resources?.memory_request || '-'} / ${spec.resources?.memory_limit || '-'}`} />
                </dl>
                <div className="cl-tags">
                  {spec.replicas > 1
                    ? <span className="tag">{spec.replicas} replicas</span>
                    : <span className="tag">standalone</span>}
                  {spec.failover?.enabled && <span className="tag">failover</span>}
                  {spec.archive?.mode && <span className="tag">archive:{spec.archive.mode}</span>}
                  {Object.keys(spec.pg_params || {}).length > 0 && <span className="tag">{Object.keys(spec.pg_params).length} pg params</span>}
                  {(spec.hba_rules || []).length > 0 && <span className="tag">{spec.hba_rules.length} hba rules</span>}
                  {p.recovery_rule_set_id && (() => {
                    const rs = recoveryRuleSets.find(r => r.id === p.recovery_rule_set_id);
                    return rs ? <span className="tag">recovery: {rs.name}</span> : null;
                  })()}
                </div>
              </div>
              <div className="cl-foot">
                <span>{timeAgo(p.created_at)}</span>
                <span className="actions" style={{ marginLeft: 'auto' }}>
                  {versions.length > 0 && (
                    <button className="btn btn-sm btn-blue" onClick={() => setHistoryTarget(p)} style={{ display: 'inline-flex', alignItems: 'center', gap: 4 }}>
                      <History size={11} />History
                    </button>
                  )}
                  <button className="btn btn-sm" onClick={() => startEdit(p)}>Edit</button>
                  <button className="btn btn-sm" onClick={() => { setCloneTarget(p.id); setCloneName(p.name + '-copy'); }}>Clone</button>
                  <button className="btn btn-sm btn-reject" onClick={() => remove(p)}>Delete</button>
                </span>
              </div>
            </div>
          );
        })}
      </div>

      {cascadeTarget && (() => {
        const attachedCount = clusterCountForProfile(cascadeTarget.id);
        return (
          <div className="confirm-overlay" onClick={() => setCascadeTarget(null)}>
            <div className="confirm-modal" onClick={e => e.stopPropagation()} style={{ maxWidth: 460 }}>
              <div className="confirm-header">
                <h3>Delete Profile</h3>
                <button className="modal-close" onClick={() => setCascadeTarget(null)}><X size={18} /></button>
              </div>
              <div className="confirm-body">
                {attachedCount > 0 ? (
                  <>
                    <p>Cannot delete <strong>{cascadeTarget.name}</strong> because it is in use.</p>
                    <p className="muted" style={{ fontSize: 12.5, marginTop: 8 }}>
                      {attachedCount} cluster{attachedCount !== 1 ? 's are' : ' is'} currently using this profile.
                      Remove or reassign all clusters before deleting.
                    </p>
                  </>
                ) : (
                  <>
                    <p>Are you sure you want to delete <strong>{cascadeTarget.name}</strong>?</p>
                    {cascadeTarget.description && (
                      <p className="muted" style={{ fontSize: 12.5, marginTop: 6 }}>{cascadeTarget.description}</p>
                    )}
                    <p className="muted" style={{ fontSize: 12.5, marginTop: 8 }}>
                      This action cannot be undone. The profile and all its version history will be permanently removed.
                    </p>
                  </>
                )}
              </div>
              <div className="confirm-footer">
                <button className="btn btn-sm" onClick={() => setCascadeTarget(null)}>
                  {attachedCount > 0 ? 'Close' : 'Cancel'}
                </button>
                {attachedCount === 0 && (
                  <button className="btn btn-sm btn-danger" onClick={confirmDelete}>Delete Profile</button>
                )}
              </div>
            </div>
          </div>
        );
      })()}

      {/* Version history modal */}
      {historyTarget && (
        <div className="confirm-overlay" onClick={() => setHistoryTarget(null)}>
          <div className="confirm-modal" onClick={e => e.stopPropagation()} style={{ width: 820, maxWidth: '95vw' }}>
            <div className="confirm-header">
              <h3><History size={18} /> Version History: {historyTarget.name}</h3>
              <button className="modal-close" onClick={() => setHistoryTarget(null)}><X size={18} /></button>
            </div>
            <div className="confirm-body" style={{ padding: 0 }}>
              <VersionHistoryList versions={profileVersions[historyTarget.id] || []} />
            </div>
            <div className="confirm-footer">
              <button className="btn btn-sm" onClick={() => setHistoryTarget(null)}>Close</button>
            </div>
          </div>
        </div>
      )}
    </>
  );
}

// Parse "param: old → new" or "param: old → new (restart)" from change_summary
function parseSummaryChanges(summary) {
  if (!summary || summary === 'no changes') return [];
  return summary.split('; ').map(part => {
    // "scale up to N replicas" / "scale down to N replicas"
    const scaleMatch = part.match(/^scale (up|down) to (\d+) replicas$/);
    if (scaleMatch) {
      return { param: 'replicas', oldVal: '', newVal: scaleMatch[2], strategy: 'scale ' + scaleMatch[1] };
    }
    // "Reverted to version N"
    if (part.startsWith('Reverted')) {
      return { param: part, oldVal: '', newVal: '', strategy: 'revert' };
    }
    // "param: old → new (strategy)"
    const m = part.match(/^(.+?):\s*(.+?)\s*→\s*(.+?)(?:\s*\((.+?)\))?$/);
    if (!m) return { param: part, oldVal: '', newVal: '', strategy: '' };
    return { param: m[1], oldVal: m[2], newVal: m[3], strategy: m[4] || 'sighup' };
  });
}

const strategyLabels = {
  sighup: { label: 'sighup', color: 'badge-green' },
  reload: { label: 'sighup', color: 'badge-green' },
  restart: { label: 'restart', color: 'badge-amber' },
  'full restart': { label: 'full restart', color: 'badge-red' },
  revert: { label: 'revert', color: 'badge-gray' },
  'scale up': { label: 'scale up', color: 'badge-blue' },
  'scale down': { label: 'scale down', color: 'badge-amber' },
};

const statusColors = {
  pending: 'badge-amber',
  applied: 'badge-green',
  failed: 'badge-red',
  reverted: 'badge-gray',
};

function VersionHistoryList({ versions }) {
  if (versions.length === 0) {
    return <p className="muted" style={{ padding: 16 }}>No version history available.</p>;
  }

  return (
    <div className="vh-list">
      {versions.map((v, i) => {
        const changes = parseSummaryChanges(v.change_summary);
        return (
          <div key={v.id} className="vh-entry">
            {/* Version header row */}
            <div className="vh-header">
              <span className="badge badge-blue" style={{ fontSize: 10.5 }}>
                <Tag size={9} />v{v.version}
              </span>
              <span className={`badge ${statusColors[v.apply_status] || 'badge-gray'}`} style={{ fontSize: 10.5 }}>
                {v.apply_status}
              </span>
              {i === 0 && <span className="badge badge-green" style={{ fontSize: 10.5, fontWeight: 600 }}>latest</span>}
              <span className="muted" style={{ fontSize: 11, marginLeft: 'auto' }}>
                {new Date(v.created_at).toLocaleString()}
              </span>
            </div>
            {/* Changes table */}
            {changes.length > 0 ? (
              <div className="vh-table-wrap">
                <table className="vh-table">
                  <thead>
                    <tr>
                      <th>Parameter</th>
                      <th>Before</th>
                      <th>After</th>
                      <th>Apply</th>
                    </tr>
                  </thead>
                  <tbody>
                    {changes.map((ch, j) => {
                      const st = strategyLabels[ch.strategy] || { label: ch.strategy || '-', color: 'badge-gray' };
                      return (
                        <tr key={j}>
                          <td className="mono">{ch.param}</td>
                          <td className="mono">
                            {ch.oldVal && (
                              <span style={{ background: 'var(--red-bg)', color: 'var(--red)', fontWeight: 600, padding: '1px 6px', borderRadius: 4, display: 'inline-block' }}>
                                {ch.oldVal}
                              </span>
                            )}
                          </td>
                          <td className="mono">
                            {ch.newVal && (
                              <span style={{ background: 'var(--green-light)', color: 'var(--green-dark)', fontWeight: 600, padding: '1px 6px', borderRadius: 4, display: 'inline-block' }}>
                                {ch.newVal}
                              </span>
                            )}
                          </td>
                          <td>
                            <span className={`badge ${st.color}`} style={{ fontSize: 10 }}>{st.label}</span>
                          </td>
                        </tr>
                      );
                    })}
                  </tbody>
                </table>
              </div>
            ) : (
              <div className="muted" style={{ fontSize: 12, padding: '4px 0' }}>
                {v.change_summary || 'Initial version'}
              </div>
            )}
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


// ── Tab definitions ─────────────────────────────────────────────────────────

const TABS = [
  { id: 'general', label: 'General' },
  { id: 'volumes', label: 'Volumes' },
  { id: 'resources', label: 'Resources' },
  { id: 'pgconfig', label: 'PostgreSQL' },
  { id: 'hba', label: 'HBA Rules' },
  { id: 'backup', label: 'Backup' },
];

const CRON_RE = /^(\S+\s+){4}\S+$/;
function isValidCron(s) { return !s || CRON_RE.test(s.trim()); }

// Parse a cron step field like "* /5", "0/5", or "*" and return the step value, or 0 if not a step.
function cronStep(field) {
  const m = field.match(/^(?:\*|\d+)\/(\d+)$/);
  return m ? parseInt(m[1]) : 0;
}

/** Convert a 5-field cron expression to a short human-readable string. */
function cronToText(expr) {
  if (!expr || !isValidCron(expr)) return '';
  const [min, hr, dom, mon, dow] = expr.trim().split(/\s+/);

  const minStep = cronStep(min);
  const hrStep = cronStep(hr);

  // Every N minutes: */5 (after start) or 0/5 (on the clock)
  if (minStep && hr === '*' && dom === '*' && mon === '*' && dow === '*') {
    if (minStep === 1) return 'Every minute';
    const qualifier = min.startsWith('*') ? ' after start' : ' on the clock';
    return `Every ${minStep} minutes${qualifier}`;
  }
  // Every N hours: */N (after start) or 0/N (on the clock)
  if (/^\d+$/.test(min) && hrStep && dom === '*' && mon === '*' && dow === '*') {
    if (hrStep === 1) return 'Every hour';
    const qualifier = hr.startsWith('*') ? ' after start' : ' on the clock';
    return `Every ${hrStep} hours${qualifier}`;
  }
  // Hourly at :MM: M * * * *
  if (/^\d+$/.test(min) && hr === '*' && dom === '*' && mon === '*' && dow === '*') {
    return `Hourly at :${min.padStart(2, '0')}`;
  }
  // Daily: 0 H * * *
  if (/^\d+$/.test(min) && /^\d+$/.test(hr) && dom === '*' && mon === '*' && dow === '*') {
    return `Daily at ${hr.padStart(2, '0')}:${min.padStart(2, '0')}`;
  }
  // Weekly: 0 H * * D
  if (/^\d+$/.test(min) && /^\d+$/.test(hr) && dom === '*' && mon === '*' && /^\d+$/.test(dow)) {
    const days = ['Sun', 'Mon', 'Tue', 'Wed', 'Thu', 'Fri', 'Sat'];
    return `${days[parseInt(dow)] || dow} at ${hr.padStart(2, '0')}:${min.padStart(2, '0')}`;
  }
  // Monthly: 0 H D * *
  if (/^\d+$/.test(min) && /^\d+$/.test(hr) && /^\d+$/.test(dom) && mon === '*' && dow === '*') {
    const d = parseInt(dom);
    const suffix = d === 1 || d === 21 || d === 31 ? 'st' : d === 2 || d === 22 ? 'nd' : d === 3 || d === 23 ? 'rd' : 'th';
    return `Monthly on ${d}${suffix} at ${hr.padStart(2, '0')}:${min.padStart(2, '0')}`;
  }
  return '';
}

// ── Profile Form ────────────────────────────────────────────────────────────

function ProfileForm({ state, setState, onSave, onCancel, postgresVersions, postgresVariants, storageTiers, recoveryRuleSets, backupStores }) {
  const spec = state.spec;
  const [activeTab, setActiveTab] = useState('general');
  const [showConfirm, setShowConfirm] = useState(false);
  const [saving, setSaving] = useState(false);
  const [saveError, setSaveError] = useState(null); // { error, immutableErrors }
  const [pgSearch, setPgSearch] = useState('');
  const [collapsedCategories, setCollapsedCategories] = useState({});

  // Build version options from postgres_versions table
  const versionOptions = useMemo(() => {
    const opts = [];
    const seen = new Set();
    for (const pv of (postgresVersions || [])) {
      const val = `${pv.version}|${pv.variant}`;
      if (!seen.has(val)) {
        seen.add(val);
        opts.push({ version: pv.version, variant: pv.variant, label: `${pv.version} ${pv.variant.charAt(0).toUpperCase() + pv.variant.slice(1)}`, image_tag: pv.image_tag });
      }
    }
    // Ensure the currently saved version is always in the list
    const currentVariant = spec.postgres?.variant || 'alpine';
    const currentVal = `${spec.postgres?.version}|${currentVariant}`;
    if (spec.postgres?.version && !seen.has(currentVal)) {
      opts.unshift({ version: spec.postgres.version, variant: currentVariant, label: `${spec.postgres.version} ${currentVariant.charAt(0).toUpperCase() + currentVariant.slice(1)}` });
    }
    return opts;
  }, [postgresVersions, spec.postgres?.version, spec.postgres?.variant]);

  function setSpec(fn) {
    setState(prev => ({ ...prev, spec: fn(prev.spec) }));
  }

  function setField(path, value) {
    setState(prev => ({ ...prev, [path]: value }));
  }

  function setPgParam(key, value) {
    setSpec(s => ({ ...s, pg_params: { ...s.pg_params, [key]: value } }));
  }

  function removePgParam(key) {
    setSpec(s => {
      const copy = { ...s.pg_params };
      delete copy[key];
      return { ...s, pg_params: copy };
    });
  }

  function addPgParam() {
    const key = prompt('Parameter name:');
    if (key && !spec.pg_params[key]) {
      setPgParam(key, '');
    }
  }

  function toggleCategory(cat) {
    setCollapsedCategories(prev => ({ ...prev, [cat]: !prev[cat] }));
  }

  // HBA helpers
  function parseHbaRule(rule) {
    const parts = rule.trim().split(/\s+/);
    if (parts[0] === 'local')
      return { type: 'local', database: parts[1]||'', user: parts[2]||'', address: '', method: parts[3]||'' };
    return { type: parts[0]||'', database: parts[1]||'', user: parts[2]||'', address: parts[3]||'', method: parts[4]||'' };
  }

  function formatHbaRule({ type, database, user, address, method }) {
    if (type === 'local') return `local ${database} ${user} ${method}`.trim();
    return `${type} ${database} ${user} ${address} ${method}`.trim();
  }

  function setHbaField(idx, field, value) {
    setSpec(s => {
      const rules = [...s.hba_rules];
      const parsed = parseHbaRule(rules[idx] || '');
      parsed[field] = value;
      if (field === 'type' && value === 'local') parsed.address = '';
      rules[idx] = formatHbaRule(parsed);
      return { ...s, hba_rules: rules };
    });
  }

  function addHbaRule() {
    setSpec(s => ({ ...s, hba_rules: [...s.hba_rules, 'host all all 0.0.0.0/0 md5'] }));
  }

  function removeHbaRule(idx) {
    setSpec(s => ({ ...s, hba_rules: s.hba_rules.filter((_, i) => i !== idx) }));
  }

  function handleSave() {
    setShowConfirm(true);
  }

  async function confirmSave() {
    setSaving(true);
    setSaveError(null);
    try {
      const result = await onSave();
      if (result?.immutableErrors) {
        setSaveError(result);
      }
    } finally {
      setSaving(false);
    }
  }

  // Count non-default pg_params for badge
  const changedParams = Object.entries(spec.pg_params).filter(([k, v]) => DEFAULT_PG_PARAMS[k] !== v);
  // Custom params (not in catalog)
  const catalogKeys = new Set(PG_PARAM_CATALOG.flatMap(g => g.params.map(p => p[0])));
  const customParams = Object.entries(spec.pg_params).filter(([k]) => !catalogKeys.has(k));

  return (
    <div className="profile-form">
      {/* Header */}
      <div className="card-head-bar">
        <span className="card-head-title">{state.isNew ? 'Create Profile' : 'Edit Profile'}</span>
        <div className="actions">
          <button className="btn btn-approve" onClick={handleSave}>Save</button>
          <button className="btn btn-reject" onClick={onCancel}>Cancel</button>
        </div>
      </div>

      {/* Tab bar */}
      <div className="tab-bar">
        {TABS.map(tab => (
          <button
            key={tab.id}
            className={'tab-item' + (activeTab === tab.id ? ' active' : '')}
            onClick={() => setActiveTab(tab.id)}
          >
            {tab.label}
            {tab.id === 'pgconfig' && changedParams.length > 0 && (
              <span className="tab-badge">{changedParams.length}</span>
            )}
            {tab.id === 'hba' && spec.hba_rules.length > 0 && (
              <span className="tab-badge">{spec.hba_rules.length}</span>
            )}
          </button>
        ))}
      </div>

      {/* Tab content */}
      <div className="tab-content">
        {activeTab === 'general' && (
          <section className="form-section">
            <h4>Profile</h4>
            <div className="form-grid">
              <div className="form-row">
                <label>Name</label>
                <input className="input" value={state.name} onChange={e => setField('name', e.target.value)} placeholder="e.g. production-standard" />
              </div>
              <div className="form-row">
                <label>Description</label>
                <input className="input" value={state.description} onChange={e => setField('description', e.target.value)} placeholder="Optional description" />
              </div>
              <div className="form-row">
                <label>Replicas</label>
                <input className="input" type="number" min="1" value={spec.replicas} onChange={e => setSpec(s => ({ ...s, replicas: parseInt(e.target.value) || 1 }))} />
              </div>
              <div className="form-row">
                <label>PostgreSQL Version</label>
                <select className="input" value={`${spec.postgres.version}|${spec.postgres.variant || 'alpine'}`}
                  onChange={e => {
                    const [v, var_] = e.target.value.split('|');
                    setSpec(s => ({ ...s, postgres: { ...s.postgres, version: v, variant: var_ } }));
                  }}>
                  {versionOptions.length === 0
                    ? <option value={`${spec.postgres.version}|${spec.postgres.variant || 'alpine'}`}>{spec.postgres.version} {spec.postgres.variant || 'alpine'}</option>
                    : versionOptions.map(o => (
                      <option key={`${o.version}|${o.variant}`} value={`${o.version}|${o.variant}`}>{o.label}</option>
                    ))}
                </select>
              </div>
              <div className="form-row">
                <label>Registry (optional)</label>
                <input className="input" value={spec.postgres.registry || ''} onChange={e => setSpec(s => ({ ...s, postgres: { ...s.postgres, registry: e.target.value } }))} placeholder="Docker Hub (default)" />
              </div>
              <div className="form-row">
                <label>Failover</label>
                <label className="toggle">
                  <input type="checkbox" checked={spec.failover?.enabled || false} onChange={e => setSpec(s => ({ ...s, failover: { ...s.failover, enabled: e.target.checked } }))} />
                  <span>Enabled</span>
                </label>
              </div>
              <div className="form-row">
                <label>Recovery RuleSet</label>
                <select className="input" value={state.recovery_rule_set_id || ''} onChange={e => setState(prev => ({ ...prev, recovery_rule_set_id: e.target.value || null }))}>
                  <option value="">None</option>
                  {(recoveryRuleSets || []).map(rs => (
                    <option key={rs.id} value={rs.id}>{rs.name} ({rs.rules?.length || 0} rules)</option>
                  ))}
                </select>
              </div>
            </div>
          </section>
        )}

        {activeTab === 'volumes' && (
          <section className="form-section">
            <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between' }}>
              <h4>Volumes</h4>
            </div>
            <div className="volume-section">
              <div className="volume-toggle">
                <h5>Deletion Protection</h5>
                <label className="toggle">
                  <input type="checkbox" checked={!!spec.deletion_protection} onChange={e => setSpec(s => ({ ...s, deletion_protection: e.target.checked }))} />
                  <span>Add finalizer to PVCs to prevent accidental deletion</span>
                </label>
              </div>
            </div>
            <div className="volume-section">
              <h5>Data Volume</h5>
              <div className="form-grid">
                <div className="form-row">
                  <label>Size</label>
                  <input className="input" value={spec.storage.size} onChange={e => setSpec(s => ({ ...s, storage: { ...s.storage, size: e.target.value } }))} />
                </div>
                <div className="form-row">
                  <label>Storage Tier</label>
                  <StorageClassSelect value={spec.storage.storage_class} storageTiers={storageTiers} onChange={v => setSpec(s => ({ ...s, storage: { ...s.storage, storage_class: v } }))} />
                </div>
              </div>
            </div>
            <div className="volume-section">
              <div className="volume-toggle">
                <h5>WAL Volume</h5>
                <label className="toggle">
                  <input type="checkbox" checked={!!spec.wal_storage} onChange={e => setSpec(s => ({ ...s, wal_storage: e.target.checked ? { size: '2Gi', storage_class: '' } : null }))} />
                  <span>Separate WAL volume</span>
                </label>
              </div>
              {spec.wal_storage && (
                <div className="form-grid">
                  <div className="form-row">
                    <label>Size</label>
                    <input className="input" value={spec.wal_storage.size} onChange={e => setSpec(s => ({ ...s, wal_storage: { ...s.wal_storage, size: e.target.value } }))} />
                  </div>
                  <div className="form-row">
                    <label>Storage Tier</label>
                    <StorageClassSelect value={spec.wal_storage.storage_class} storageTiers={storageTiers} onChange={v => setSpec(s => ({ ...s, wal_storage: { ...s.wal_storage, storage_class: v } }))} />
                  </div>
                </div>
              )}
            </div>
          </section>
        )}

        {activeTab === 'resources' && (
          <section className="form-section">
            <h4>Resource Configuration</h4>
            <div className="form-grid">
              <div className="form-row">
                <label>CPU Request</label>
                <input className="input" value={spec.resources.cpu_request} onChange={e => setSpec(s => ({ ...s, resources: { ...s.resources, cpu_request: e.target.value } }))} />
              </div>
              <div className="form-row">
                <label>CPU Limit</label>
                <input className="input" value={spec.resources.cpu_limit} onChange={e => setSpec(s => ({ ...s, resources: { ...s.resources, cpu_limit: e.target.value } }))} />
              </div>
              <div className="form-row">
                <label>Memory Request</label>
                <input className="input" value={spec.resources.memory_request} onChange={e => setSpec(s => ({ ...s, resources: { ...s.resources, memory_request: e.target.value } }))} />
              </div>
              <div className="form-row">
                <label>Memory Limit</label>
                <input className="input" value={spec.resources.memory_limit} onChange={e => setSpec(s => ({ ...s, resources: { ...s.resources, memory_limit: e.target.value } }))} />
              </div>
            </div>
          </section>
        )}

        {activeTab === 'pgconfig' && (
          <section className="form-section">
            <h4>PostgreSQL Configuration <span className="muted sm">(postgresql.conf)</span></h4>

            {/* Search */}
            <div className="pg-search-bar">
              <input
                className="input"
                placeholder="Search parameters..."
                value={pgSearch}
                onChange={e => setPgSearch(e.target.value)}
              />
              {changedParams.length > 0 && (
                <span className="muted sm">{changedParams.length} modified from defaults</span>
              )}
            </div>

            {/* Mandatory / operator-managed params */}
            <div className="pg-category">
              <div className="pg-category-head" onClick={() => toggleCategory('_mandatory')}>
                <span className="pg-category-arrow">{collapsedCategories['_mandatory'] ? '\u25b6' : '\u25bc'}</span>
                <span className="pg-category-title">Operator-Managed</span>
                <span className="muted sm">{Object.keys(MANDATORY_PARAMS).length} params (read-only)</span>
              </div>
              {!collapsedCategories['_mandatory'] && (
                <div className="pg-category-body">
                  {Object.entries(MANDATORY_PARAMS).map(([key, { value, desc }]) => (
                    <div className="param-row param-readonly" key={key} title={desc}>
                      <span className="mono param-key">{key}</span>
                      <span className="input param-value param-disabled">{spec.pg_params[key] || value}</span>
                      <span className="param-desc">{desc}</span>
                    </div>
                  ))}
                </div>
              )}
            </div>

            {/* Catalog categories */}
            {PG_PARAM_CATALOG.map(group => {
              const searchLower = pgSearch.toLowerCase();
              const filtered = group.params.filter(([key, , desc]) =>
                !pgSearch || key.includes(searchLower) || desc.toLowerCase().includes(searchLower)
              );
              if (filtered.length === 0) return null;
              const catKey = group.category;
              const isCollapsed = collapsedCategories[catKey];
              const modCount = filtered.filter(([k]) => spec.pg_params[k] !== undefined && spec.pg_params[k] !== DEFAULT_PG_PARAMS[k]).length;

              return (
                <div className="pg-category" key={catKey}>
                  <div className="pg-category-head" onClick={() => toggleCategory(catKey)}>
                    <span className="pg-category-arrow">{isCollapsed ? '\u25b6' : '\u25bc'}</span>
                    <span className="pg-category-title">{catKey}</span>
                    <span className="muted sm">{filtered.length} params</span>
                    {modCount > 0 && <span className="tab-badge">{modCount}</span>}
                  </div>
                  {!isCollapsed && (
                    <div className="pg-category-body">
                      {filtered.map(([key, defaultVal, desc]) => {
                        const current = spec.pg_params[key];
                        const isSet = current !== undefined;
                        const isModified = isSet && current !== defaultVal;
                        return (
                          <div className={'param-row' + (isModified ? ' param-modified' : '')} key={key} title={desc}>
                            <span className="mono param-key">{key}</span>
                            <input
                              className="input param-value"
                              value={isSet ? current : defaultVal}
                              onChange={e => setPgParam(key, e.target.value)}
                              placeholder={defaultVal}
                            />
                            <span className="param-desc">{desc}</span>
                            {isSet && (
                              <button className="btn-icon" onClick={() => removePgParam(key)} title="Reset to default">&times;</button>
                            )}
                          </div>
                        );
                      })}
                    </div>
                  )}
                </div>
              );
            })}

            {/* Custom (user-added) params not in catalog */}
            {customParams.length > 0 && (
              <div className="pg-category">
                <div className="pg-category-head">
                  <span className="pg-category-arrow">{'\u25bc'}</span>
                  <span className="pg-category-title">Custom Parameters</span>
                  <span className="muted sm">{customParams.length} params</span>
                </div>
                <div className="pg-category-body">
                  {customParams.map(([key, value]) => (
                    <div className="param-row param-modified" key={key}>
                      <span className="mono param-key">{key}</span>
                      <input className="input param-value" value={value} onChange={e => setPgParam(key, e.target.value)} />
                      <span className="param-desc"></span>
                      <button className="btn-icon" onClick={() => removePgParam(key)} title="Remove">&times;</button>
                    </div>
                  ))}
                </div>
              </div>
            )}

            <button className="btn btn-sm" onClick={addPgParam} style={{ marginTop: 8 }}>+ Add Custom Parameter</button>
          </section>
        )}

        {activeTab === 'hba' && (
          <section className="form-section">
            <h4>Client Authentication <span className="muted sm">(pg_hba.conf)</span></h4>
            <p className="muted sm" style={{ marginBottom: 10 }}>
              Mandatory rules (local trust, host md5, replication) are added automatically by the operator.
            </p>
            <table className="hba-table">
              <thead>
                <tr><th>TYPE</th><th>DATABASE</th><th>USER</th><th>ADDRESS</th><th>METHOD</th><th></th></tr>
              </thead>
              <tbody>
                {spec.hba_rules.map((rule, i) => {
                  const parsed = parseHbaRule(rule);
                  return (
                    <tr key={i}>
                      <td>
                        <select className="input" value={parsed.type} onChange={e => setHbaField(i, 'type', e.target.value)}>
                          <option value="local">local</option>
                          <option value="host">host</option>
                          <option value="hostssl">hostssl</option>
                          <option value="hostnossl">hostnossl</option>
                        </select>
                      </td>
                      <td><input className="input mono" value={parsed.database} onChange={e => setHbaField(i, 'database', e.target.value)} /></td>
                      <td><input className="input mono" value={parsed.user} onChange={e => setHbaField(i, 'user', e.target.value)} /></td>
                      <td><input className="input mono" value={parsed.address} onChange={e => setHbaField(i, 'address', e.target.value)} disabled={parsed.type === 'local'} /></td>
                      <td><input className="input mono" value={parsed.method} onChange={e => setHbaField(i, 'method', e.target.value)} /></td>
                      <td><button className="btn-icon" onClick={() => removeHbaRule(i)}>&times;</button></td>
                    </tr>
                  );
                })}
              </tbody>
            </table>
            <button className="btn btn-sm" onClick={addHbaRule} style={{ marginTop: 6 }}>+ Add Rule</button>
          </section>
        )}

        {activeTab === 'backup' && (
          <BackupTab spec={spec} setSpec={setSpec} backupStores={backupStores || []} />
        )}

      </div>

      {/* Save confirmation modal */}
      {showConfirm && (
        <ConfirmReport
          state={state}
          spec={spec}
          changedParams={changedParams}
          onConfirm={confirmSave}
          onCancel={() => { setShowConfirm(false); setSaveError(null); }}
          saving={saving}
          saveError={saveError}
        />
      )}
    </div>
  );
}

// ── Spec Diff Helper ────────────────────────────────────────────────────────

function computeSpecChanges(oldSpec, newSpec) {
  if (!oldSpec) return [];
  const changes = [];

  function add(path, oldVal, newVal) {
    const o = String(oldVal ?? '');
    const n = String(newVal ?? '');
    if (o !== n) changes.push({ path, old_value: o, new_value: n });
  }

  // Top-level
  add('replicas', oldSpec.replicas, newSpec.replicas);

  // Postgres
  add('postgres.version', oldSpec.postgres?.version, newSpec.postgres?.version);
  add('postgres.variant', oldSpec.postgres?.variant, newSpec.postgres?.variant);
  add('postgres.registry', oldSpec.postgres?.registry, newSpec.postgres?.registry);

  // Storage
  add('storage.size', oldSpec.storage?.size, newSpec.storage?.size);
  add('storage.storage_class', oldSpec.storage?.storage_class, newSpec.storage?.storage_class);
  add('wal_storage.size', oldSpec.wal_storage?.size, newSpec.wal_storage?.size);
  add('wal_storage.storage_class', oldSpec.wal_storage?.storage_class, newSpec.wal_storage?.storage_class);

  // Resources
  add('resources.cpu_request', oldSpec.resources?.cpu_request, newSpec.resources?.cpu_request);
  add('resources.cpu_limit', oldSpec.resources?.cpu_limit, newSpec.resources?.cpu_limit);
  add('resources.memory_request', oldSpec.resources?.memory_request, newSpec.resources?.memory_request);
  add('resources.memory_limit', oldSpec.resources?.memory_limit, newSpec.resources?.memory_limit);

  // Failover
  add('failover.enabled', oldSpec.failover?.enabled, newSpec.failover?.enabled);
  add('failover.health_check_interval_seconds', oldSpec.failover?.health_check_interval_seconds, newSpec.failover?.health_check_interval_seconds);

  // PG Params
  const allKeys = new Set([...Object.keys(oldSpec.pg_params || {}), ...Object.keys(newSpec.pg_params || {})]);
  for (const k of allKeys) {
    add('pg_params.' + k, (oldSpec.pg_params || {})[k], (newSpec.pg_params || {})[k]);
  }

  // HBA rules
  const oldHba = (oldSpec.hba_rules || []).join('\n');
  const newHba = (newSpec.hba_rules || []).join('\n');
  if (oldHba !== newHba) {
    changes.push({ path: 'hba_rules', old_value: (oldSpec.hba_rules || []).length + ' rule(s)', new_value: (newSpec.hba_rules || []).length + ' rule(s)' });
  }

  return changes;
}

// ── Backup Tab ──────────────────────────────────────────────────────────────

function emptyBackup() {
  return {
    store_id: null,
    physical: { enabled: false, base_schedule: '0 2 * * 0', incremental_schedule: '', wal_archive_enabled: true, archive_timeout_seconds: 60 },
    logical: { enabled: false, schedule: '0 3 * * 0', databases: [], format: 'custom' },
    retention: { base_backup_count: 4, incremental_backup_count: 14, wal_retention_days: 14, logical_backup_count: 4 },
  };
}

function BackupTab({ spec, setSpec, backupStores }) {
  const backup = spec.backup;
  const enabled = backup != null;

  function toggle() {
    if (enabled) {
      setSpec(s => ({ ...s, backup: null }));
    } else {
      setSpec(s => ({ ...s, backup: emptyBackup() }));
    }
  }

  function setBackup(fn) {
    setSpec(s => ({ ...s, backup: fn(s.backup || emptyBackup()) }));
  }

  function setPhysical(fn) {
    setBackup(b => ({ ...b, physical: fn(b.physical) }));
  }

  function setLogical(fn) {
    setBackup(b => ({ ...b, logical: fn(b.logical) }));
  }

  function setRetention(fn) {
    setBackup(b => ({ ...b, retention: fn(b.retention) }));
  }

  const b = backup || emptyBackup();

  return (
    <section className="form-section">
      <h4>Backup Configuration</h4>

      <label className="toggle" style={{ marginBottom: 16 }}>
        <input type="checkbox" checked={enabled} onChange={toggle} />
        <span>Enable backups for clusters using this profile</span>
      </label>

      {enabled && (
        <>
          {/* Store selector */}
          <div className="form-grid" style={{ marginBottom: 20 }}>
            <div className="form-row">
              <label>Backup Store</label>
              <select className="input" value={b.store_id || ''}
                onChange={e => setBackup(prev => ({ ...prev, store_id: e.target.value || null }))}>
                <option value="">Select a store...</option>
                {backupStores.map(s => (
                  <option key={s.id} value={s.id}>
                    {s.name} ({s.store_type.toUpperCase()})
                  </option>
                ))}
              </select>
              {backupStores.length === 0 && (
                <span className="sm muted" style={{ marginTop: 4 }}>No backup stores configured. Add one in Admin &rarr; Backup Stores.</span>
              )}
            </div>
          </div>

          {/* Physical backups */}
          <h4 style={{ fontSize: 13, marginBottom: 8 }}>Physical Backups <span className="muted sm">(pg_basebackup + WAL)</span></h4>
          <label className="toggle" style={{ marginBottom: 10 }}>
            <input type="checkbox" checked={b.physical.enabled}
              onChange={e => setPhysical(p => ({ ...p, enabled: e.target.checked }))} />
            <span>Enable physical backups</span>
          </label>
          {b.physical.enabled && (
            <div className="form-grid" style={{ marginBottom: 20 }}>
              <div className="form-row">
                <label>Base schedule (cron)</label>
                <input className="input mono" value={b.physical.base_schedule}
                  onChange={e => setPhysical(p => ({ ...p, base_schedule: e.target.value }))}
                  placeholder="0 2 * * 0" />
                {b.physical.base_schedule && (
                  <span className="sm muted">{cronToText(b.physical.base_schedule)}</span>
                )}
              </div>
              <div className="form-row">
                <label>Incremental schedule (cron, optional)</label>
                <input className="input mono" value={b.physical.incremental_schedule}
                  onChange={e => setPhysical(p => ({ ...p, incremental_schedule: e.target.value }))}
                  placeholder="0 2 * * 1-6" />
                {b.physical.incremental_schedule && (
                  <span className="sm muted">{cronToText(b.physical.incremental_schedule)}</span>
                )}
              </div>
              <div className="form-row">
                <label className="toggle">
                  <input type="checkbox" checked={b.physical.wal_archive_enabled}
                    onChange={e => setPhysical(p => ({ ...p, wal_archive_enabled: e.target.checked }))} />
                  <span>WAL archiving</span>
                </label>
              </div>
              <div className="form-row">
                <label>Archive timeout (seconds)</label>
                <input className="input" type="number" min={0} value={b.physical.archive_timeout_seconds}
                  onChange={e => setPhysical(p => ({ ...p, archive_timeout_seconds: parseInt(e.target.value) || 60 }))}
                  style={{ width: 100 }} />
              </div>
            </div>
          )}

          {/* Logical backups */}
          <h4 style={{ fontSize: 13, marginBottom: 8 }}>Logical Backups <span className="muted sm">(pg_dump)</span></h4>
          <label className="toggle" style={{ marginBottom: 10 }}>
            <input type="checkbox" checked={b.logical.enabled}
              onChange={e => setLogical(l => ({ ...l, enabled: e.target.checked }))} />
            <span>Enable logical backups</span>
          </label>
          {b.logical.enabled && (
            <div className="form-grid" style={{ marginBottom: 20 }}>
              <div className="form-row">
                <label>Schedule (cron)</label>
                <input className="input mono" value={b.logical.schedule}
                  onChange={e => setLogical(l => ({ ...l, schedule: e.target.value }))}
                  placeholder="0 3 * * 0" />
                {b.logical.schedule && (
                  <span className="sm muted">{cronToText(b.logical.schedule)}</span>
                )}
              </div>
              <div className="form-row">
                <label>Databases (comma-separated, empty = all)</label>
                <input className="input" value={(b.logical.databases || []).join(', ')}
                  onChange={e => setLogical(l => ({ ...l, databases: e.target.value ? e.target.value.split(',').map(s => s.trim()).filter(Boolean) : [] }))}
                  placeholder="Leave empty for all databases" />
              </div>
              <div className="form-row">
                <label>Format</label>
                <select className="input" value={b.logical.format}
                  onChange={e => setLogical(l => ({ ...l, format: e.target.value }))}>
                  <option value="custom">Custom (pg_restore compatible)</option>
                  <option value="plain">Plain SQL</option>
                  <option value="directory">Directory</option>
                </select>
              </div>
            </div>
          )}

          {/* Retention */}
          {(b.physical.enabled || b.logical.enabled) && (
            <>
              <h4 style={{ fontSize: 13, marginBottom: 8 }}>Retention</h4>
              <div className="form-grid" style={{ marginBottom: 10 }}>
                {b.physical.enabled && (
                  <>
                    <div className="form-row">
                      <label>Base backups to keep</label>
                      <input className="input" type="number" min={1} value={b.retention.base_backup_count}
                        onChange={e => setRetention(r => ({ ...r, base_backup_count: parseInt(e.target.value) || 1 }))}
                        style={{ width: 80 }} />
                    </div>
                    <div className="form-row">
                      <label>Incremental backups to keep</label>
                      <input className="input" type="number" min={0} value={b.retention.incremental_backup_count}
                        onChange={e => setRetention(r => ({ ...r, incremental_backup_count: parseInt(e.target.value) || 0 }))}
                        style={{ width: 80 }} />
                    </div>
                    <div className="form-row">
                      <label>WAL retention (days)</label>
                      <input className="input" type="number" min={1} value={b.retention.wal_retention_days}
                        onChange={e => setRetention(r => ({ ...r, wal_retention_days: parseInt(e.target.value) || 1 }))}
                        style={{ width: 80 }} />
                    </div>
                  </>
                )}
                {b.logical.enabled && (
                  <div className="form-row">
                    <label>Logical backups to keep</label>
                    <input className="input" type="number" min={1} value={b.retention.logical_backup_count}
                      onChange={e => setRetention(r => ({ ...r, logical_backup_count: parseInt(e.target.value) || 1 }))}
                      style={{ width: 80 }} />
                  </div>
                )}
              </div>
            </>
          )}
        </>
      )}
    </section>
  );
}

// ── Confirmation Report ─────────────────────────────────────────────────────

function ConfirmReport({ state, spec, changedParams, onConfirm, onCancel, saving, saveError }) {
  const isEdit = !state.isNew && state.originalSpec;
  const specChanges = isEdit ? computeSpecChanges(state.originalSpec, spec) : [];

  const content = (
    <>
      <div className="confirm-header">
        <h3>{isEdit ? 'Review Changes' : 'Configuration Report'}</h3>
        <p className="muted sm">Review before saving profile <strong>{state.name || '(unnamed)'}</strong></p>
      </div>

        <div className="confirm-body">

          {/* Immutable field errors from server */}
          {saveError && (
            <div className="report-section" style={{ background: 'var(--red-bg)', borderRadius: 8, padding: 12, marginBottom: 12 }}>
              <h5 style={{ color: 'var(--red)', margin: 0 }}>Blocked - Immutable Fields</h5>
              <p style={{ color: 'var(--red)', fontSize: 12.5, margin: '6px 0 8px' }}>{saveError.error}</p>
              <table className="node-table" style={{ fontSize: 12 }}>
                <thead><tr><th>Field</th><th>Current</th><th></th><th>Requested</th></tr></thead>
                <tbody>
                  {saveError.immutableErrors.map((ch, i) => (
                    <tr key={i}>
                      <td className="mono" style={{ color: 'var(--red)' }}>{ch.path}</td>
                      <td className="mono">{ch.old_value}</td>
                      <td style={{ textAlign: 'center', color: 'var(--text-secondary)' }}>&rarr;</td>
                      <td className="mono" style={{ fontWeight: 600 }}>{ch.new_value}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
              <p className="muted" style={{ fontSize: 11.5, marginTop: 8 }}>These fields cannot be changed while clusters are using this profile. Go back and revert these fields.</p>
            </div>
          )}

          {/* Changes diff (edit mode) */}
          {isEdit && specChanges.length > 0 && !saveError && (
            <div className="report-section" style={{ marginBottom: 12 }}>
              <h5>What's Changing <span className="tab-badge">{specChanges.length}</span></h5>
              <table className="node-table" style={{ fontSize: 12 }}>
                <thead><tr><th>Field</th><th>Before</th><th></th><th>After</th></tr></thead>
                <tbody>
                  {specChanges.map((ch, i) => (
                    <tr key={i}>
                      <td className="mono">{ch.path}</td>
                      <td className="mono">
                        <span style={{ background: 'var(--red-bg)', color: 'var(--red)', fontWeight: 600, padding: '1px 6px', borderRadius: 4, display: 'inline-block' }}>
                          {ch.old_value || '(unset)'}
                        </span>
                      </td>
                      <td style={{ textAlign: 'center', color: 'var(--text-secondary)' }}>&rarr;</td>
                      <td className="mono">
                        <span style={{ background: 'var(--green-light)', color: 'var(--green-dark)', fontWeight: 600, padding: '1px 6px', borderRadius: 4, display: 'inline-block' }}>
                          {ch.new_value || '(unset)'}
                        </span>
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}

          {isEdit && specChanges.length === 0 && !saveError && (
            <div className="report-section">
              <p className="muted">No configuration changes detected.</p>
            </div>
          )}

          {/* Full config summary (new profiles or as collapsed detail) */}
          {(!isEdit || specChanges.length === 0) && !saveError && (
            <>
              {/* Cluster */}
              <div className="report-section">
                <h5>Cluster</h5>
                <div className="report-grid">
                  <ReportRow label="Replicas" value={spec.replicas} />
                  <ReportRow label="PostgreSQL" value={`${spec.postgres.version} ${spec.postgres.variant || 'alpine'}${spec.postgres.registry ? ` (registry: ${spec.postgres.registry})` : ''}`} />
                  <ReportRow label="Failover" value={spec.failover?.enabled ? 'Enabled' : 'Disabled'} />
                </div>
              </div>

              {/* Volumes */}
              <div className="report-section">
                <h5>Volumes</h5>
                <div className="report-grid">
                  <ReportRow label="Data" value={`${spec.storage.size}${spec.storage.storage_class ? ` (${spec.storage.storage_class})` : ''}`} />
                  {spec.wal_storage && (
                    <ReportRow label="WAL" value={`${spec.wal_storage.size}${spec.wal_storage.storage_class ? ` (${spec.wal_storage.storage_class})` : ''}`} />
                  )}
                </div>
              </div>

              {/* Resources */}
              <div className="report-section">
                <h5>Resources</h5>
                <div className="report-grid">
                  <ReportRow label="CPU" value={`${spec.resources.cpu_request} / ${spec.resources.cpu_limit}`} />
                  <ReportRow label="Memory" value={`${spec.resources.memory_request} / ${spec.resources.memory_limit}`} />
                </div>
              </div>

              {/* PG Params — only non-default */}
              {changedParams.length > 0 && (
                <div className="report-section">
                  <h5>Modified PostgreSQL Parameters <span className="tab-badge">{changedParams.length}</span></h5>
                  <div className="report-params">
                    {changedParams.map(([key, value]) => (
                      <div className="report-param" key={key}>
                        <span className="mono">{key}</span>
                        <span className="mono report-param-val">{value}</span>
                        {DEFAULT_PG_PARAMS[key] !== undefined && (
                          <span className="muted sm">default: {DEFAULT_PG_PARAMS[key]}</span>
                        )}
                      </div>
                    ))}
                  </div>
                </div>
              )}

              {/* HBA */}
              <div className="report-section">
                <h5>HBA Rules</h5>
                <div className="report-hba">
                  {spec.hba_rules.map((rule, i) => (
                    <div className="mono sm" key={i}>{rule}</div>
                  ))}
                </div>
              </div>
            </>
          )}

        </div>

        <div className="confirm-footer">
          {!saveError && (
            <button className="btn btn-approve" onClick={onConfirm} disabled={saving}>{saving ? 'Saving...' : 'Confirm & Save'}</button>
          )}
          <button className="btn btn-reject" onClick={onCancel} disabled={saving}>Back to Editing</button>
        </div>
    </>
  );

  return (
    <div className="confirm-overlay">
      <div className="confirm-modal">
        {content}
      </div>
    </div>
  );
}

function ReportRow({ label, value }) {
  return (
    <div className="report-row">
      <span className="report-label">{label}</span>
      <span className="report-value">{value}</span>
    </div>
  );
}

function StorageClassSelect({ value, storageTiers = [], onChange }) {
  const isTier = value && value.startsWith('tier:');
  const hasValue = !value || isTier || storageTiers.some(t => 'tier:' + t.name === value);
  return (
    <select className="input" value={value} onChange={e => onChange(e.target.value)}>
      <option value="">Default</option>
      {!hasValue && value && <option value={value}>{value}</option>}
      {storageTiers.map(t => (
        <option key={t.name} value={'tier:' + t.name}>{t.name}{t.description ? ' — ' + t.description : ''}</option>
      ))}
    </select>
  );
}
