import { useState, useEffect } from 'react';
import { useData } from '../context/DataContext';
import { useToast } from '../context/ToastContext';
import { api, parseSpec, timeAgo } from '../api';
import { Info, X } from 'lucide-react';

const TABS = [
  { id: 'general', label: 'General' },
  { id: 'schedule', label: 'Schedule' },
  { id: 'destination', label: 'Destination' },
  { id: 'retention', label: 'Retention' },
];

function emptySpec() {
  return {
    physical: { base_schedule: '0 0 2 * * 0', wal_archive_enabled: true, archive_timeout_seconds: 60 },
    logical: null,
    destination: { type: 's3', s3: { bucket: '', region: '', endpoint: '', path_prefix: '' } },
    retention: { base_backup_count: 7, wal_retention_days: 14, logical_backup_count: 30 },
  };
}

function KV({ label, value }) {
  return <div><dt>{label}</dt><dd>{String(value)}</dd></div>;
}

// Describe a cron expression in plain English.
// Supports both 5-field (min hour dom mon dow) and 6-field (sec min hour dom mon dow).
function describeCron(cron) {
  if (!cron) return null;
  const parts = cron.trim().split(/\s+/);
  if (parts.length < 5) return null;
  // 6-field: second minute hour dom month dow
  const [minute, hour, dayOfMonth, month, dayOfWeek] = parts.length >= 6 ? parts.slice(1) : parts;

  const dayNames = ['Sunday', 'Monday', 'Tuesday', 'Wednesday', 'Thursday', 'Friday', 'Saturday'];
  const monthNames = ['', 'January', 'February', 'March', 'April', 'May', 'June', 'July', 'August', 'September', 'October', 'November', 'December'];

  // Expand ranges like "1-5" to [1,2,3,4,5] and handle commas
  function expandField(field, names) {
    const result = [];
    for (const part of field.split(',')) {
      if (part.includes('-')) {
        const [start, end] = part.split('-').map(Number);
        for (let i = start; i <= end; i++) result.push(names ? (names[i] || String(i)) : String(i));
      } else {
        const n = parseInt(part);
        result.push(names ? (names[n] || part) : part);
      }
    }
    return result;
  }

  let timeStr = '';
  if (hour !== '*' && minute !== '*') {
    const hours = expandField(hour);
    const minutes = expandField(minute);
    const times = [];
    for (const h of hours) {
      for (const m of minutes) {
        const n = parseInt(h);
        const ampm = n >= 12 ? 'PM' : 'AM';
        const h12 = n === 0 ? 12 : n > 12 ? n - 12 : n;
        times.push(`${h12}:${String(parseInt(m)).padStart(2, '0')} ${ampm}`);
      }
    }
    timeStr = `at ${times.join(', ')}`;
  } else if (hour !== '*' && minute === '*') {
    const hours = expandField(hour);
    const names = hours.map(h => { const n = parseInt(h); return n >= 12 ? `${n === 12 ? 12 : n - 12} PM` : `${n === 0 ? 12 : n} AM`; });
    timeStr = `every minute during ${names.join(', ')}`;
  } else if (hour === '*' && minute !== '*') {
    const minutes = expandField(minute);
    timeStr = `at minute ${minutes.join(', ')} of every hour`;
  }

  // Every N minutes/hours
  if (minute.startsWith('*/')) return `Every ${minute.slice(2)} minutes`;
  if (hour.startsWith('*/')) return `Every ${hour.slice(2)} hours`;

  // Daily: * * *
  if (dayOfMonth === '*' && month === '*' && dayOfWeek === '*') {
    return `Every day ${timeStr}`.trim();
  }
  // Weekly: * * 0-6 (handles commas and ranges like 1-5 or 0,6)
  if (dayOfMonth === '*' && month === '*' && dayOfWeek !== '*') {
    const days = expandField(dayOfWeek, dayNames);
    return `Every ${days.join(', ')} ${timeStr}`.trim();
  }
  // Monthly: N * *
  if (dayOfMonth !== '*' && month === '*' && dayOfWeek === '*') {
    if (dayOfMonth.startsWith('*/')) return `Every ${dayOfMonth.slice(2)} days ${timeStr}`.trim();
    const suffix = n => { if (n >= 11 && n <= 13) return n + 'th'; return n + ['th','st','nd','rd'][n % 10 > 3 ? 0 : n % 10]; };
    const dates = expandField(dayOfMonth).map(d => suffix(parseInt(d)));
    return `On the ${dates.join(', ')} of every month ${timeStr}`.trim();
  }
  // Yearly: N N *
  if (dayOfMonth !== '*' && month !== '*' && dayOfWeek === '*') {
    const months = expandField(month, monthNames);
    return `On ${months.join(', ')} ${dayOfMonth} ${timeStr}`.trim();
  }
  return null;
}

// Estimate the interval in days between runs of a cron schedule.
// Handles common patterns; returns null if unrecognizable.
function estimateCronIntervalDays(cron) {
  if (!cron) return null;
  const parts = cron.trim().split(/\s+/);
  if (parts.length < 5) return null;
  const fields = parts.length >= 6 ? parts.slice(1) : parts;
  const [, , dayOfMonth, month, dayOfWeek] = fields;

  // "0 2 * * 0" = weekly
  if (dayOfMonth === '*' && month === '*' && dayOfWeek !== '*') {
    const days = dayOfWeek.split(',');
    if (days.length === 1) return 7;
    return Math.floor(7 / days.length);
  }
  // "0 2 * * *" = daily
  if (dayOfMonth === '*' && month === '*' && dayOfWeek === '*') return 1;
  // "0 2 1 * *" = monthly
  if (dayOfMonth !== '*' && month === '*' && dayOfWeek === '*') {
    const dates = dayOfMonth.split(',');
    return Math.floor(30 / dates.length);
  }
  // "0 2 */N * *" = every N days
  if (dayOfMonth.startsWith('*/')) {
    const n = parseInt(dayOfMonth.slice(2));
    if (n > 0) return n;
  }
  return null;
}

// Calculate minimum WAL retention days needed to cover all base backups.
function minWalRetentionDays(schedule, baseCount) {
  const interval = estimateCronIntervalDays(schedule);
  if (!interval || !baseCount) return null;
  return interval * baseCount;
}

export default function BackupProfiles() {
  const { backupProfiles, refresh } = useData();
  const toast = useToast();

  useEffect(() => { document.title = 'Backup Rules - PG-Swarm'; }, []);
  const [editing, setEditing] = useState(null);

  function startCreate() {
    setEditing({ name: '', description: '', spec: emptySpec(), isNew: true });
  }

  function startEdit(rule) {
    setEditing({
      id: rule.id,
      name: rule.name,
      description: rule.description,
      spec: { ...emptySpec(), ...parseSpec(rule.config) },
      isNew: false,
    });
  }

  async function save() {
    const payload = { name: editing.name, description: editing.description, config: editing.spec };
    try {
      if (editing.isNew) {
        await api.createBackupProfile(payload);
        toast('Backup profile created');
      } else {
        await api.updateBackupProfile(editing.id, payload);
        toast('Backup profile updated');
      }
      setEditing(null);
      refresh();
    } catch (e) {
      toast('Save failed: ' + e.message, true);
      throw e;
    }
  }

  async function remove(id) {
    try {
      await api.deleteBackupProfile(id);
      toast('Backup profile deleted');
      refresh();
    } catch (e) {
      toast('Delete failed: ' + e.message, true);
    }
  }

  return (
    <>
      <div className="card-head-bar">
        <span className="card-head-title">Backup Rules</span>
        <button className="btn btn-approve" onClick={startCreate}>+ New Rule</button>
      </div>

      <div className="profile-grid">
        {backupProfiles.length === 0 ? (
          <div className="empty">No backup rules created yet</div>
        ) : backupProfiles.map(rule => {
          const spec = parseSpec(rule.config);
          const backupType = (spec.physical && spec.logical) ? 'Physical + Logical' : spec.physical ? 'Physical' : spec.logical ? 'Logical' : '-';
          const schedule = spec.physical?.base_schedule || spec.logical?.schedule || '-';
          return (
            <div className="cl-card" key={rule.id}>
              <div className="cl-head">
                <h3>{rule.name}</h3>
                <div className="badges">
                  <span className="badge badge-green"><span className="dot" />{spec.destination?.type?.toUpperCase() || '?'}</span>
                  <span className="badge badge-gray">{backupType}</span>
                </div>
              </div>
              <div className="cl-body">
                {rule.description && <p className="sm muted" style={{ marginBottom: 8 }}>{rule.description}</p>}
                <dl className="cl-grid">
                  <KV label="Type" value={backupType} />
                  <KV label="Schedule" value={schedule} />
                  <KV label="WAL Archive" value={spec.physical?.wal_archive_enabled ? 'Enabled' : 'Disabled'} />
                  <KV label="Base Retention" value={spec.retention?.base_backup_count || 7} />
                  <KV label="WAL Days" value={spec.retention?.wal_retention_days || 14} />
                  <KV label="Logical Retention" value={spec.retention?.logical_backup_count || 30} />
                </dl>
                <div className="cl-tags">
                  <span className="tag">{spec.destination?.type}</span>
                  {spec.physical?.wal_archive_enabled && <span className="tag">wal-archive</span>}
                  {spec.logical?.databases?.length > 0 && <span className="tag">{spec.logical.databases.length} dbs</span>}
                </div>
              </div>
              <div className="cl-foot">
                <span>{timeAgo(rule.created_at)}</span>
                <span className="actions" style={{ marginLeft: 'auto' }}>
                  <button className="btn btn-sm" onClick={() => startEdit(rule)}>Edit</button>
                  <button className="btn btn-sm btn-reject" onClick={() => remove(rule.id)}>Delete</button>
                </span>
              </div>
            </div>
          );
        })}
      </div>

      {editing && (
        <BackupProfileForm state={editing} setState={setEditing} onSave={save} onCancel={() => setEditing(null)} />
      )}
    </>
  );
}

// ── Form ────────────────────────────────────────────────────────────────────

function BackupProfileForm({ state, setState, onSave, onCancel }) {
  const spec = state.spec;
  const [activeTab, setActiveTab] = useState('general');
  const [showConfirm, setShowConfirm] = useState(false);

  function setField(path, value) {
    setState(prev => ({ ...prev, [path]: value }));
  }

  function setSpec(fn) {
    setState(prev => ({ ...prev, spec: fn(prev.spec) }));
  }

  function togglePhysical() {
    setSpec(s => ({
      ...s,
      physical: s.physical ? null : { base_schedule: '0 0 2 * * 0', wal_archive_enabled: true, archive_timeout_seconds: 60 },
    }));
  }

  function toggleLogical() {
    setSpec(s => ({
      ...s,
      logical: s.logical ? null : { schedule: '0 0 2 * * *', databases: [], format: 'custom' },
    }));
  }

  function setDestType(type) {
    const dest = { type };
    if (type === 's3') dest.s3 = { bucket: '', region: '', endpoint: '', path_prefix: '' };
    if (type === 'gcs') dest.gcs = { bucket: '', path_prefix: '', service_account_json: '' };
    if (type === 'sftp') dest.sftp = { host: '', port: 22, user: '', password: '', base_path: '' };
    if (type === 'local') dest.local = { size: '10Gi', storage_class: '' };
    setSpec(s => ({ ...s, destination: dest }));
  }

  const [saving, setSaving] = useState(false);

  function handleSave() {
    setShowConfirm(true);
  }

  async function confirmSave() {
    setSaving(true);
    try {
      await onSave();
      setShowConfirm(false);
    } finally {
      setSaving(false);
    }
  }

  return (
    <div className="confirm-overlay" onClick={onCancel}>
      <div className="confirm-modal backup-profile-modal" onClick={e => e.stopPropagation()}>
        <div className="confirm-header">
          <h3>{state.isNew ? 'Create Backup Rule' : 'Edit Backup Rule'}</h3>
          <button className="modal-close" onClick={onCancel}><X size={18} /></button>
        </div>

        <div className="confirm-body">
          <div className="tab-bar">
            {TABS.map(tab => (
              <button
                key={tab.id}
                className={'tab-item' + (activeTab === tab.id ? ' active' : '')}
                onClick={() => setActiveTab(tab.id)}
              >
                {tab.label}
              </button>
            ))}
          </div>

          <div className="tab-content">
            {activeTab === 'general' && (
          <section className="form-section">
            <h4>Backup Rule</h4>
            <div className="form-grid">
              <div className="form-row">
                <label>Name</label>
                <input className="input" value={state.name} onChange={e => setField('name', e.target.value)} placeholder="e.g. s3-weekly-base" />
              </div>
              <div className="form-row">
                <label>Description</label>
                <input className="input" value={state.description} onChange={e => setField('description', e.target.value)} placeholder="Optional description" />
              </div>
              <div className="form-row">
                <label>Backup Types</label>
                <div className="radio-group">
                  <label className="toggle">
                    <input type="checkbox" checked={!!spec.physical} onChange={togglePhysical} />
                    <span>Enable Physical Backup</span>
                    <span className="info-tip" title="Full binary copy of the data directory. Supports continuous WAL archiving and point-in-time recovery (PITR). Best for disaster recovery."><Info size={14} /></span>
                  </label>
                  <label className="toggle">
                    <input type="checkbox" checked={!!spec.logical} onChange={toggleLogical} />
                    <span>Enable Logical Backup</span>
                    <span className="info-tip" title="SQL-level dump of database objects and data. Portable across PG versions. Best for migrations, selective restores, and cross-version upgrades."><Info size={14} /></span>
                  </label>
                </div>
              </div>
            </div>
          </section>
        )}

        {activeTab === 'schedule' && (
          <section className="form-section">
            {spec.physical && (
              <>
                <h4>Physical Backup Schedule</h4>
                <div className="form-grid">
                  <div className="form-row">
                    <label>Base Backup Schedule (cron)</label>
                    <input className="input" value={spec.physical.base_schedule} onChange={e => setSpec(s => ({ ...s, physical: { ...s.physical, base_schedule: e.target.value } }))} placeholder="0 0 2 * * 0" />
                    {describeCron(spec.physical.base_schedule)
                      ? <span className="form-hint cron-desc">{describeCron(spec.physical.base_schedule)}</span>
                      : <span className="form-hint">Cron: second minute hour day month weekday</span>}
                  </div>
                  <div className="form-row">
                    <label>Incremental Backup Schedule (PG 17+)</label>
                    <input className="input" value={spec.physical.incremental_schedule || ''} onChange={e => setSpec(s => ({ ...s, physical: { ...s.physical, incremental_schedule: e.target.value } }))} placeholder="Optional, e.g. 0 0 2 * * 1-6" />
                    {spec.physical.incremental_schedule && describeCron(spec.physical.incremental_schedule)
                      ? <span className="form-hint cron-desc">{describeCron(spec.physical.incremental_schedule)}</span>
                      : <span className="form-hint">Requires PostgreSQL 17+. Takes smaller backups of only changed blocks since last full.</span>}
                  </div>
                  <div className="form-row">
                    <label>WAL Archiving</label>
                    <label className="toggle">
                      <input type="checkbox" checked={spec.physical.wal_archive_enabled} onChange={e => setSpec(s => ({ ...s, physical: { ...s.physical, wal_archive_enabled: e.target.checked } }))} />
                      <span>Enable continuous WAL archiving</span>
                    </label>
                    <span className="form-hint">Required for point-in-time recovery between backups</span>
                  </div>
                  {spec.physical.wal_archive_enabled && (
                    <div className="form-row">
                      <label>Archive Timeout (seconds)</label>
                      <input className="input" type="number" value={spec.physical.archive_timeout_seconds || 60} onChange={e => setSpec(s => ({ ...s, physical: { ...s.physical, archive_timeout_seconds: parseInt(e.target.value) || 60 } }))} />
                      <span className="form-hint">Force a WAL switch after this many idle seconds</span>
                    </div>
                  )}
                </div>
              </>
            )}

            {spec.logical && (
              <>
                <h4>Logical Backup Schedule</h4>
                <div className="form-grid">
                  <div className="form-row">
                    <label>Schedule (cron)</label>
                    <input className="input" value={spec.logical.schedule} onChange={e => setSpec(s => ({ ...s, logical: { ...s.logical, schedule: e.target.value } }))} placeholder="0 0 2 * * *" />
                    {describeCron(spec.logical.schedule)
                      ? <span className="form-hint cron-desc">{describeCron(spec.logical.schedule)}</span>
                      : <span className="form-hint">Cron: second minute hour day month weekday</span>}
                  </div>
                  <div className="form-row">
                    <label>Dump Format</label>
                    <select className="input" value={spec.logical.format || 'custom'} onChange={e => setSpec(s => ({ ...s, logical: { ...s.logical, format: e.target.value } }))}>
                      <option value="custom">Custom (pg_restore compatible)</option>
                      <option value="plain">Plain SQL</option>
                      <option value="directory">Directory</option>
                    </select>
                  </div>
                  <div className="form-row">
                    <label>Databases (empty = pg_dumpall)</label>
                    <input className="input" value={(spec.logical.databases || []).join(', ')} onChange={e => setSpec(s => ({ ...s, logical: { ...s.logical, databases: e.target.value ? e.target.value.split(',').map(d => d.trim()).filter(Boolean) : [] } }))} placeholder="Leave empty for all databases" />
                    <span className="form-hint">Comma-separated list, or leave empty to dump all databases</span>
                  </div>
                </div>
              </>
            )}
          </section>
        )}

        {activeTab === 'destination' && (
          <section className="form-section">
            <h4>Destination</h4>
            <div className="form-grid">
              <div className="form-row">
                <label>Type</label>
                <select className="input" value={spec.destination.type} onChange={e => setDestType(e.target.value)}>
                  <option value="s3">S3 / S3-Compatible (MinIO)</option>
                  <option value="gcs">Google Cloud Storage</option>
                  <option value="sftp">SFTP</option>
                  <option value="local">Local PVC</option>
                </select>
              </div>
            </div>

            {spec.destination.type === 's3' && spec.destination.s3 && (
              <>
                <h4 style={{ marginTop: 20 }}>S3 Configuration</h4>
                <div className="form-grid">
                  <div className="form-row">
                    <label>Bucket</label>
                    <input className="input" value={spec.destination.s3.bucket} onChange={e => setSpec(s => ({ ...s, destination: { ...s.destination, s3: { ...s.destination.s3, bucket: e.target.value } } }))} placeholder="my-pg-backups" />
                  </div>
                  <div className="form-row">
                    <label>Region</label>
                    <input className="input" value={spec.destination.s3.region} onChange={e => setSpec(s => ({ ...s, destination: { ...s.destination, s3: { ...s.destination.s3, region: e.target.value } } }))} placeholder="us-east-1" />
                  </div>
                  <div className="form-row">
                    <label>Endpoint (for S3-compatible)</label>
                    <input className="input" value={spec.destination.s3.endpoint || ''} onChange={e => setSpec(s => ({ ...s, destination: { ...s.destination, s3: { ...s.destination.s3, endpoint: e.target.value } } }))} placeholder="https://minio.example.com" />
                    <span className="form-hint">Leave empty for AWS S3</span>
                  </div>
                  <div className="form-row">
                    <label>Path Prefix</label>
                    <input className="input" value={spec.destination.s3.path_prefix || ''} onChange={e => setSpec(s => ({ ...s, destination: { ...s.destination, s3: { ...s.destination.s3, path_prefix: e.target.value } } }))} placeholder="pg-swarm/" />
                  </div>
                  <div className="form-row">
                    <label>Access Key ID</label>
                    <input className="input" value={spec.destination.s3.access_key_id || ''} onChange={e => setSpec(s => ({ ...s, destination: { ...s.destination, s3: { ...s.destination.s3, access_key_id: e.target.value } } }))} placeholder="AKIA..." />
                    <span className="form-hint">Stored as a K8s Secret on each satellite</span>
                  </div>
                  <div className="form-row">
                    <label>Secret Access Key</label>
                    <input className="input" type="password" value={spec.destination.s3.secret_access_key || ''} onChange={e => setSpec(s => ({ ...s, destination: { ...s.destination, s3: { ...s.destination.s3, secret_access_key: e.target.value } } }))} />
                  </div>
                </div>
              </>
            )}

            {spec.destination.type === 'gcs' && spec.destination.gcs && (
              <>
                <h4 style={{ marginTop: 20 }}>GCS Configuration</h4>
                <div className="form-grid">
                  <div className="form-row">
                    <label>Bucket</label>
                    <input className="input" value={spec.destination.gcs.bucket} onChange={e => setSpec(s => ({ ...s, destination: { ...s.destination, gcs: { ...s.destination.gcs, bucket: e.target.value } } }))} placeholder="my-pg-backups" />
                  </div>
                  <div className="form-row">
                    <label>Path Prefix</label>
                    <input className="input" value={spec.destination.gcs.path_prefix || ''} onChange={e => setSpec(s => ({ ...s, destination: { ...s.destination, gcs: { ...s.destination.gcs, path_prefix: e.target.value } } }))} placeholder="pg-swarm/" />
                  </div>
                  <div className="form-row form-row-wide">
                    <label>Service Account JSON</label>
                    <textarea className="input" rows="6" value={spec.destination.gcs.service_account_json || ''} onChange={e => setSpec(s => ({ ...s, destination: { ...s.destination, gcs: { ...s.destination.gcs, service_account_json: e.target.value } } }))} placeholder='Paste service account JSON or use the upload button' style={{ fontFamily: 'var(--mono)', fontSize: 12 }} />
                    <div style={{ marginTop: 6 }}>
                      <label className="btn btn-sm" style={{ cursor: 'pointer' }}>
                        Upload JSON file
                        <input type="file" accept=".json" style={{ display: 'none' }} onChange={e => {
                          const file = e.target.files[0];
                          if (!file) return;
                          const reader = new FileReader();
                          reader.onload = ev => setSpec(s => ({ ...s, destination: { ...s.destination, gcs: { ...s.destination.gcs, service_account_json: ev.target.result } } }));
                          reader.readAsText(file);
                        }} />
                      </label>
                    </div>
                    <span className="form-hint">Stored as a K8s Secret on each satellite</span>
                  </div>
                </div>
              </>
            )}

            {spec.destination.type === 'sftp' && spec.destination.sftp && (
              <>
                <h4 style={{ marginTop: 20 }}>SFTP Configuration</h4>
                <div className="form-grid">
                  <div className="form-row">
                    <label>Host</label>
                    <input className="input" value={spec.destination.sftp.host} onChange={e => setSpec(s => ({ ...s, destination: { ...s.destination, sftp: { ...s.destination.sftp, host: e.target.value } } }))} placeholder="backup.example.com" />
                  </div>
                  <div className="form-row">
                    <label>Port</label>
                    <input className="input" type="number" value={spec.destination.sftp.port || 22} onChange={e => setSpec(s => ({ ...s, destination: { ...s.destination, sftp: { ...s.destination.sftp, port: parseInt(e.target.value) || 22 } } }))} />
                  </div>
                  <div className="form-row">
                    <label>Username</label>
                    <input className="input" value={spec.destination.sftp.user} onChange={e => setSpec(s => ({ ...s, destination: { ...s.destination, sftp: { ...s.destination.sftp, user: e.target.value } } }))} placeholder="backup-user" />
                  </div>
                  <div className="form-row">
                    <label>Password</label>
                    <input className="input" type="password" value={spec.destination.sftp.password || ''} onChange={e => setSpec(s => ({ ...s, destination: { ...s.destination, sftp: { ...s.destination.sftp, password: e.target.value } } }))} />
                    <span className="form-hint">Stored as a K8s Secret on each satellite</span>
                  </div>
                  <div className="form-row">
                    <label>Base Path</label>
                    <input className="input" value={spec.destination.sftp.base_path} onChange={e => setSpec(s => ({ ...s, destination: { ...s.destination, sftp: { ...s.destination.sftp, base_path: e.target.value } } }))} placeholder="/backups/pg-swarm" />
                  </div>
                </div>
              </>
            )}

            {spec.destination.type === 'local' && spec.destination.local && (
              <>
                <h4 style={{ marginTop: 20 }}>Local PVC Configuration</h4>
                <div className="form-grid">
                  <div className="form-row">
                    <label>Volume Size</label>
                    <input className="input" value={spec.destination.local.size} onChange={e => setSpec(s => ({ ...s, destination: { ...s.destination, local: { ...s.destination.local, size: e.target.value } } }))} placeholder="50Gi" />
                  </div>
                  <div className="form-row">
                    <label>Storage Class (optional)</label>
                    <input className="input" value={spec.destination.local.storage_class || ''} onChange={e => setSpec(s => ({ ...s, destination: { ...s.destination, local: { ...s.destination.local, storage_class: e.target.value } } }))} placeholder="Default storage class" />
                  </div>
                </div>
              </>
            )}
          </section>
        )}

        {activeTab === 'retention' && (
          <section className="form-section">
            <h4>Retention Policy</h4>
            <div className="form-grid">
              {spec.physical && (() => {
                const baseCount = spec.retention.base_backup_count || 7;
                const walDays = spec.retention.wal_retention_days || 14;
                const minDays = minWalRetentionDays(spec.physical.base_schedule, baseCount);
                const walTooShort = spec.physical.wal_archive_enabled && minDays && walDays < minDays;
                return (
                  <>
                    <div className="form-row">
                      <label>Base Backup Count</label>
                      <input className="input" type="number" min="1" value={baseCount} onChange={e => setSpec(s => ({ ...s, retention: { ...s.retention, base_backup_count: parseInt(e.target.value) || 7 } }))} />
                      <span className="form-hint">Number of full base backups to keep before oldest is deleted</span>
                    </div>
                    {spec.physical.incremental_schedule && (
                      <div className="form-row">
                        <label>Incremental Backup Count</label>
                        <input className="input" type="number" min="1" value={spec.retention.incremental_backup_count || 6} onChange={e => setSpec(s => ({ ...s, retention: { ...s.retention, incremental_backup_count: parseInt(e.target.value) || 6 } }))} />
                        <span className="form-hint">Number of incremental backups to keep per full backup cycle</span>
                      </div>
                    )}
                    {spec.physical.wal_archive_enabled && (
                      <div className="form-row">
                        <label>WAL Retention Days</label>
                        <input className={'input' + (walTooShort ? ' input-warn' : '')} type="number" min="1" value={walDays} onChange={e => setSpec(s => ({ ...s, retention: { ...s.retention, wal_retention_days: parseInt(e.target.value) || 14 } }))} />
                        {walTooShort ? (
                          <span className="form-warn">
                            WAL retention ({walDays}d) is shorter than the span covered by {baseCount} base backups ({minDays}d).
                            Older base backups won't support PITR.{' '}
                            <button className="btn-link" onClick={() => setSpec(s => ({ ...s, retention: { ...s.retention, wal_retention_days: minDays } }))}>
                              Set to {minDays} days
                            </button>
                          </span>
                        ) : (
                          <span className="form-hint">
                            Days of WAL segments to retain for point-in-time recovery
                            {minDays && <> (minimum {minDays}d to cover all {baseCount} base backups)</>}
                          </span>
                        )}
                      </div>
                    )}
                  </>
                );
              })()}
              {spec.logical && (
                <div className="form-row">
                  <label>Logical Backup Count</label>
                  <input className="input" type="number" min="1" value={spec.retention.logical_backup_count || 30} onChange={e => setSpec(s => ({ ...s, retention: { ...s.retention, logical_backup_count: parseInt(e.target.value) || 30 } }))} />
                  <span className="form-hint">Number of logical dumps to keep before oldest is deleted</span>
                </div>
              )}
            </div>
          </section>
        )}
          </div>
        </div>

        <div className="confirm-footer">
          <button className="btn" onClick={onCancel}>Cancel</button>
          <button className="btn btn-approve" onClick={handleSave}>Save</button>
        </div>

        {showConfirm && (
          <div className="confirm-overlay" onClick={() => setShowConfirm(false)}>
            <div className="confirm-modal" onClick={e => e.stopPropagation()}>
              <div className="confirm-header">
                <h3>{state.isNew ? 'Create' : 'Update'} Backup Rule</h3>
                <button className="modal-close" onClick={() => setShowConfirm(false)}><X size={18} /></button>
              </div>
              <div className="confirm-body">
                <p>This will {state.isNew ? 'create' : 'update'} the backup rule <strong>{state.name}</strong>. If attached to profiles, all linked clusters will be re-pushed with the new backup configuration.</p>
              </div>
              <div className="confirm-footer">
                <button className="btn" onClick={() => setShowConfirm(false)} disabled={saving}>Cancel</button>
                <button className="btn btn-approve" onClick={confirmSave} disabled={saving}>{saving ? 'Saving…' : 'Confirm'}</button>
              </div>
            </div>
          </div>
        )}
      </div>
    </div>
  );
}
