import { useState, useEffect } from 'react';
import { useData } from '../context/DataContext';
import { useToast } from '../context/ToastContext';
import { api } from '../api';
import {
  Plus, Pencil, Trash2, Save, X, Star, Settings, Layers, Box, Database, Shield, SlidersHorizontal, HardDrive
} from 'lucide-react';
import RecoveryRulesTab from '../components/RecoveryRulesTab';

export default function Admin() {
  const { postgresVersions, postgresVariants, storageTiers, backupStores, refresh } = useData();
  const toast = useToast();

  useEffect(() => { document.title = 'Admin - PG-Swarm'; }, []);
  const [activeTab, setActiveTab] = useState('tiers');

  const tabs = [
    { key: 'tiers', label: 'Storage Tiers', icon: <Layers size={14} /> },
    { key: 'variants', label: 'Image Variants', icon: <Box size={14} /> },
    { key: 'versions', label: 'PG Versions', icon: <Database size={14} /> },
    { key: 'recovery', label: 'Recovery Rules', icon: <Shield size={14} /> },
    { key: 'pgparams', label: 'Update Rules', icon: <SlidersHorizontal size={14} /> },
    { key: 'backupstores', label: 'Backup Stores', icon: <HardDrive size={14} /> },
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
      {activeTab === 'variants' && <VariantsTab postgresVariants={postgresVariants} refresh={refresh} toast={toast} />}
      {activeTab === 'versions' && <VersionsTab postgresVersions={postgresVersions} postgresVariants={postgresVariants} refresh={refresh} toast={toast} />}
      {activeTab === 'recovery' && <RecoveryRulesTab toast={toast} />}
      {activeTab === 'pgparams' && <PgParamClassificationsTab toast={toast} />}
      {activeTab === 'backupstores' && <BackupStoresTab backupStores={backupStores} refresh={refresh} toast={toast} />}
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

// --- PG Parameter Classifications Tab ---

function PgParamClassificationsTab({ toast }) {
  const [params, setParams] = useState([]);
  const [editing, setEditing] = useState(null);
  const [search, setSearch] = useState('');
  const [form, setForm] = useState({ name: '', restart_mode: 'reload', description: '', pg_context: 'sighup' });

  async function load() {
    try {
      const data = await api.pgParamClassifications();
      setParams(data || []);
    } catch (e) {
      toast('Failed to load param classifications: ' + e.message, true);
    }
  }

  useEffect(() => { load(); }, []);

  function startCreate() {
    setForm({ name: '', restart_mode: 'reload', description: '', pg_context: 'sighup' });
    setEditing('new');
  }

  function startEdit(p) {
    setForm({ name: p.name, restart_mode: p.restart_mode, description: p.description, pg_context: p.pg_context });
    setEditing(p.name);
  }

  async function save() {
    try {
      await api.upsertPgParamClassification(form);
      toast(editing === 'new' ? 'Parameter added' : 'Parameter updated');
      setEditing(null);
      load();
    } catch (e) {
      toast('Save failed: ' + e.message, true);
    }
  }

  async function remove(name) {
    try {
      await api.deletePgParamClassification(name);
      toast('Parameter removed (defaults to reload)');
      load();
    } catch (e) {
      toast('Delete failed: ' + e.message, true);
    }
  }

  const modeBadge = (mode) => {
    const styles = {
      reload:       { cls: 'badge-green',  label: 'Reload' },
      sequential:   { cls: 'badge-amber',  label: 'Restart' },
      full_restart: { cls: 'badge-red',    label: 'Full Restart' },
    };
    const s = styles[mode] || styles.reload;
    return <span className={'badge ' + s.cls}><span className="dot" />{s.label}</span>;
  };

  const term = search.toLowerCase().trim();
  const filtered = term
    ? params.filter(p => p.name.includes(term) || (p.description && p.description.toLowerCase().includes(term)) || (p.pg_context && p.pg_context.includes(term)))
    : params;

  return (
    <>
      <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', padding: '12px 20px', gap: 12 }}>
        <span className="sm muted" style={{ flex: 1 }}>
          Update rules for PostgreSQL parameters. <strong>Reload</strong> = pg_reload_conf() with no restart.
          <strong> Restart</strong> = rolling pod restart. <strong>Full Restart</strong> = full cluster shutdown.
          Parameters not listed default to reload.
        </span>
        <input className="input" placeholder="Search parameters..." value={search}
          onChange={e => setSearch(e.target.value)} style={{ width: 220 }} />
        <button className="btn btn-approve" onClick={startCreate} style={{ whiteSpace: 'nowrap' }}><Plus size={14} /> Add</button>
      </div>

      {editing && (
        <div className="admin-form-bar">
          <input className="input" placeholder="Parameter name" value={form.name}
            onChange={e => setForm(f => ({ ...f, name: e.target.value }))}
            disabled={editing !== 'new'} style={{ width: 200 }} />
          <select className="input" value={form.restart_mode}
            onChange={e => setForm(f => ({ ...f, restart_mode: e.target.value }))} style={{ width: 150 }}>
            <option value="reload">Reload</option>
            <option value="sequential">Restart</option>
            <option value="full_restart">Full Restart</option>
          </select>
          <select className="input" value={form.pg_context}
            onChange={e => setForm(f => ({ ...f, pg_context: e.target.value }))} style={{ width: 140 }}>
            <option value="sighup">sighup</option>
            <option value="postmaster">postmaster</option>
            <option value="superuser">superuser</option>
            <option value="user">user</option>
          </select>
          <input className="input" placeholder="Description" value={form.description}
            onChange={e => setForm(f => ({ ...f, description: e.target.value }))} style={{ flex: 1 }} />
          <button className="btn btn-approve" onClick={save}><Save size={13} /> Save</button>
          <button className="btn btn-reject" onClick={() => setEditing(null)}><X size={13} /> Cancel</button>
        </div>
      )}

      {params.length === 0 && !editing ? (
        <div className="empty-state" style={{ padding: '40px 20px' }}>
          <SlidersHorizontal size={48} strokeWidth={1.2} />
          <h3>No parameter classifications</h3>
          <p>All parameters default to reload (pg_reload_conf). Add entries to classify parameters that need a pod restart or full cluster shutdown.</p>
        </div>
      ) : (
        <table>
          <thead>
            <tr>
              <th>Parameter</th>
              <th>Update Mode</th>
              <th>PG Context</th>
              <th>Description</th>
              <th style={{ width: 180 }}>Actions</th>
            </tr>
          </thead>
          <tbody>
            {filtered.map(p => (
              <tr key={p.name}>
                <td className="mono">{p.name}</td>
                <td>{modeBadge(p.restart_mode)}</td>
                <td className="sm muted">{p.pg_context || '-'}</td>
                <td className="sm muted">{p.description || '-'}</td>
                <td>
                  <div className="actions">
                    <button className="btn btn-sm" onClick={() => startEdit(p)}><Pencil size={12} /> Edit</button>
                    <button className="btn btn-sm btn-reject" onClick={() => remove(p.name)}><Trash2 size={12} /></button>
                  </div>
                </td>
              </tr>
            ))}
            {filtered.length === 0 && term && (
              <tr><td colSpan={5} className="sm muted" style={{ textAlign: 'center', padding: 20 }}>No parameters matching "{search}"</td></tr>
            )}
          </tbody>
        </table>
      )}
    </>
  );
}

// --- Backup Stores Tab ---

const emptyBackupStoreForm = () => ({
  name: '', description: '', store_type: 'gcs',
  config: { bucket: '', path_prefix: '' },
  credentials: {},
});

function BackupStoresTab({ backupStores, refresh, toast }) {
  const [editing, setEditing] = useState(null);
  const [form, setForm] = useState(emptyBackupStoreForm());

  function startCreate() {
    setForm(emptyBackupStoreForm());
    setEditing('new');
  }

  function startEdit(bs) {
    const cfg = typeof bs.config === 'string' ? JSON.parse(bs.config) : (bs.config || {});
    setForm({
      name: bs.name,
      description: bs.description,
      store_type: bs.store_type,
      config: bs.store_type === 'gcs'
        ? { bucket: cfg.bucket || '', path_prefix: cfg.path_prefix || '' }
        : { host: cfg.host || '', port: cfg.port || 22, user: cfg.user || '', base_path: cfg.base_path || '' },
      credentials: {},
    });
    setEditing(bs.id);
  }

  function changeType(type) {
    setForm(f => ({
      ...f,
      store_type: type,
      config: type === 'gcs'
        ? { bucket: '', path_prefix: '' }
        : { host: '', port: 22, user: '', base_path: '' },
      credentials: {},
    }));
  }

  async function save() {
    try {
      const payload = {
        name: form.name,
        description: form.description,
        store_type: form.store_type,
        config: form.config,
      };
      // Only include credentials if any field is non-empty
      const creds = form.credentials || {};
      const hasCredentials = Object.values(creds).some(v => v && v.length > 0);
      if (hasCredentials) {
        payload.credentials = creds;
      }

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

  const typeBadge = (type) =>
    type === 'gcs'
      ? <span className="badge badge-blue"><span className="dot" />GCS</span>
      : <span className="badge badge-amber"><span className="dot" />SFTP</span>;

  const credBadges = (bs) => {
    const cs = bs.credentials_set || {};
    const entries = Object.entries(cs);
    if (entries.length === 0) return <span className="sm muted">none</span>;
    return entries.map(([key, set]) => (
      <span key={key} className={'badge ' + (set ? 'badge-green' : 'badge-gray')} style={{ marginRight: 4 }}>
        {key.replace(/_/g, ' ')}
      </span>
    ));
  };

  return (
    <>
      <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', padding: '12px 20px' }}>
        <span className="sm muted">Configure GCS or SFTP destinations for PostgreSQL backups. Credentials are stored encrypted.</span>
        <button className="btn btn-approve" onClick={startCreate}><Plus size={14} /> Add Store</button>
      </div>

      {editing && (
        <div className="admin-form-bar" style={{ flexWrap: 'wrap', gap: 8 }}>
          <input className="input" placeholder="Name" value={form.name}
            onChange={e => setForm(f => ({ ...f, name: e.target.value }))} style={{ width: 160 }} />
          <input className="input" placeholder="Description" value={form.description}
            onChange={e => setForm(f => ({ ...f, description: e.target.value }))} style={{ width: 200 }} />
          <select className="input" value={form.store_type} onChange={e => changeType(e.target.value)}
            disabled={editing !== 'new'} style={{ width: 100 }}>
            <option value="gcs">GCS</option>
            <option value="sftp">SFTP</option>
          </select>

          {form.store_type === 'gcs' ? (
            <>
              <input className="input" placeholder="Bucket" value={form.config.bucket || ''}
                onChange={e => setForm(f => ({ ...f, config: { ...f.config, bucket: e.target.value } }))} style={{ width: 180 }} />
              <input className="input" placeholder="Path prefix" value={form.config.path_prefix || ''}
                onChange={e => setForm(f => ({ ...f, config: { ...f.config, path_prefix: e.target.value } }))} style={{ flex: 1 }} />
              <div style={{ width: '100%', display: 'flex', gap: 8, alignItems: 'start' }}>
                <textarea className="input" placeholder="Service account JSON (paste or upload)" rows={25}
                  value={form.credentials.service_account_json || ''}
                  onChange={e => setForm(f => ({ ...f, credentials: { ...f.credentials, service_account_json: e.target.value } }))}
                  style={{ flex: 1, minHeight: 375, fontFamily: 'monospace', fontSize: 12 }} />
                <label className="btn btn-sm" style={{ cursor: 'pointer', whiteSpace: 'nowrap', marginTop: 2 }}>
                  Upload JSON
                  <input type="file" accept=".json,application/json" hidden
                    onChange={e => {
                      const file = e.target.files?.[0];
                      if (!file) return;
                      const reader = new FileReader();
                      reader.onload = () => setForm(f => ({ ...f, credentials: { ...f.credentials, service_account_json: reader.result } }));
                      reader.readAsText(file);
                      e.target.value = '';
                    }} />
                </label>
              </div>
            </>
          ) : (
            <>
              <input className="input" placeholder="Host" value={form.config.host || ''}
                onChange={e => setForm(f => ({ ...f, config: { ...f.config, host: e.target.value } }))} style={{ width: 160 }} />
              <input className="input" placeholder="Port" type="number" value={form.config.port || 22}
                onChange={e => setForm(f => ({ ...f, config: { ...f.config, port: parseInt(e.target.value) || 22 } }))} style={{ width: 80 }} />
              <input className="input" placeholder="User" value={form.config.user || ''}
                onChange={e => setForm(f => ({ ...f, config: { ...f.config, user: e.target.value } }))} style={{ width: 120 }} />
              <input className="input" placeholder="Base path" value={form.config.base_path || ''}
                onChange={e => setForm(f => ({ ...f, config: { ...f.config, base_path: e.target.value } }))} style={{ flex: 1 }} />
              <input className="input" placeholder="Password (leave empty to keep)" type="password"
                value={form.credentials.password || ''}
                onChange={e => setForm(f => ({ ...f, credentials: { ...f.credentials, password: e.target.value } }))} style={{ width: 180 }} />
              <textarea className="input" placeholder="Private key (PEM)" rows={1}
                value={form.credentials.private_key || ''}
                onChange={e => setForm(f => ({ ...f, credentials: { ...f.credentials, private_key: e.target.value } }))}
                style={{ width: '100%', minHeight: 36, fontFamily: 'monospace', fontSize: 12 }} />
            </>
          )}

          <div style={{ display: 'flex', gap: 6, marginLeft: 'auto' }}>
            <button className="btn btn-approve" onClick={save}><Save size={13} /> Save</button>
            <button className="btn btn-reject" onClick={() => setEditing(null)}><X size={13} /> Cancel</button>
          </div>
        </div>
      )}

      {backupStores.length === 0 && !editing ? (
        <div className="empty-state" style={{ padding: '40px 20px' }}>
          <HardDrive size={48} strokeWidth={1.2} />
          <h3>No backup stores configured</h3>
          <p>Add a GCS bucket or SFTP server to store PostgreSQL backups.</p>
        </div>
      ) : (
        <table>
          <thead>
            <tr>
              <th>Name</th>
              <th>Type</th>
              <th>Description</th>
              <th>Credentials</th>
              <th style={{ width: 180 }}>Actions</th>
            </tr>
          </thead>
          <tbody>
            {backupStores.map(bs => (
              <tr key={bs.id}>
                <td className="mono">{bs.name}</td>
                <td>{typeBadge(bs.store_type)}</td>
                <td className="sm muted">{bs.description || '-'}</td>
                <td>{credBadges(bs)}</td>
                <td>
                  <div className="actions">
                    <button className="btn btn-sm" onClick={() => startEdit(bs)}><Pencil size={11} /> Edit</button>
                    <button className="btn btn-sm btn-reject" onClick={() => remove(bs.id)}><Trash2 size={11} /> Delete</button>
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
