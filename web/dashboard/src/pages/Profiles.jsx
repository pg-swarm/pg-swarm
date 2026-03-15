import { useState, useMemo, useEffect } from 'react';
import { useData } from '../context/DataContext';
import { useToast } from '../context/ToastContext';
import { api, parseSpec, timeAgo } from '../api';

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
    databases: [],
    failover: { enabled: true, health_check_interval_seconds: 5 },
  };
}

export default function Profiles() {
  const { profiles, postgresVersions, postgresVariants, satellites, clusters, refresh } = useData();
  const toast = useToast();

  useEffect(() => { document.title = 'Profiles - pg-swarm'; }, []);
  const [editing, setEditing] = useState(null);
  const [viewing, setViewing] = useState(null);
  const [cloneName, setCloneName] = useState('');
  const [cloneTarget, setCloneTarget] = useState(null);
  const [scRefreshing, setScRefreshing] = useState(false);

  // Deduplicate storage classes across all satellites
  const storageClasses = useMemo(() => {
    const seen = new Set();
    const result = [];
    for (const sat of satellites) {
      for (const sc of (sat.storage_classes || [])) {
        if (!seen.has(sc.name)) {
          seen.add(sc.name);
          result.push(sc);
        }
      }
    }
    return result.sort((a, b) => a.name.localeCompare(b.name));
  }, [satellites]);

  async function refreshAllStorageClasses() {
    setScRefreshing(true);
    try {
      await Promise.all(satellites.map(s => api.refreshStorageClasses(s.id).catch(() => {})));
      // Wait briefly for reports to arrive, then refresh
      setTimeout(() => { refresh(); setScRefreshing(false); }, 2000);
    } catch {
      setScRefreshing(false);
    }
  }

  function startCreate() {
    setEditing({ name: '', description: '', spec: emptySpec(), isNew: true });
  }

  function startEdit(profile) {
    const spec = parseSpec(profile.config);
    const pg_params = { ...DEFAULT_PG_PARAMS, ...spec.pg_params };
    const hba_rules = spec.hba_rules?.length ? spec.hba_rules : [...DEFAULT_HBA_RULES];
    setEditing({
      id: profile.id,
      name: profile.name,
      description: profile.description,
      spec: { ...emptySpec(), ...spec, pg_params, hba_rules },
      isNew: false,
    });
  }

  function startView(profile) {
    const spec = parseSpec(profile.config);
    const pg_params = { ...DEFAULT_PG_PARAMS, ...spec.pg_params };
    const hba_rules = spec.hba_rules?.length ? spec.hba_rules : [...DEFAULT_HBA_RULES];
    setViewing({
      name: profile.name,
      description: profile.description,
      spec: { ...emptySpec(), ...spec, pg_params, hba_rules },
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
    };
    try {
      if (editing.isNew) {
        await api.createProfile(payload);
        toast('Profile created');
      } else {
        await api.updateProfile(editing.id, payload);
        toast('Profile updated');
      }
      setEditing(null);
      refresh();
    } catch (e) {
      toast('Save failed: ' + e.message, true);
    }
  }

  async function remove(id) {
    try {
      await api.deleteProfile(id);
      toast('Profile deleted');
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
    return <ProfileForm state={editing} setState={setEditing} onSave={save} onCancel={() => setEditing(null)} postgresVersions={postgresVersions} postgresVariants={postgresVariants} storageClasses={storageClasses} scRefreshing={scRefreshing} onRefreshStorageClasses={refreshAllStorageClasses} />;
  }

  if (viewing) {
    return <ProfileView state={viewing} onClose={() => setViewing(null)} />;
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
          return (
            <div className="cl-card" key={p.id}>
              <div className="cl-head">
                <h3>{p.name}</h3>
                <div className="badges">
                  {p.locked
                    ? <span className="badge badge-amber"><span className="dot" />Locked</span>
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
                  <KV label="Databases" value={spec.databases?.length || 0} />
                </dl>
                <div className="cl-tags">
                  {spec.failover?.enabled && <span className="tag">failover</span>}
                  {spec.archive?.mode && <span className="tag">archive:{spec.archive.mode}</span>}
                  {Object.keys(spec.pg_params || {}).length > 0 && <span className="tag">{Object.keys(spec.pg_params).length} pg params</span>}
                  {(spec.hba_rules || []).length > 0 && <span className="tag">{spec.hba_rules.length} hba rules</span>}
                </div>
              </div>
              <div className="cl-foot">
                <span>{timeAgo(p.created_at)}</span>
                <span className="actions" style={{ marginLeft: 'auto' }}>
                  {p.locked
                    ? <button className="btn btn-sm" onClick={() => startView(p)}>View</button>
                    : <button className="btn btn-sm" onClick={() => startEdit(p)}>Edit</button>}
                  <button className="btn btn-sm" onClick={() => { setCloneTarget(p.id); setCloneName(p.name + '-copy'); }}>Clone</button>
                  {!p.locked && <button className="btn btn-sm btn-reject" onClick={() => remove(p.id)}>Delete</button>}
                </span>
              </div>
            </div>
          );
        })}
      </div>
    </>
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
  { id: 'databases', label: 'Databases' },
];

// ── Profile Form ────────────────────────────────────────────────────────────

function ProfileForm({ state, setState, onSave, onCancel, postgresVersions, postgresVariants, storageClasses, scRefreshing, onRefreshStorageClasses }) {
  const spec = state.spec;
  const [activeTab, setActiveTab] = useState('general');
  const [showConfirm, setShowConfirm] = useState(false);
  const [pgSearch, setPgSearch] = useState('');
  const [collapsedCategories, setCollapsedCategories] = useState({});

  // Build version options from postgres_versions table
  const versionOptions = useMemo(() => {
    const opts = [];
    const seen = new Set();
    for (const pv of (postgresVersions || [])) {
      const key = `${pv.version} ${pv.variant.charAt(0).toUpperCase() + pv.variant.slice(1)}`;
      if (!seen.has(key)) {
        seen.add(key);
        opts.push({ version: pv.version, variant: pv.variant, label: key, image_tag: pv.image_tag });
      }
    }
    return opts;
  }, [postgresVersions]);

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

  // Database helpers
  function addDatabase() {
    setSpec(s => ({ ...s, databases: [...(s.databases || []), { name: '', user: '', password: '' }] }));
  }

  function setDatabase(idx, field, value) {
    setSpec(s => {
      const dbs = [...s.databases];
      dbs[idx] = { ...dbs[idx], [field]: value };
      return { ...s, databases: dbs };
    });
  }

  function removeDatabase(idx) {
    setSpec(s => ({ ...s, databases: s.databases.filter((_, i) => i !== idx) }));
  }

  function handleSave() {
    setShowConfirm(true);
  }

  function confirmSave() {
    setShowConfirm(false);
    onSave();
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
            {tab.id === 'databases' && (spec.databases || []).length > 0 && (
              <span className="tab-badge">{spec.databases.length}</span>
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
            </div>
          </section>
        )}

        {activeTab === 'volumes' && (
          <section className="form-section">
            <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between' }}>
              <h4>Volumes</h4>
              <button className="btn-sm" onClick={onRefreshStorageClasses} disabled={scRefreshing}>
                {scRefreshing ? 'Refreshing...' : 'Refresh Storage Classes'}
              </button>
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
                  <label>Storage Class</label>
                  <StorageClassSelect value={spec.storage.storage_class} storageClasses={storageClasses} onChange={v => setSpec(s => ({ ...s, storage: { ...s.storage, storage_class: v } }))} />
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
                    <label>Storage Class</label>
                    <StorageClassSelect value={spec.wal_storage.storage_class} storageClasses={storageClasses} onChange={v => setSpec(s => ({ ...s, wal_storage: { ...s.wal_storage, storage_class: v } }))} />
                  </div>
                </div>
              )}
            </div>
            {spec.archive?.mode === 'pvc' && (
              <div className="volume-section">
                <h5>WAL Archive Volume</h5>
                <div className="form-grid">
                  <div className="form-row">
                    <label>Size</label>
                    <input className="input" value={spec.archive?.archive_storage?.size || ''} onChange={e => setSpec(s => ({ ...s, archive: { ...s.archive, archive_storage: { ...(s.archive?.archive_storage || {}), size: e.target.value } } }))} />
                  </div>
                  <div className="form-row">
                    <label>Storage Class</label>
                    <StorageClassSelect value={spec.archive?.archive_storage?.storage_class || ''} storageClasses={storageClasses} onChange={v => setSpec(s => ({ ...s, archive: { ...s.archive, archive_storage: { ...(s.archive?.archive_storage || {}), storage_class: v } } }))} />
                  </div>
                </div>
              </div>
            )}
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

        {activeTab === 'databases' && (
          <section className="form-section">
            <h4>Databases & Users</h4>
            {spec.replicas > 1 && (
              <div className="repl-user-info">
                <table className="db-table">
                  <thead>
                    <tr><th>Database</th><th>User</th><th>Password</th><th></th></tr>
                  </thead>
                  <tbody>
                    <tr className="repl-user-row">
                      <td><span className="mono muted">replication</span></td>
                      <td><span className="mono muted">repl_user</span></td>
                      <td><span className="muted">(auto-generated)</span></td>
                      <td><span className="badge badge-green" style={{ fontSize: 10 }}>auto</span></td>
                    </tr>
                  </tbody>
                </table>
                <p className="muted sm" style={{ marginTop: 4, marginBottom: 8 }}>Replication user is automatically created when replicas &gt; 1</p>
              </div>
            )}
            {(spec.databases || []).length === 0 && spec.replicas <= 1 ? (
              <p className="muted sm">No databases configured. The default postgres database will be available.</p>
            ) : (spec.databases || []).length > 0 ? (
              <table className="db-table">
                <thead>
                  <tr><th>Database</th><th>User</th><th>Password</th><th></th></tr>
                </thead>
                <tbody>
                  {spec.databases.map((db, i) => (
                    <tr key={i}>
                      <td><input className="input" value={db.name} onChange={e => setDatabase(i, 'name', e.target.value)} placeholder="dbname" /></td>
                      <td><input className="input" value={db.user} onChange={e => setDatabase(i, 'user', e.target.value)} placeholder="username" /></td>
                      <td><input className="input" type="password" value={db.password} onChange={e => setDatabase(i, 'password', e.target.value)} placeholder="password" /></td>
                      <td><button className="btn-icon" onClick={() => removeDatabase(i)}>&times;</button></td>
                    </tr>
                  ))}
                </tbody>
              </table>
            ) : null}
            <button className="btn btn-sm" onClick={addDatabase} style={{ marginTop: 6 }}>+ Add Database</button>
          </section>
        )}
      </div>

      {/* Save confirmation modal */}
      {showConfirm && (
        <ConfirmReport
          state={state}
          spec={spec}
          changedParams={changedParams}
          onConfirm={confirmSave}
          onCancel={() => setShowConfirm(false)}
        />
      )}
    </div>
  );
}

// ── Profile View (read-only for locked profiles) ────────────────────────────

function ProfileView({ state, onClose }) {
  const spec = state.spec;
  const changedParams = Object.entries(spec.pg_params || {}).filter(([k, v]) => DEFAULT_PG_PARAMS[k] !== v);

  return (
    <div className="profile-form">
      <div className="card-head-bar">
        <span className="card-head-title">Profile: {state.name}</span>
        <div className="actions">
          <button className="btn" onClick={onClose}>Close</button>
        </div>
      </div>
      <ConfirmReport
        state={state}
        spec={spec}
        changedParams={changedParams}
        onConfirm={onClose}
        onCancel={onClose}
        readOnly
      />
    </div>
  );
}

// ── Confirmation Report ─────────────────────────────────────────────────────

function ConfirmReport({ state, spec, changedParams, onConfirm, onCancel, readOnly }) {
  const content = (
    <>
      <div className="confirm-header">
        <h3>{readOnly ? 'Profile Configuration' : 'Configuration Report'}</h3>
        {!readOnly && <p className="muted sm">Review before saving profile <strong>{state.name || '(unnamed)'}</strong></p>}
        {readOnly && state.description && <p className="muted sm">{state.description}</p>}
      </div>

        <div className="confirm-body">
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
              {spec.archive?.mode === 'pvc' && spec.archive?.archive_storage?.size && (
                <ReportRow label="WAL Archive" value={`${spec.archive.archive_storage.size}${spec.archive.archive_storage.storage_class ? ` (${spec.archive.archive_storage.storage_class})` : ''}`} />
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

          {/* Databases */}
          {(spec.databases || []).length > 0 && (
            <div className="report-section">
              <h5>Databases</h5>
              <div className="report-grid">
                {spec.databases.map((db, i) => (
                  <ReportRow key={i} label={db.name} value={`owner: ${db.user}`} />
                ))}
              </div>
            </div>
          )}
        </div>

        {!readOnly && (
          <div className="confirm-footer">
            <button className="btn btn-approve" onClick={onConfirm}>Confirm & Save</button>
            <button className="btn btn-reject" onClick={onCancel}>Back to Editing</button>
          </div>
        )}
    </>
  );

  if (readOnly) {
    return <div className="confirm-body-standalone">{content}</div>;
  }

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

function StorageClassSelect({ value, storageClasses, onChange }) {
  // If the current value isn't in the list (e.g. typed manually before), keep it as an option
  const hasValue = !value || storageClasses.some(sc => sc.name === value);
  return (
    <select className="input" value={value} onChange={e => onChange(e.target.value)}>
      <option value="">Default</option>
      {!hasValue && <option value={value}>{value}</option>}
      {storageClasses.map(sc => (
        <option key={sc.name} value={sc.name}>
          {sc.name}{sc.is_default ? ' (default)' : ''} — {sc.provisioner}
        </option>
      ))}
    </select>
  );
}
