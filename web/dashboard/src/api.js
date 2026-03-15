const API = '/api/v1';

async function request(path, opts = {}) {
  const res = await fetch(API + path, {
    headers: { 'Content-Type': 'application/json' },
    ...opts,
  });
  if (!res.ok) {
    const body = await res.json().catch(() => ({}));
    throw new Error(body.error || res.statusText);
  }
  return res.json();
}

export const api = {
  satellites:  ()       => request('/satellites'),
  clusters:    ()       => request('/clusters'),
  health:      ()       => request('/health'),
  events:      (n)      => request('/events?limit=' + (n || 50)),
  approve:     (id)     => request('/satellites/' + id + '/approve', { method: 'POST' }),
  reject:      (id)     => request('/satellites/' + id + '/reject',  { method: 'POST' }),
  profiles:    ()       => request('/profiles'),
  createProfile: (data) => request('/profiles', { method: 'POST', body: JSON.stringify(data) }),
  updateProfile: (id, data) => request('/profiles/' + id, { method: 'PUT', body: JSON.stringify(data) }),
  deleteProfile: (id)   => request('/profiles/' + id, { method: 'DELETE' }),
  cloneProfile:  (id, name) => request('/profiles/' + id + '/clone', { method: 'POST', body: JSON.stringify({ name }) }),
  updateSatelliteLabels: (id, labels) => request('/satellites/' + id + '/labels', { method: 'PUT', body: JSON.stringify({ labels }) }),
  refreshStorageClasses: (id) => request('/satellites/' + id + '/refresh-storage-classes', { method: 'POST' }),
  pauseCluster:  (id) => request('/clusters/' + id + '/pause', { method: 'POST' }),
  resumeCluster: (id) => request('/clusters/' + id + '/resume', { method: 'POST' }),
  switchover:    (id, targetPod) => request('/clusters/' + id + '/switchover', { method: 'POST', body: JSON.stringify({ target_pod: targetPod }) }),
  deploymentRules: () => request('/deployment-rules'),
  createDeploymentRule: (data) => request('/deployment-rules', { method: 'POST', body: JSON.stringify(data) }),
  getDeploymentRule: (id) => request('/deployment-rules/' + id),
  updateDeploymentRule: (id, data) => request('/deployment-rules/' + id, { method: 'PUT', body: JSON.stringify(data) }),
  deleteDeploymentRule: (id) => request('/deployment-rules/' + id, { method: 'DELETE' }),
  deploymentRuleClusters: (id) => request('/deployment-rules/' + id + '/clusters'),
  postgresVersions: () => request('/postgres-versions'),
  createPostgresVersion: (data) => request('/postgres-versions', { method: 'POST', body: JSON.stringify(data) }),
  updatePostgresVersion: (id, data) => request('/postgres-versions/' + id, { method: 'PUT', body: JSON.stringify(data) }),
  deletePostgresVersion: (id) => request('/postgres-versions/' + id, { method: 'DELETE' }),
  setDefaultPostgresVersion: (id) => request('/postgres-versions/' + id + '/default', { method: 'POST' }),
  postgresVariants: () => request('/postgres-variants'),
  createPostgresVariant: (data) => request('/postgres-variants', { method: 'POST', body: JSON.stringify(data) }),
  deletePostgresVariant: (id) => request('/postgres-variants/' + id, { method: 'DELETE' }),
  satelliteLogs: (id, limit, level) => request('/satellites/' + id + '/logs?limit=' + (limit || 200) + '&level=' + (level || 'info')),
  setSatelliteLogLevel: (id, level) => request('/satellites/' + id + '/log-level', { method: 'POST', body: JSON.stringify({ level }) }),

  // Backup Rules
  backupRules:        ()           => request('/backup-rules'),
  createBackupRule:   (data)       => request('/backup-rules', { method: 'POST', body: JSON.stringify(data) }),
  getBackupRule:      (id)         => request('/backup-rules/' + id),
  updateBackupRule:   (id, data)   => request('/backup-rules/' + id, { method: 'PUT', body: JSON.stringify(data) }),
  deleteBackupRule:   (id)         => request('/backup-rules/' + id, { method: 'DELETE' }),
  attachBackupRule:   (profileId, backupRuleId) => request('/profiles/' + profileId + '/attach-backup-rule', { method: 'POST', body: JSON.stringify({ backup_rule_id: backupRuleId }) }),
  detachBackupRule:   (profileId, backupRuleId) => request('/profiles/' + profileId + '/detach-backup-rule', { method: 'POST', body: JSON.stringify({ backup_rule_id: backupRuleId }) }),

  // Backup Inventory & Restore
  clusterBackups:     (id)         => request('/clusters/' + id + '/backups'),
  getBackup:          (id)         => request('/backups/' + id),
  initiateRestore:    (clusterId, data) => request('/clusters/' + clusterId + '/restore', { method: 'POST', body: JSON.stringify(data) }),
  clusterRestores:    (id)         => request('/clusters/' + id + '/restores'),
};

export function subscribeSatelliteLogs(satelliteId, onEntry, onError) {
  const es = new EventSource(API + '/satellites/' + satelliteId + '/logs/stream');
  es.onmessage = (e) => { try { onEntry(JSON.parse(e.data)); } catch {} };
  es.onerror = (e) => { if (onError) onError(e); };
  return () => es.close();
}

const HEARTBEAT_TIMEOUT_S = 60;

export function deriveSatState(sat) {
  if (sat.state === 'pending') return 'pending';
  if (!sat.last_heartbeat) return 'offline';
  const age = (Date.now() - new Date(sat.last_heartbeat).getTime()) / 1000;
  return age > HEARTBEAT_TIMEOUT_S ? 'offline' : sat.state;
}

export function timeAgo(ts) {
  if (!ts) return 'never';
  const s = Math.floor((Date.now() - new Date(ts).getTime()) / 1000);
  if (s < 60)    return s + 's ago';
  if (s < 3600)  return Math.floor(s / 60) + 'm ago';
  if (s < 86400) return Math.floor(s / 3600) + 'h ago';
  return Math.floor(s / 86400) + 'd ago';
}

export function parseSpec(config) {
  try {
    return typeof config === 'string' ? JSON.parse(config) : config || {};
  } catch {
    return {};
  }
}
