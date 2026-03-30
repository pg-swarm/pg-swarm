const API = '/api/v1';

async function request(path, opts = {}) {
  const res = await fetch(API + path, {
    headers: { 'Content-Type': 'application/json' },
    ...opts,
  });
  if (!res.ok) {
    const body = await res.json().catch(() => ({}));
    const err = new Error(body.error || res.statusText);
    err.status = res.status;
    err.body = body;
    throw err;
  }
  return res.json();
}

export const api = {
  satellites:  ()       => request('/satellites'),
  clusters:    ()       => request('/clusters'),
  health:      ()       => request('/health'),
  events:      (n)      => request('/events?limit=' + (n || 50)),
  approve:     (id, body, replace) => request('/satellites/' + id + '/approve' + (replace ? '?replace=true' : ''), { method: 'POST', body: JSON.stringify(body) }),
  reject:      (id)     => request('/satellites/' + id + '/reject',  { method: 'POST' }),
  profiles:    ()       => request('/profiles'),
  createProfile: (data) => request('/profiles', { method: 'POST', body: JSON.stringify(data) }),
  updateProfile: (id, data) => request('/profiles/' + id, { method: 'PUT', body: JSON.stringify(data) }),
  deleteProfile: (id)   => request('/profiles/' + id, { method: 'DELETE' }),
  cloneProfile:  (id, name) => request('/profiles/' + id + '/clone', { method: 'POST', body: JSON.stringify({ name }) }),
  updateSatelliteLabels: (id, labels) => request('/satellites/' + id + '/labels', { method: 'PUT', body: JSON.stringify({ labels }) }),
  refreshStorageClasses: (id) => request('/satellites/' + id + '/refresh-storage-classes', { method: 'POST' }),
  updateSatelliteTierMappings: (id, tierMappings) => request('/satellites/' + id + '/tier-mappings', { method: 'PUT', body: JSON.stringify({ tier_mappings: tierMappings }) }),
  storageTiers:        ()           => request('/storage-tiers'),
  createStorageTier:   (data)       => request('/storage-tiers', { method: 'POST', body: JSON.stringify(data) }),
  updateStorageTier:   (id, data)   => request('/storage-tiers/' + id, { method: 'PUT', body: JSON.stringify(data) }),
  deleteStorageTier:   (id)         => request('/storage-tiers/' + id, { method: 'DELETE' }),
  // Event Rule Sets
  eventRuleSets:              ()           => request('/event-rule-sets'),
  createEventRuleSet:         (data)       => request('/event-rule-sets', { method: 'POST', body: JSON.stringify(data) }),
  updateEventRuleSet:         (id, data)   => request('/event-rule-sets/' + id, { method: 'PUT', body: JSON.stringify(data) }),
  deleteEventRuleSet:         (id)         => request('/event-rule-sets/' + id, { method: 'DELETE' }),
  listRuleSetHandlers:        (id)         => request('/event-rule-sets/' + id + '/handlers'),
  addHandlerToRuleSet:        (id, handlerId) => request('/event-rule-sets/' + id + '/handlers', { method: 'POST', body: JSON.stringify({ handler_id: handlerId }) }),
  removeHandlerFromRuleSet:   (id, hid)    => request('/event-rule-sets/' + id + '/handlers/' + hid, { method: 'DELETE' }),
  // Event Rules (global)
  eventRules:                 ()           => request('/event-rules'),
  createEventRule:            (data)       => request('/event-rules', { method: 'POST', body: JSON.stringify(data) }),
  updateEventRule:            (id, data)   => request('/event-rules/' + id, { method: 'PUT', body: JSON.stringify(data) }),
  deleteEventRule:            (id)         => request('/event-rules/' + id, { method: 'DELETE' }),
  // Event Actions (global)
  eventActions:               ()           => request('/event-actions'),
  createEventAction:          (data)       => request('/event-actions', { method: 'POST', body: JSON.stringify(data) }),
  updateEventAction:          (id, data)   => request('/event-actions/' + id, { method: 'PUT', body: JSON.stringify(data) }),
  deleteEventAction:          (id)         => request('/event-actions/' + id, { method: 'DELETE' }),
  // Event Handlers (global)
  eventHandlers:              ()           => request('/event-handlers'),
  createEventHandler:         (data)       => request('/event-handlers', { method: 'POST', body: JSON.stringify(data) }),
  updateEventHandler:         (id, data)   => request('/event-handlers/' + id, { method: 'PUT', body: JSON.stringify(data) }),
  deleteEventHandler:         (id)         => request('/event-handlers/' + id, { method: 'DELETE' }),
  backupStores:           ()           => request('/backup-stores'),
  createBackupStore:      (data)       => request('/backup-stores', { method: 'POST', body: JSON.stringify(data) }),
  updateBackupStore:      (id, data)   => request('/backup-stores/' + id, { method: 'PUT', body: JSON.stringify(data) }),
  deleteBackupStore:      (id)         => request('/backup-stores/' + id, { method: 'DELETE' }),
  deleteCluster: (id) => request('/clusters/' + id, { method: 'DELETE' }),
  cascadePreview: (id) => request('/profiles/' + id + '/cascade-preview'),
  cascadeDeleteProfile: (id) => request('/profiles/' + id + '?cascade=true', { method: 'DELETE' }),
  pauseCluster:  (id) => request('/clusters/' + id + '/pause', { method: 'POST' }),
  resumeCluster: (id) => request('/clusters/' + id + '/resume', { method: 'POST' }),
  switchover:         (id, targetPod, interactive = true) => request('/clusters/' + id + '/switchover', { method: 'POST', body: JSON.stringify({ target_pod: targetPod, interactive }) }),
  switchoverContinue: (id, operationId) => request('/clusters/' + id + '/switchover/continue', { method: 'POST', body: JSON.stringify({ operation_id: operationId }) }),
  switchoverAbort:    (id, operationId) => request('/clusters/' + id + '/switchover/abort', { method: 'POST', body: JSON.stringify({ operation_id: operationId }) }),
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
  clusterDatabases: (id) => request('/clusters/' + id + '/databases'),
  createClusterDatabase: (id, data) => request('/clusters/' + id + '/databases', { method: 'POST', body: JSON.stringify(data) }),
  updateClusterDatabase: (clusterId, dbId, data) => request('/clusters/' + clusterId + '/databases/' + dbId, { method: 'PUT', body: JSON.stringify(data) }),
  deleteClusterDatabase: (clusterId, dbId) => request('/clusters/' + clusterId + '/databases/' + dbId, { method: 'DELETE' }),
  clusterBackups:   (id, limit) => request('/clusters/' + id + '/backups?limit=' + (limit || 50)),
  triggerBackup:    (id, backupType) => request('/clusters/' + id + '/trigger-backup', { method: 'POST', body: JSON.stringify({ backup_type: backupType }) }),
  clusterRestores:  (id, limit) => request('/clusters/' + id + '/restores?limit=' + (limit || 50)),
  triggerRestore:   (id, data) => request('/clusters/' + id + '/restore', { method: 'POST', body: JSON.stringify(data) }),
  setSidecarLogLevel: (id, level) => request('/clusters/' + id + '/sidecar-log-level', { method: 'POST', body: JSON.stringify({ level }) }),
  clusterProfileDiff: (id) => request('/clusters/' + id + '/profile-diff'),
  applyCluster: (id) => request('/clusters/' + id + '/apply', { method: 'POST', body: JSON.stringify({ confirmed: true }) }),
  applyProfile: (id) => request('/profiles/' + id + '/apply', { method: 'POST', body: JSON.stringify({ confirmed: true }) }),
  profileVersions: (id) => request('/profiles/' + id + '/versions'),
  pgParamClassifications: () => request('/pg-param-classifications'),
  upsertPgParamClassification: (data) => request('/pg-param-classifications', { method: 'POST', body: JSON.stringify(data) }),
  deletePgParamClassification: (name) => request('/pg-param-classifications/' + encodeURIComponent(name), { method: 'DELETE' }),
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
  if (sat.state === 'replaced') return 'replaced';
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
