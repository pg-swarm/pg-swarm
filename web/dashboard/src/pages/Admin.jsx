import { useState, useEffect } from 'react';
import { useData } from '../context/DataContext';
import { useToast } from '../context/ToastContext';
import { api } from '../api';
import {
  Plus, Pencil, Trash2, Save, X, Star, Settings, Layers, Box, Database, Shield, HardDrive
} from 'lucide-react';
import RecoveryRulesTab from '../components/RecoveryRulesTab';

export default function Admin() {
  const { postgresVersions, postgresVariants, storageTiers, backupStores, refresh } = useData();
  const toast = useToast();

  useEffect(() => { document.title = 'Admin - PG-Swarm'; }, []);
  const [activeTab, setActiveTab] = useState('tiers');

  const tabs = [
    { key: 'tiers', label: 'Storage Tiers', icon: <Layers size={14} /> },
    { key: 'stores', label: 'Backup Stores', icon: <HardDrive size={14} /> },
    { key: 'variants', label: 'Image Variants', icon: <Box size={14} /> },
    { key: 'versions', label: 'PG Versions', icon: <Database size={14} /> },
    { key: 'recovery', label: 'Recovery Rules', icon: <Shield size={14} /> },
  ];

  return (
    <>
      <div className="tab-bar">
        {tabs.map(t => (
          <button
            key={t.key}
            className={'tab-item' + (activeTab === t.key ? ' active' : '')}
            onClick={() => setActiveTab(t.key)}
          >
            {t.icon} {t.label}
          </button>
        ))}
      </div>
      <div className="card">

      {activeTab === 'tiers' && <StorageTiersTab storageTiers={storageTiers} refresh={refresh} toast={toast} />}
      {activeTab === 'stores' && <BackupStoresTab backupStores={backupStores} refresh={refresh} toast={toast} />}
      {activeTab === 'variants' && <VariantsTab postgresVariants={postgresVariants} refresh={refresh} toast={toast} />}
      {activeTab === 'versions' && <VersionsTab postgresVersions={postgresVersions} postgresVariants={postgresVariants} refresh={refresh} toast={toast} />}
      {activeTab === 'recovery' && <RecoveryRulesTab toast={toast} />}
      </div>
    </>
  );
}

function StorageTiersTab({ storageTiers, refresh, toast }) {
  const [editing, setEditing] = useState(null);
  const [form, setForm] = useState({ name: '', description: '' });

  function startCreate() {
    setForm({ name: '', description: '' });
    setEditing('new');
  }

  function startEdit(t) {
    setForm({ name: t.name, description: t.description });
    setEditing(t.id);
  }

  async function save() {
    try {
      if (editing === 'new') {
        await api.createStorageTier(form);
        toast('Storage tier added');
      } else {
        await api.updateStorageTier(editing, form);
        toast('Storage tier updated');
      }
      setEditing(null);
      refresh();
    } catch (e) {
      toast('Save failed: ' + e.message, true);
    }
  }

  async function remove(id) {
    try {
      await api.deleteStorageTier(id);
      toast('Storage tier removed');
      refresh();
    } catch (e) {
      toast('Delete failed: ' + e.message, true);
    }
  }

  return (
    <>
      <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', padding: '12px 20px' }}>
        <span className="sm muted">Abstract storage tiers used in profiles. Each satellite maps these to its concrete storage classes.</span>
        <button className="btn btn-approve" onClick={startCreate}><Plus size={14} /> Add Tier</button>
      </div>

      {editing && (
        <div className="admin-form-bar">
          <input className="input" placeholder="Name (e.g. fast)" value={form.name}
            onChange={e => setForm(f => ({ ...f, name: e.target.value }))} style={{ width: 140 }} />
          <input className="input" placeholder="Description (optional)" value={form.description}
            onChange={e => setForm(f => ({ ...f, description: e.target.value }))} style={{ flex: 1 }} />
          <button className="btn btn-approve" onClick={save}><Save size={13} /> Save</button>
          <button className="btn btn-reject" onClick={() => setEditing(null)}><X size={13} /> Cancel</button>
        </div>
      )}

      {storageTiers.length === 0 && !editing ? (
        <div className="empty-state" style={{ padding: '40px 20px' }}>
          <Layers size={48} strokeWidth={1.2} />
          <h3>No storage tiers defined</h3>
          <p>Add tiers like "fast", "replicated", or "snapshotted" to abstract storage classes across clusters.</p>
        </div>
      ) : (
        <table>
          <thead>
            <tr>
              <th>Name</th>
              <th>Description</th>
              <th style={{ width: 180 }}>Actions</th>
            </tr>
          </thead>
          <tbody>
            {storageTiers.map(t => (
              <tr key={t.id}>
                <td className="mono">{t.name}</td>
                <td className="sm muted">{t.description || '-'}</td>
                <td>
                  <div className="actions">
                    <button className="btn btn-sm" onClick={() => startEdit(t)}><Pencil size={11} /> Edit</button>
                    <button className="btn btn-sm btn-reject" onClick={() => remove(t.id)}><Trash2 size={11} /> Delete</button>
                  </div>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </>
  );
}

const STORE_TYPES = ['s3', 'gcs', 'sftp', 'local'];

const STORE_TYPE_LABELS = { s3: 'S3', gcs: 'GCS', sftp: 'SFTP', local: 'Local' };

function emptyStoreForm() {
  return { name: '', description: '', store_type: 's3', config: {}, credentials: {} };
}

function configFieldsForType(type) {
  switch (type) {
    case 's3': return [
      { key: 'bucket', label: 'Bucket', required: true },
      { key: 'region', label: 'Region' },
      { key: 'endpoint', label: 'Endpoint' },
      { key: 'force_path_style', label: 'Force Path Style', type: 'checkbox' },
    ];
    case 'gcs': return [
      { key: 'bucket', label: 'Bucket', required: true },
      { key: 'path_prefix', label: 'Path Prefix' },
    ];
    case 'sftp': return [
      { key: 'host', label: 'Host', required: true },
      { key: 'port', label: 'Port', type: 'number' },
      { key: 'user', label: 'User' },
      { key: 'base_path', label: 'Base Path', required: true },
    ];
    case 'local': return [
      { key: 'size', label: 'Size (e.g. 20Gi)', required: true },
      { key: 'storage_class', label: 'Storage Class' },
    ];
    default: return [];
  }
}

function credentialFieldsForType(type) {
  switch (type) {
    case 's3': return [
      { key: 'access_key_id', label: 'Access Key ID' },
      { key: 'secret_access_key', label: 'Secret Access Key' },
    ];
    case 'gcs': return [
      { key: 'service_account_json', label: 'Service Account JSON', type: 'textarea', accept_file: '.json', required: true },
    ];
    case 'sftp': return [
      { key: 'password', label: 'Password' },
      { key: 'private_key', label: 'Private Key', type: 'textarea' },
    ];
    default: return [];
  }
}

function configSummary(type, config) {
  if (!config) return '-';
  switch (type) {
    case 's3': return [config.bucket, config.region].filter(Boolean).join(' / ') || '-';
    case 'gcs': return [config.bucket, config.path_prefix].filter(Boolean).join('/') || '-';
    case 'sftp': return [config.host, config.base_path].filter(Boolean).join(':') || '-';
    case 'local': return config.size || '-';
    default: return '-';
  }
}

function BackupStoresTab({ backupStores, refresh, toast }) {
  const [editing, setEditing] = useState(null); // null | 'new' | store id
  const [form, setForm] = useState(emptyStoreForm());

  function startCreate() {
    setForm(emptyStoreForm());
    setEditing('new');
  }

  function startEdit(store) {
    setForm({
      name: store.name,
      description: store.description,
      store_type: store.store_type,
      config: typeof store.config === 'string' ? JSON.parse(store.config) : (store.config || {}),
      credentials: {},
      credentials_set: store.credentials_set || {},
    });
    setEditing(store.id);
  }

  function setConfig(key, value) {
    setForm(f => ({ ...f, config: { ...f.config, [key]: value } }));
  }

  function setCred(key, value) {
    setForm(f => ({ ...f, credentials: { ...f.credentials, [key]: value } }));
  }

  function changeType(newType) {
    setForm(f => ({ ...f, store_type: newType, config: {}, credentials: {}, credentials_set: {} }));
  }

  async function save() {
    if (!form.name.trim()) { toast('Name is required', true); return; }

    // Validate required credential fields on create, or when editing and not already set
    const credFields = credentialFieldsForType(form.store_type);
    for (const f of credFields) {
      if (f.required && editing === 'new' && !(form.credentials[f.key] || '').trim()) {
        toast(f.label + ' is required', true); return;
      }
    }

    const payload = {
      name: form.name.trim(),
      description: form.description,
      store_type: form.store_type,
      config: form.config,
    };
    // Only send credentials if user entered new values
    const hasNewCreds = Object.values(form.credentials || {}).some(v => v && v.length > 0);
    if (hasNewCreds) {
      payload.credentials = form.credentials;
    }

    try {
      if (editing === 'new') {
        await api.createBackupStore(payload);
        toast('Backup store created');
      } else {
        await api.updateBackupStore(editing, payload);
        toast('Backup store updated');
      }
      setEditing(null);
      refresh();
    } catch (e) {
      toast('Save failed: ' + e.message, true);
    }
  }

  async function remove(id) {
    try {
      await api.deleteBackupStore(id);
      toast('Backup store removed');
      refresh();
    } catch (e) {
      toast('Delete failed: ' + e.message, true);
    }
  }

  if (editing) {
    const cfgFields = configFieldsForType(form.store_type);
    const credFields = credentialFieldsForType(form.store_type);

    return (
      <div style={{ padding: '16px 20px' }}>
        <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', marginBottom: 16 }}>
          <h4 style={{ margin: 0 }}>{editing === 'new' ? 'New Backup Store' : 'Edit Backup Store'}</h4>
          <div className="actions">
            <button className="btn btn-approve" onClick={save}><Save size={13} /> Save</button>
            <button className="btn btn-reject" onClick={() => setEditing(null)}><X size={13} /> Cancel</button>
          </div>
        </div>

        <div className="form-grid" style={{ display: 'grid', gridTemplateColumns: '1fr 1fr 1fr', gap: 12, marginBottom: 16 }}>
          <div className="form-row">
            <label>Name</label>
            <input className="input" value={form.name} onChange={e => setForm(f => ({ ...f, name: e.target.value }))} placeholder="e.g. prod-s3" />
          </div>
          <div className="form-row">
            <label>Description</label>
            <input className="input" value={form.description} onChange={e => setForm(f => ({ ...f, description: e.target.value }))} placeholder="Optional description" />
          </div>
          <div className="form-row">
            <label>Store Type</label>
            <select className="input" value={form.store_type} onChange={e => changeType(e.target.value)}>
              {STORE_TYPES.map(t => <option key={t} value={t}>{STORE_TYPE_LABELS[t]}</option>)}
            </select>
          </div>
        </div>

        {cfgFields.length > 0 && (
          <>
            <h5 style={{ margin: '12px 0 8px', color: 'var(--text-secondary)', fontSize: 11, textTransform: 'uppercase', letterSpacing: '0.5px' }}>Connection</h5>
            <div className="form-grid" style={{ display: 'grid', gridTemplateColumns: '1fr 1fr', gap: 12, marginBottom: 16 }}>
              {cfgFields.map(f => (
                <div className="form-row" key={f.key}>
                  <label>{f.label}{f.required ? ' *' : ''}</label>
                  {f.type === 'checkbox' ? (
                    <label style={{ display: 'flex', alignItems: 'center', gap: 6 }}>
                      <input type="checkbox" checked={!!form.config[f.key]} onChange={e => setConfig(f.key, e.target.checked)} />
                      <span>{f.label}</span>
                    </label>
                  ) : (
                    <input className="input" type={f.type || 'text'} value={form.config[f.key] || ''} onChange={e => setConfig(f.key, f.type === 'number' ? parseInt(e.target.value) || 0 : e.target.value)} />
                  )}
                </div>
              ))}
            </div>
          </>
        )}

        {credFields.length > 0 && (
          <>
            <h5 style={{ margin: '12px 0 8px', color: 'var(--text-secondary)', fontSize: 11, textTransform: 'uppercase', letterSpacing: '0.5px' }}>Credentials</h5>
            <div className="form-grid" style={{ display: 'grid', gridTemplateColumns: '1fr 1fr', gap: 12 }}>
              {credFields.map(f => (
                <div className="form-row" key={f.key} style={f.type === 'textarea' ? { gridColumn: '1 / -1' } : {}}>
                  <label style={{ display: 'flex', alignItems: 'center', gap: 8 }}>
                    {f.label}{f.required ? ' *' : ''}
                    {f.accept_file && (
                      <label className="btn btn-sm" style={{ cursor: 'pointer', fontSize: 11, padding: '2px 8px' }}>
                        Upload file
                        <input type="file" accept={f.accept_file} style={{ display: 'none' }}
                          onChange={e => {
                            const file = e.target.files?.[0];
                            if (!file) return;
                            const reader = new FileReader();
                            reader.onload = () => setCred(f.key, reader.result);
                            reader.readAsText(file);
                            e.target.value = '';
                          }} />
                      </label>
                    )}
                  </label>
                  {f.type === 'textarea' ? (
                    <textarea className="input" rows={4} value={form.credentials[f.key] || ''} onChange={e => setCred(f.key, e.target.value)}
                      placeholder={form.credentials_set?.[f.key] ? '••••••••  (leave blank to keep existing)' : ''} />
                  ) : (
                    <input className="input" type="text" value={form.credentials[f.key] || ''} onChange={e => setCred(f.key, e.target.value)}
                      placeholder={form.credentials_set?.[f.key] ? '••••••••  (leave blank to keep existing)' : ''} />
                  )}
                </div>
              ))}
            </div>
          </>
        )}
      </div>
    );
  }

  return (
    <>
      <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', padding: '12px 20px' }}>
        <span className="sm muted">Reusable storage destinations for backups. Credentials are encrypted at rest.</span>
        <button className="btn btn-approve" onClick={startCreate}><Plus size={14} /> Add Store</button>
      </div>

      {backupStores.length === 0 ? (
        <div className="empty-state" style={{ padding: '40px 20px' }}>
          <HardDrive size={48} strokeWidth={1.2} />
          <h3>No backup stores defined</h3>
          <p>Add storage destinations like S3, GCS, SFTP, or local PVCs for your backup configurations.</p>
        </div>
      ) : (
        <table>
          <thead>
            <tr>
              <th>Name</th>
              <th>Type</th>
              <th>Config</th>
              <th>Credentials</th>
              <th style={{ width: 180 }}>Actions</th>
            </tr>
          </thead>
          <tbody>
            {backupStores.map(s => (
              <tr key={s.id}>
                <td>
                  <div className="mono">{s.name}</div>
                  {s.description && <div className="sm muted">{s.description}</div>}
                </td>
                <td><span className="badge">{STORE_TYPE_LABELS[s.store_type] || s.store_type}</span></td>
                <td className="sm">{configSummary(s.store_type, s.config)}</td>
                <td className="sm">
                  {s.credentials_set && Object.keys(s.credentials_set).length > 0
                    ? <span style={{ color: 'var(--green)' }}>Set</span>
                    : <span className="muted">Not set</span>}
                </td>
                <td>
                  <div className="actions">
                    <button className="btn btn-sm" onClick={() => startEdit(s)}><Pencil size={11} /> Edit</button>
                    <button className="btn btn-sm btn-reject" onClick={() => remove(s.id)}><Trash2 size={11} /> Delete</button>
                  </div>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </>
  );
}

function VariantsTab({ postgresVariants, refresh, toast }) {
  const [showForm, setShowForm] = useState(false);
  const [form, setForm] = useState({ name: '', description: '' });

  async function addVariant() {
    if (!form.name.trim()) return;
    try {
      await api.createPostgresVariant({ name: form.name.trim(), description: form.description.trim() });
      toast('Variant added');
      setForm({ name: '', description: '' });
      setShowForm(false);
      refresh();
    } catch (e) {
      toast('Failed: ' + e.message, true);
    }
  }

  async function removeVariant(id) {
    try {
      await api.deletePostgresVariant(id);
      toast('Variant removed');
      refresh();
    } catch (e) {
      toast('Delete failed: ' + e.message, true);
    }
  }

  return (
    <>
      <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', padding: '12px 20px' }}>
        <span className="sm muted">Base image variants available for PostgreSQL versions.</span>
        <button className="btn btn-approve" onClick={() => setShowForm(true)}><Plus size={14} /> Add Variant</button>
      </div>

      {showForm && (
        <div className="admin-form-bar">
          <input className="input" placeholder="Name (e.g. bookworm)" value={form.name}
            onChange={e => setForm(f => ({ ...f, name: e.target.value }))} style={{ width: 140 }} />
          <input className="input" placeholder="Description (optional)" value={form.description}
            onChange={e => setForm(f => ({ ...f, description: e.target.value }))} style={{ flex: 1 }} />
          <button className="btn btn-approve" onClick={addVariant}><Save size={13} /> Save</button>
          <button className="btn btn-reject" onClick={() => setShowForm(false)}><X size={13} /> Cancel</button>
        </div>
      )}

      {postgresVariants.length === 0 && !showForm ? (
        <div className="empty-state" style={{ padding: '40px 20px' }}>
          <Box size={48} strokeWidth={1.2} />
          <h3>No variants configured</h3>
          <p>Add variants like "alpine" or "debian".</p>
        </div>
      ) : (
        <table>
          <thead>
            <tr>
              <th>Name</th>
              <th>Description</th>
              <th style={{ width: 100 }}>Actions</th>
            </tr>
          </thead>
          <tbody>
            {postgresVariants.map(v => (
              <tr key={v.id}>
                <td className="mono">{v.name}</td>
                <td className="sm muted">{v.description || '-'}</td>
                <td>
                  <button className="btn btn-sm btn-reject" onClick={() => removeVariant(v.id)}><Trash2 size={11} /> Delete</button>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </>
  );
}

function VersionsTab({ postgresVersions, postgresVariants, refresh, toast }) {
  const [editing, setEditing] = useState(null);
  const [form, setForm] = useState({ version: '', variant: '', image_tag: '' });

  const defaultVariant = postgresVariants.length > 0 ? postgresVariants[0].name : 'alpine';

  function startCreate() {
    setForm({ version: '', variant: defaultVariant, image_tag: '' });
    setEditing('new');
  }

  function startEdit(pv) {
    setForm({ version: pv.version, variant: pv.variant, image_tag: pv.image_tag });
    setEditing(pv.id);
  }

  async function save() {
    try {
      if (editing === 'new') {
        await api.createPostgresVersion(form);
        toast('Version added');
      } else {
        await api.updatePostgresVersion(editing, form);
        toast('Version updated');
      }
      setEditing(null);
      refresh();
    } catch (e) {
      toast('Save failed: ' + e.message, true);
    }
  }

  async function remove(id) {
    try {
      await api.deletePostgresVersion(id);
      toast('Version removed');
      refresh();
    } catch (e) {
      toast('Delete failed: ' + e.message, true);
    }
  }

  async function setDefault(id) {
    try {
      await api.setDefaultPostgresVersion(id);
      toast('Default version set');
      refresh();
    } catch (e) {
      toast('Failed: ' + e.message, true);
    }
  }

  return (
    <>
      <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', padding: '12px 20px' }}>
        <span className="sm muted">
          The default version is pre-selected when creating new profiles. Image is resolved as{' '}
          <code style={{ background: 'var(--gray-bg)', padding: '1px 6px', borderRadius: 3 }}>[registry/]postgres:image_tag</code>{' '}
          at deployment time.
        </span>
        <button className="btn btn-approve" onClick={startCreate}><Plus size={14} /> Add Version</button>
      </div>

      {editing && (
        <div className="admin-form-bar">
          <input className="input" placeholder="Version (e.g. 17)" value={form.version}
            onChange={e => setForm(f => ({ ...f, version: e.target.value }))} style={{ width: 100 }} />
          <select className="input" value={form.variant}
            onChange={e => setForm(f => ({ ...f, variant: e.target.value }))} style={{ width: 140 }}>
            {postgresVariants.map(v => (
              <option key={v.id} value={v.name}>{v.name.charAt(0).toUpperCase() + v.name.slice(1)}</option>
            ))}
            {postgresVariants.length === 0 && <option value="">No variants</option>}
          </select>
          <input className="input" placeholder="Image tag (e.g. 17.9-alpine3.23)" value={form.image_tag}
            onChange={e => setForm(f => ({ ...f, image_tag: e.target.value }))} style={{ flex: 1 }} />
          <button className="btn btn-approve" onClick={save}><Save size={13} /> Save</button>
          <button className="btn btn-reject" onClick={() => setEditing(null)}><X size={13} /> Cancel</button>
        </div>
      )}

      {postgresVersions.length === 0 && !editing ? (
        <div className="empty-state" style={{ padding: '40px 20px' }}>
          <Settings size={48} strokeWidth={1.2} />
          <h3>No PostgreSQL versions configured</h3>
          <p>Add a version to get started with cluster profiles.</p>
        </div>
      ) : (
        <table>
          <thead>
            <tr>
              <th>Version</th>
              <th>Variant</th>
              <th>Image Tag</th>
              <th>Default</th>
              <th style={{ width: 240 }}>Actions</th>
            </tr>
          </thead>
          <tbody>
            {postgresVersions.map(pv => (
              <tr key={pv.id}>
                <td className="mono">{pv.version}</td>
                <td>{pv.variant}</td>
                <td className="mono sm">{pv.image_tag}</td>
                <td>
                  {pv.is_default
                    ? <span className="badge badge-green"><Star size={11} /> Default</span>
                    : <button className="btn btn-sm" onClick={() => setDefault(pv.id)}><Star size={11} /> Set Default</button>}
                </td>
                <td>
                  <div className="actions">
                    <button className="btn btn-sm" onClick={() => startEdit(pv)}><Pencil size={11} /> Edit</button>
                    <button className="btn btn-sm btn-reject" onClick={() => remove(pv.id)}><Trash2 size={11} /> Delete</button>
                  </div>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </>
  );
}
