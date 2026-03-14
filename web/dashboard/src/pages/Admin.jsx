import { useState } from 'react';
import { useData } from '../context/DataContext';
import { useToast } from '../context/ToastContext';
import { api } from '../api';
import {
  Plus, Pencil, Trash2, Save, X, Star, Settings
} from 'lucide-react';

export default function Admin() {
  const { postgresVersions, refresh } = useData();
  const toast = useToast();
  const [editing, setEditing] = useState(null);
  const [form, setForm] = useState({ version: '', variant: 'alpine', image_tag: '' });

  function startCreate() {
    setForm({ version: '', variant: 'alpine', image_tag: '' });
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
            onChange={e => setForm(f => ({ ...f, variant: e.target.value }))} style={{ width: 120 }}>
            <option value="alpine">Alpine</option>
            <option value="debian">Debian</option>
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
  );
}
