import { useState, useEffect } from 'react';
import { useData } from '../context/DataContext';
import { useToast } from '../context/ToastContext';
import { api } from '../api';
import {
  Plus, Pencil, Trash2, Save, X, Star, Settings, Layers, Box, Database
} from 'lucide-react';

export default function Admin() {
  const { postgresVersions, postgresVariants, storageTiers, refresh } = useData();
  const toast = useToast();

  useEffect(() => { document.title = 'Admin - PG-Swarm'; }, []);
  const [activeTab, setActiveTab] = useState('tiers');

  const tabs = [
    { key: 'tiers', label: 'Storage Tiers', icon: <Layers size={14} /> },
    { key: 'variants', label: 'Image Variants', icon: <Box size={14} /> },
    { key: 'versions', label: 'PG Versions', icon: <Database size={14} /> },
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
