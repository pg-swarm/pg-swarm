import { useState } from 'react';
import { useData } from '../context/DataContext';
import { useToast } from '../context/ToastContext';
import { api, parseSpec, timeAgo } from '../api';

const DEST_TYPES = ['s3', 'gcs', 'sftp', 'local'];

function emptySpec() {
  return {
    physical: { base_schedule: '0 2 * * 0', wal_archive_enabled: true, archive_timeout_seconds: 60 },
    logical: null,
    destination: { type: 's3', s3: { bucket: '', region: '', endpoint: '', path_prefix: '' } },
    retention: { base_backup_count: 7, wal_retention_days: 14, logical_backup_count: 30 },
  };
}

export default function BackupRules() {
  const { backupRules, refresh } = useData();
  const toast = useToast();
  const [editing, setEditing] = useState(null); // null | 'new' | rule.id
  const [form, setForm] = useState({ name: '', description: '', spec: emptySpec() });
  const [busy, setBusy] = useState(false);

  function startCreate() {
    setForm({ name: '', description: '', spec: emptySpec() });
    setEditing('new');
  }

  function startEdit(rule) {
    setForm({ name: rule.name, description: rule.description, spec: parseSpec(rule.config) });
    setEditing(rule.id);
  }

  async function save() {
    setBusy(true);
    try {
      const data = { name: form.name, description: form.description, config: form.spec };
      if (editing === 'new') {
        await api.createBackupRule(data);
        toast.success('Backup rule created');
      } else {
        await api.updateBackupRule(editing, data);
        toast.success('Backup rule updated');
      }
      setEditing(null);
      refresh();
    } catch (err) {
      toast.error(err.message);
    } finally {
      setBusy(false);
    }
  }

  async function remove(id) {
    if (!confirm('Delete this backup rule?')) return;
    try {
      await api.deleteBackupRule(id);
      toast.success('Backup rule deleted');
      refresh();
    } catch (err) {
      toast.error(err.message);
    }
  }

  function updateSpec(path, value) {
    setForm(prev => {
      const spec = JSON.parse(JSON.stringify(prev.spec));
      const parts = path.split('.');
      let obj = spec;
      for (let i = 0; i < parts.length - 1; i++) obj = obj[parts[i]];
      obj[parts[parts.length - 1]] = value;
      return { ...prev, spec };
    });
  }

  function togglePhysical() {
    setForm(prev => {
      const spec = { ...prev.spec };
      spec.physical = spec.physical ? null : { base_schedule: '0 2 * * 0', wal_archive_enabled: true, archive_timeout_seconds: 60 };
      return { ...prev, spec };
    });
  }

  function toggleLogical() {
    setForm(prev => {
      const spec = { ...prev.spec };
      spec.logical = spec.logical ? null : { schedule: '0 2 * * *', databases: [], format: 'custom' };
      return { ...prev, spec };
    });
  }

  function setDestType(type) {
    setForm(prev => {
      const spec = { ...prev.spec };
      const dest = { type };
      if (type === 's3') dest.s3 = { bucket: '', region: '', endpoint: '', path_prefix: '' };
      if (type === 'gcs') dest.gcs = { bucket: '', path_prefix: '' };
      if (type === 'sftp') dest.sftp = { host: '', port: 22, user: '', base_path: '' };
      if (type === 'local') dest.local = { size: '10Gi', storage_class: '' };
      spec.destination = dest;
      return { ...prev, spec };
    });
  }

  if (editing !== null) {
    const spec = form.spec;
    return (
      <div className="page">
        <div className="page-header">
          <h1>{editing === 'new' ? 'Create Backup Rule' : 'Edit Backup Rule'}</h1>
        </div>

        <div className="card" style={{ maxWidth: 700 }}>
          <label className="field-label">Name</label>
          <input className="input" value={form.name} onChange={e => setForm({ ...form, name: e.target.value })} />

          <label className="field-label" style={{ marginTop: 12 }}>Description</label>
          <input className="input" value={form.description} onChange={e => setForm({ ...form, description: e.target.value })} />

          {/* Physical Backup */}
          <div style={{ marginTop: 16 }}>
            <label style={{ display: 'flex', alignItems: 'center', gap: 8 }}>
              <input type="checkbox" checked={!!spec.physical} onChange={togglePhysical} />
              <strong>Physical Backup (pg_basebackup)</strong>
            </label>
            {spec.physical && (
              <div style={{ marginLeft: 24, marginTop: 8 }}>
                <label className="field-label">Base Schedule (cron)</label>
                <input className="input" value={spec.physical.base_schedule} onChange={e => updateSpec('physical.base_schedule', e.target.value)} />
                <label style={{ display: 'flex', alignItems: 'center', gap: 8, marginTop: 8 }}>
                  <input type="checkbox" checked={spec.physical.wal_archive_enabled} onChange={e => updateSpec('physical.wal_archive_enabled', e.target.checked)} />
                  Enable WAL archiving
                </label>
              </div>
            )}
          </div>

          {/* Logical Backup */}
          <div style={{ marginTop: 16 }}>
            <label style={{ display: 'flex', alignItems: 'center', gap: 8 }}>
              <input type="checkbox" checked={!!spec.logical} onChange={toggleLogical} />
              <strong>Logical Backup (pg_dump)</strong>
            </label>
            {spec.logical && (
              <div style={{ marginLeft: 24, marginTop: 8 }}>
                <label className="field-label">Schedule (cron)</label>
                <input className="input" value={spec.logical.schedule} onChange={e => updateSpec('logical.schedule', e.target.value)} />
                <label className="field-label" style={{ marginTop: 8 }}>Format</label>
                <select className="input" value={spec.logical.format || 'custom'} onChange={e => updateSpec('logical.format', e.target.value)}>
                  <option value="custom">Custom</option>
                  <option value="plain">Plain SQL</option>
                  <option value="directory">Directory</option>
                </select>
              </div>
            )}
          </div>

          {/* Destination */}
          <div style={{ marginTop: 16 }}>
            <strong>Destination</strong>
            <div style={{ display: 'flex', gap: 8, marginTop: 8 }}>
              {DEST_TYPES.map(t => (
                <button key={t} className={'btn ' + (spec.destination.type === t ? 'btn-primary' : 'btn-ghost')} onClick={() => setDestType(t)}>
                  {t.toUpperCase()}
                </button>
              ))}
            </div>
            <div style={{ marginTop: 8 }}>
              {spec.destination.type === 's3' && spec.destination.s3 && <>
                <label className="field-label">Bucket</label>
                <input className="input" value={spec.destination.s3.bucket} onChange={e => updateSpec('destination.s3.bucket', e.target.value)} />
                <label className="field-label" style={{ marginTop: 8 }}>Region</label>
                <input className="input" value={spec.destination.s3.region} onChange={e => updateSpec('destination.s3.region', e.target.value)} />
                <label className="field-label" style={{ marginTop: 8 }}>Endpoint (optional, for MinIO)</label>
                <input className="input" value={spec.destination.s3.endpoint} onChange={e => updateSpec('destination.s3.endpoint', e.target.value)} />
                <label className="field-label" style={{ marginTop: 8 }}>Path Prefix</label>
                <input className="input" value={spec.destination.s3.path_prefix} onChange={e => updateSpec('destination.s3.path_prefix', e.target.value)} />
              </>}
              {spec.destination.type === 'gcs' && spec.destination.gcs && <>
                <label className="field-label">Bucket</label>
                <input className="input" value={spec.destination.gcs.bucket} onChange={e => updateSpec('destination.gcs.bucket', e.target.value)} />
                <label className="field-label" style={{ marginTop: 8 }}>Path Prefix</label>
                <input className="input" value={spec.destination.gcs.path_prefix} onChange={e => updateSpec('destination.gcs.path_prefix', e.target.value)} />
              </>}
              {spec.destination.type === 'sftp' && spec.destination.sftp && <>
                <label className="field-label">Host</label>
                <input className="input" value={spec.destination.sftp.host} onChange={e => updateSpec('destination.sftp.host', e.target.value)} />
                <label className="field-label" style={{ marginTop: 8 }}>User</label>
                <input className="input" value={spec.destination.sftp.user} onChange={e => updateSpec('destination.sftp.user', e.target.value)} />
                <label className="field-label" style={{ marginTop: 8 }}>Base Path</label>
                <input className="input" value={spec.destination.sftp.base_path} onChange={e => updateSpec('destination.sftp.base_path', e.target.value)} />
              </>}
              {spec.destination.type === 'local' && spec.destination.local && <>
                <label className="field-label">Volume Size</label>
                <input className="input" value={spec.destination.local.size} onChange={e => updateSpec('destination.local.size', e.target.value)} />
                <label className="field-label" style={{ marginTop: 8 }}>Storage Class (optional)</label>
                <input className="input" value={spec.destination.local.storage_class} onChange={e => updateSpec('destination.local.storage_class', e.target.value)} />
              </>}
            </div>
          </div>

          {/* Retention */}
          <div style={{ marginTop: 16 }}>
            <strong>Retention</strong>
            <div style={{ display: 'grid', gridTemplateColumns: '1fr 1fr 1fr', gap: 12, marginTop: 8 }}>
              <div>
                <label className="field-label">Base Backup Count</label>
                <input className="input" type="number" value={spec.retention.base_backup_count} onChange={e => updateSpec('retention.base_backup_count', parseInt(e.target.value) || 0)} />
              </div>
              <div>
                <label className="field-label">WAL Retention Days</label>
                <input className="input" type="number" value={spec.retention.wal_retention_days} onChange={e => updateSpec('retention.wal_retention_days', parseInt(e.target.value) || 0)} />
              </div>
              <div>
                <label className="field-label">Logical Backup Count</label>
                <input className="input" type="number" value={spec.retention.logical_backup_count} onChange={e => updateSpec('retention.logical_backup_count', parseInt(e.target.value) || 0)} />
              </div>
            </div>
          </div>

          <div style={{ display: 'flex', gap: 8, marginTop: 20 }}>
            <button className="btn btn-primary" onClick={save} disabled={busy || !form.name}>
              {busy ? 'Saving...' : 'Save'}
            </button>
            <button className="btn btn-ghost" onClick={() => setEditing(null)}>Cancel</button>
          </div>
        </div>
      </div>
    );
  }

  return (
    <div className="page">
      <div className="page-header">
        <h1>Backup Rules</h1>
        <button className="btn btn-primary" onClick={startCreate}>Create Rule</button>
      </div>

      {backupRules.length === 0 ? (
        <div className="empty-state">No backup rules yet. Create one to start scheduling backups.</div>
      ) : (
        <div className="grid-cards">
          {backupRules.map(rule => {
            const spec = parseSpec(rule.config);
            return (
              <div key={rule.id} className="card">
                <div className="card-header">
                  <h3>{rule.name}</h3>
                  <span className="badge" style={{ textTransform: 'uppercase' }}>
                    {spec.destination?.type || '?'}
                  </span>
                </div>
                {rule.description && <p style={{ color: 'var(--text-secondary)', margin: '4px 0 8px' }}>{rule.description}</p>}
                <div style={{ fontSize: 13, color: 'var(--text-secondary)' }}>
                  {spec.physical && <div>Physical: {spec.physical.base_schedule} {spec.physical.wal_archive_enabled && '+ WAL'}</div>}
                  {spec.logical && <div>Logical: {spec.logical.schedule}</div>}
                  <div>Retention: {spec.retention?.base_backup_count || 7} base, {spec.retention?.wal_retention_days || 14}d WAL, {spec.retention?.logical_backup_count || 30} logical</div>
                </div>
                <div style={{ display: 'flex', gap: 8, marginTop: 12 }}>
                  <button className="btn btn-ghost btn-sm" onClick={() => startEdit(rule)}>Edit</button>
                  <button className="btn btn-danger btn-sm" onClick={() => remove(rule.id)}>Delete</button>
                </div>
                <div style={{ fontSize: 11, color: 'var(--text-tertiary)', marginTop: 8 }}>Created {timeAgo(rule.created_at)}</div>
              </div>
            );
          })}
        </div>
      )}
    </div>
  );
}
