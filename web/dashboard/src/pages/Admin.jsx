import { useState, useEffect } from 'react';
import { useData } from '../context/DataContext';
import { useToast } from '../context/ToastContext';
import { api } from '../api';
import {
  Plus, Pencil, Trash2, Save, X, Star, Settings
} from 'lucide-react';

export default function Admin() {
  const { postgresVersions, postgresVariants, refresh } = useData();
  const toast = useToast();

  useEffect(() => { document.title = 'Admin - pg-swarm'; }, []);
  const [editing, setEditing] = useState(null);
  const [form, setForm] = useState({ version: '', variant: '', image_tag: '' });
  const [newVariant, setNewVariant] = useState({ name: '', description: '' });
  const [showVariantForm, setShowVariantForm] = useState(false);

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

  async function addVariant() {
    if (!newVariant.name.trim()) return;
    try {
      await api.createPostgresVariant({ name: newVariant.name.trim(), description: newVariant.description.trim() });
      toast('Variant added');
      setNewVariant({ name: '', description: '' });
      setShowVariantForm(false);
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
      {/* Variants */}
      <div className="card" style={{ marginBottom: 16 }}>
        <div className="card-head" style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center' }}>
          <span>Image Variants</span>
          <button className="btn btn-approve" onClick={() => setShowVariantForm(true)}><Plus size={14} /> Add Variant</button>
        </div>

        {showVariantForm && (
          <div className="admin-form-bar">
            <input className="input" placeholder="Name (e.g. bookworm)" value={newVariant.name}
              onChange={e => setNewVariant(v => ({ ...v, name: e.target.value }))} style={{ width: 140 }} />
            <input className="input" placeholder="Description (optional)" value={newVariant.description}
              onChange={e => setNewVariant(v => ({ ...v, description: e.target.value }))} style={{ flex: 1 }} />
            <button className="btn btn-approve" onClick={addVariant}><Save size={13} /> Save</button>
            <button className="btn btn-reject" onClick={() => setShowVariantForm(false)}><X size={13} /> Cancel</button>
          </div>
        )}

        {postgresVariants.length === 0 && !showVariantForm ? (
          <div className="empty-state" style={{ padding: '24px 20px' }}>
            <p>No variants configured. Add variants like "alpine" or "debian".</p>
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
      </div>

      {/* Versions */}
      <div className="card">
        <div className="card-head" style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center' }}>
          <span>PostgreSQL Version Registry</span>
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

        <div style={{ padding: '14px 20px', borderTop: '1px solid var(--border)', fontSize: '12.5px', color: 'var(--text-secondary)' }}>
          The default version is pre-selected when creating new profiles. Image is resolved as
          <code style={{ margin: '0 4px', background: 'var(--gray-bg)', padding: '1px 6px', borderRadius: 3 }}>[registry/]postgres:image_tag</code>
          at deployment time.
        </div>
      </div>
    </>
  );
}
