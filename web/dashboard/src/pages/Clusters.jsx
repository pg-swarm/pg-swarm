import { useData } from '../context/DataContext';
import { ClusterBadge } from '../components/Badge';
import { parseSpec, timeAgo } from '../api';

export default function Clusters() {
  const { clusters, satellites, health } = useData();

  if (clusters.length === 0) {
    return <div className="empty">No clusters configured</div>;
  }

  return (
    <div className="cluster-grid">
      {clusters.map(c => {
        const s = parseSpec(c.config);
        const sat = satellites.find(x => x.id === c.satellite_id);
        const h = health.find(x => x.cluster_name === c.name);

        const tags = [];
        if (s.failover?.enabled) tags.push('failover');
        if (s.archive?.mode) tags.push('archive:' + s.archive.mode);
        if (s.databases?.length) tags.push(s.databases.length + ' db' + (s.databases.length > 1 ? 's' : ''));

        return (
          <div className="cl-card" key={c.id}>
            <div className="cl-head">
              <h3>{c.name}</h3>
              <div className="badges">
                {h && <ClusterBadge state={h.state} />}
                <ClusterBadge state={c.state} />
              </div>
            </div>
            <div className="cl-body">
              <dl className="cl-grid">
                <KV label="Namespace" value={c.namespace || 'default'} />
                <KV label="Replicas" value={s.replicas || '-'} />
                <KV label="PostgreSQL" value={s.postgres?.version || '-'} />
                <KV label="Storage" value={s.storage?.size || '-'} />
                <KV label="CPU" value={`${s.resources?.cpu_request || '-'} / ${s.resources?.cpu_limit || '-'}`} />
                <KV label="Memory" value={`${s.resources?.memory_request || '-'} / ${s.resources?.memory_limit || '-'}`} />
              </dl>
              {tags.length > 0 && (
                <div className="cl-tags">
                  {tags.map(t => <span className="tag" key={t}>{t}</span>)}
                </div>
              )}
            </div>
            <div className="cl-foot">
              <span>{sat ? sat.hostname : (c.satellite_id ? c.satellite_id.substring(0, 8) : 'unassigned')}</span>
              <span>v{c.config_version}</span>
              <span>{timeAgo(c.updated_at)}</span>
            </div>
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
