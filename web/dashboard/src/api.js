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
  satellites:  ()   => request('/satellites'),
  clusters:    ()   => request('/clusters'),
  health:      ()   => request('/health'),
  events:      (n)  => request('/events?limit=' + (n || 50)),
  approve:     (id) => request('/satellites/' + id + '/approve', { method: 'POST' }),
  reject:      (id) => request('/satellites/' + id + '/reject',  { method: 'POST' }),
};

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
