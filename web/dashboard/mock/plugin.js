// Vite plugin that intercepts /api/v1/* requests and returns mock data.
// Usage: MOCK=true npm run dev  (or: npm run dev:mock)

import {
  satellites,
  clusters,
  health,
  events,
  profiles,
  deploymentRules,
  postgresVersions,
  postgresVariants,
  backupProfiles,
  storageTiers,
  backups,
  restores,
  generateLogs,
} from './data.js';

function json(res, data, status = 200) {
  res.writeHead(status, { 'Content-Type': 'application/json' });
  res.end(JSON.stringify(data));
}

function body(req) {
  return new Promise((resolve) => {
    let data = '';
    req.on('data', (c) => (data += c));
    req.on('end', () => {
      try { resolve(JSON.parse(data)); } catch { resolve({}); }
    });
  });
}

export default function mockApiPlugin() {
  // Mutable copies so mutations within a session are visible
  const state = {
    satellites: [...satellites],
    profiles: [...profiles],
    deploymentRules: [...deploymentRules],
    postgresVersions: [...postgresVersions],
    postgresVariants: [...postgresVariants],
    backupProfiles: [...backupProfiles],
    storageTiers: [...storageTiers],
  };

  return {
    name: 'mock-api',
    configureServer(server) {
      server.middlewares.use((req, res, next) => {
        if (!req.url.startsWith('/api/v1')) return next();

        const url = new URL(req.url, 'http://localhost');
        const path = url.pathname.replace('/api/v1', '');
        const method = req.method;

        // ---- Satellites ----
        if (path === '/satellites' && method === 'GET') {
          return json(res, state.satellites);
        }
        if (path.match(/^\/satellites\/[^/]+\/approve$/) && method === 'POST') {
          const id = path.split('/')[2];
          const sat = state.satellites.find((s) => s.id === id);
          if (sat) { sat.state = 'connected'; sat.last_heartbeat = new Date().toISOString(); }
          return json(res, sat || { error: 'not found' }, sat ? 200 : 404);
        }
        if (path.match(/^\/satellites\/[^/]+\/reject$/) && method === 'POST') {
          const id = path.split('/')[2];
          state.satellites = state.satellites.filter((s) => s.id !== id);
          return json(res, { ok: true });
        }
        if (path.match(/^\/satellites\/[^/]+\/labels$/) && method === 'PUT') {
          const id = path.split('/')[2];
          const sat = state.satellites.find((s) => s.id === id);
          body(req).then((b) => { if (sat) sat.labels = b.labels || {}; json(res, sat || {}); });
          return;
        }
        if (path.match(/^\/satellites\/[^/]+\/refresh-storage-classes$/) && method === 'POST') {
          return json(res, { ok: true });
        }
        if (path.match(/^\/satellites\/[^/]+\/tier-mappings$/) && method === 'PUT') {
          const id = path.split('/')[2];
          const sat = state.satellites.find((s) => s.id === id);
          body(req).then((b) => { if (sat) sat.tier_mappings = b.tier_mappings || {}; json(res, sat || {}); });
          return;
        }
        if (path.match(/^\/satellites\/[^/]+\/logs$/) && method === 'GET') {
          const id = path.split('/')[2];
          const limit = parseInt(url.searchParams.get('limit') || '200', 10);
          return json(res, generateLogs(id, limit));
        }
        if (path.match(/^\/satellites\/[^/]+\/logs\/stream$/)) {
          const id = path.split('/')[2];
          res.writeHead(200, {
            'Content-Type': 'text/event-stream',
            'Cache-Control': 'no-cache',
            Connection: 'keep-alive',
          });
          const msgs = [
            'Heartbeat sent to central',
            'Config reconciliation complete',
            'Health check completed',
            'StatefulSet orders-db: 3/3 pods ready',
            'WAL archiver status: healthy',
          ];
          const iv = setInterval(() => {
            const entry = {
              timestamp: new Date().toISOString(),
              level: 'info',
              message: msgs[Math.floor(Math.random() * msgs.length)],
              source: 'satellite/' + id.slice(0, 12),
            };
            res.write('data: ' + JSON.stringify(entry) + '\n\n');
          }, 2000);
          req.on('close', () => clearInterval(iv));
          return;
        }
        if (path.match(/^\/satellites\/[^/]+\/log-level$/) && method === 'POST') {
          return json(res, { ok: true });
        }

        // ---- Clusters ----
        if (path === '/clusters' && method === 'GET') {
          return json(res, clusters);
        }
        if (path.match(/^\/clusters\/[^/]+\/pause$/) && method === 'POST') {
          const id = path.split('/')[2];
          const cl = clusters.find((c) => c.id === id);
          if (cl) { cl.paused = true; cl.state = 'paused'; }
          return json(res, cl || {});
        }
        if (path.match(/^\/clusters\/[^/]+\/resume$/) && method === 'POST') {
          const id = path.split('/')[2];
          const cl = clusters.find((c) => c.id === id);
          if (cl) { cl.paused = false; cl.state = 'running'; }
          return json(res, cl || {});
        }
        if (path.match(/^\/clusters\/[^/]+\/switchover$/) && method === 'POST') {
          return json(res, { ok: true, message: 'Switchover initiated' });
        }
        if (path.match(/^\/clusters\/[^/]+\/backups$/) && method === 'GET') {
          const id = path.split('/')[2];
          return json(res, backups.filter((b) => b.cluster_id === id));
        }
        if (path.match(/^\/clusters\/[^/]+\/restore$/) && method === 'POST') {
          return json(res, { restore_id: 'rst-' + Date.now(), status: 'running' });
        }
        if (path.match(/^\/clusters\/[^/]+\/restores$/) && method === 'GET') {
          const id = path.split('/')[2];
          return json(res, restores.filter((r) => r.cluster_id === id));
        }

        // ---- Health ----
        if (path === '/health' && method === 'GET') {
          return json(res, health);
        }

        // ---- Events ----
        if (path === '/events' && method === 'GET') {
          const limit = parseInt(url.searchParams.get('limit') || '50', 10);
          return json(res, events.slice(0, limit));
        }

        // ---- Profiles ----
        if (path === '/profiles' && method === 'GET') {
          return json(res, state.profiles);
        }
        if (path === '/profiles' && method === 'POST') {
          body(req).then((b) => {
            const p = { id: 'prof-' + Date.now(), created_at: new Date().toISOString(), updated_at: new Date().toISOString(), locked: false, ...b };
            state.profiles.push(p);
            json(res, p, 201);
          });
          return;
        }
        if (path.match(/^\/profiles\/[^/]+$/) && method === 'PUT') {
          const id = path.split('/')[2];
          body(req).then((b) => {
            const idx = state.profiles.findIndex((p) => p.id === id);
            if (idx >= 0) { Object.assign(state.profiles[idx], b, { updated_at: new Date().toISOString() }); json(res, state.profiles[idx]); }
            else json(res, { error: 'not found' }, 404);
          });
          return;
        }
        if (path.match(/^\/profiles\/[^/]+$/) && method === 'DELETE') {
          const id = path.split('/')[2];
          state.profiles = state.profiles.filter((p) => p.id !== id);
          return json(res, { ok: true });
        }
        if (path.match(/^\/profiles\/[^/]+\/clone$/) && method === 'POST') {
          const id = path.split('/')[2];
          body(req).then((b) => {
            const src = state.profiles.find((p) => p.id === id);
            if (!src) return json(res, { error: 'not found' }, 404);
            const clone = { ...src, id: 'prof-' + Date.now(), name: b.name || src.name + '-copy', created_at: new Date().toISOString(), updated_at: new Date().toISOString() };
            state.profiles.push(clone);
            json(res, clone, 201);
          });
          return;
        }
        if (path.match(/^\/profiles\/[^/]+\/backup-profiles$/) && method === 'GET') {
          return json(res, state.backupProfiles.slice(0, 1));
        }
        if (path.match(/^\/profiles\/[^/]+\/attach-backup-profile$/) && method === 'POST') {
          return json(res, { ok: true });
        }
        if (path.match(/^\/profiles\/[^/]+\/detach-backup-profile$/) && method === 'POST') {
          return json(res, { ok: true });
        }

        // ---- Deployment Rules ----
        if (path === '/deployment-rules' && method === 'GET') {
          return json(res, state.deploymentRules);
        }
        if (path === '/deployment-rules' && method === 'POST') {
          body(req).then((b) => {
            const r = { id: 'rule-' + Date.now(), created_at: new Date().toISOString(), updated_at: new Date().toISOString(), ...b };
            state.deploymentRules.push(r);
            json(res, r, 201);
          });
          return;
        }
        if (path.match(/^\/deployment-rules\/[^/]+$/) && method === 'GET') {
          const id = path.split('/')[2];
          return json(res, state.deploymentRules.find((r) => r.id === id) || { error: 'not found' });
        }
        if (path.match(/^\/deployment-rules\/[^/]+$/) && method === 'PUT') {
          const id = path.split('/')[2];
          body(req).then((b) => {
            const idx = state.deploymentRules.findIndex((r) => r.id === id);
            if (idx >= 0) { Object.assign(state.deploymentRules[idx], b, { updated_at: new Date().toISOString() }); json(res, state.deploymentRules[idx]); }
            else json(res, { error: 'not found' }, 404);
          });
          return;
        }
        if (path.match(/^\/deployment-rules\/[^/]+$/) && method === 'DELETE') {
          const id = path.split('/')[2];
          state.deploymentRules = state.deploymentRules.filter((r) => r.id !== id);
          return json(res, { ok: true });
        }
        if (path.match(/^\/deployment-rules\/[^/]+\/clusters$/) && method === 'GET') {
          const id = path.split('/')[2];
          return json(res, clusters.filter((c) => c.deployment_rule_id === id));
        }

        // ---- PostgreSQL Versions ----
        if (path === '/postgres-versions' && method === 'GET') {
          return json(res, state.postgresVersions);
        }
        if (path === '/postgres-versions' && method === 'POST') {
          body(req).then((b) => {
            const v = { id: 'pgv-' + Date.now(), is_default: false, ...b };
            state.postgresVersions.push(v);
            json(res, v, 201);
          });
          return;
        }
        if (path.match(/^\/postgres-versions\/[^/]+$/) && method === 'PUT') {
          const id = path.split('/')[2];
          body(req).then((b) => {
            const idx = state.postgresVersions.findIndex((v) => v.id === id);
            if (idx >= 0) { Object.assign(state.postgresVersions[idx], b); json(res, state.postgresVersions[idx]); }
            else json(res, { error: 'not found' }, 404);
          });
          return;
        }
        if (path.match(/^\/postgres-versions\/[^/]+$/) && method === 'DELETE') {
          const id = path.split('/')[2];
          state.postgresVersions = state.postgresVersions.filter((v) => v.id !== id);
          return json(res, { ok: true });
        }
        if (path.match(/^\/postgres-versions\/[^/]+\/default$/) && method === 'POST') {
          const id = path.split('/')[2];
          state.postgresVersions.forEach((v) => { v.is_default = v.id === id; });
          return json(res, { ok: true });
        }

        // ---- PostgreSQL Variants ----
        if (path === '/postgres-variants' && method === 'GET') {
          return json(res, state.postgresVariants);
        }
        if (path === '/postgres-variants' && method === 'POST') {
          body(req).then((b) => {
            const v = { id: 'pgvar-' + Date.now(), ...b };
            state.postgresVariants.push(v);
            json(res, v, 201);
          });
          return;
        }
        if (path.match(/^\/postgres-variants\/[^/]+$/) && method === 'DELETE') {
          const id = path.split('/')[2];
          state.postgresVariants = state.postgresVariants.filter((v) => v.id !== id);
          return json(res, { ok: true });
        }

        // ---- Backup Profiles ----
        if (path === '/backup-profiles' && method === 'GET') {
          return json(res, state.backupProfiles);
        }
        if (path === '/backup-profiles' && method === 'POST') {
          body(req).then((b) => {
            const bp = { id: 'bp-' + Date.now(), created_at: new Date().toISOString(), ...b };
            state.backupProfiles.push(bp);
            json(res, bp, 201);
          });
          return;
        }
        if (path.match(/^\/backup-profiles\/[^/]+$/) && method === 'GET') {
          const id = path.split('/')[2];
          return json(res, state.backupProfiles.find((bp) => bp.id === id) || { error: 'not found' });
        }
        if (path.match(/^\/backup-profiles\/[^/]+$/) && method === 'PUT') {
          const id = path.split('/')[2];
          body(req).then((b) => {
            const idx = state.backupProfiles.findIndex((bp) => bp.id === id);
            if (idx >= 0) { Object.assign(state.backupProfiles[idx], b); json(res, state.backupProfiles[idx]); }
            else json(res, { error: 'not found' }, 404);
          });
          return;
        }
        if (path.match(/^\/backup-profiles\/[^/]+$/) && method === 'DELETE') {
          const id = path.split('/')[2];
          state.backupProfiles = state.backupProfiles.filter((bp) => bp.id !== id);
          return json(res, { ok: true });
        }

        // ---- Backups ----
        if (path.match(/^\/backups\/[^/]+$/) && method === 'GET') {
          const id = path.split('/')[2];
          return json(res, backups.find((b) => b.id === id) || { error: 'not found' });
        }

        // ---- Storage Tiers ----
        if (path === '/storage-tiers' && method === 'GET') {
          return json(res, state.storageTiers);
        }
        if (path === '/storage-tiers' && method === 'POST') {
          body(req).then((b) => {
            const t = { id: 'tier-' + Date.now(), created_at: new Date().toISOString(), updated_at: new Date().toISOString(), ...b };
            state.storageTiers.push(t);
            json(res, t, 201);
          });
          return;
        }
        if (path.match(/^\/storage-tiers\/[^/]+$/) && method === 'PUT') {
          const id = path.split('/')[2];
          body(req).then((b) => {
            const idx = state.storageTiers.findIndex((t) => t.id === id);
            if (idx >= 0) { Object.assign(state.storageTiers[idx], b, { updated_at: new Date().toISOString() }); json(res, state.storageTiers[idx]); }
            else json(res, { error: 'not found' }, 404);
          });
          return;
        }
        if (path.match(/^\/storage-tiers\/[^/]+$/) && method === 'DELETE') {
          const id = path.split('/')[2];
          state.storageTiers = state.storageTiers.filter((t) => t.id !== id);
          return json(res, { ok: true });
        }

        // Fallback
        json(res, { error: 'mock: unhandled route ' + method + ' ' + path }, 404);
      });
    },
  };
}
