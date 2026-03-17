import { createContext, useContext, useState, useEffect, useCallback, useRef } from 'react';
import { api } from '../api';

const DataContext = createContext(null);

const EMPTY = {
  satellites: [], clusters: [], health: [], events: [], profiles: [],
  deploymentRules: [], postgresVersions: [], postgresVariants: [],
  backupProfiles: [], storageTiers: [],
};

function wsUrl() {
  const proto = location.protocol === 'https:' ? 'wss:' : 'ws:';
  return proto + '//' + location.host + '/api/v1/ws';
}

export function DataProvider({ children }) {
  const [data, setData] = useState(EMPTY);
  const [lastRefresh, setLastRefresh] = useState(null);
  const [wsConnected, setWsConnected] = useState(false);
  const wsRef = useRef(null);
  const pollRef = useRef(null);

  // REST polling fallback — same as before.
  const refresh = useCallback(async () => {
    try {
      const safe = (p) => p.catch(() => null);
      const [satellites, clusters, health, events, profiles, deploymentRules, postgresVersions, postgresVariants, backupProfiles, storageTiers] = await Promise.all([
        safe(api.satellites()), safe(api.clusters()), safe(api.health()), safe(api.events(50)), safe(api.profiles()), safe(api.deploymentRules()), safe(api.postgresVersions()), safe(api.postgresVariants()), safe(api.backupProfiles()), safe(api.storageTiers()),
      ]);
      setData({
        satellites:        satellites || [],
        clusters:          clusters || [],
        health:            health || [],
        events:            events || [],
        profiles:          profiles || [],
        deploymentRules:   deploymentRules || [],
        postgresVersions:  postgresVersions || [],
        postgresVariants:  postgresVariants || [],
        backupProfiles:    backupProfiles || [],
        storageTiers:      storageTiers || [],
      });
      setLastRefresh(new Date());
    } catch (err) {
      console.error('refresh failed:', err);
    }
  }, []);

  // Start REST polling.
  const startPolling = useCallback(() => {
    if (pollRef.current) return;
    refresh();
    pollRef.current = setInterval(refresh, 10000);
  }, [refresh]);

  // Stop REST polling.
  const stopPolling = useCallback(() => {
    if (pollRef.current) {
      clearInterval(pollRef.current);
      pollRef.current = null;
    }
  }, []);

  // Apply a full_state message from WebSocket.
  const applyWsState = useCallback((state) => {
    setData({
      satellites:        state.satellites || [],
      clusters:          state.clusters || [],
      health:            state.health || [],
      events:            state.events || [],
      profiles:          state.profiles || [],
      deploymentRules:   state.deploymentRules || [],
      postgresVersions:  state.postgresVersions || [],
      postgresVariants:  state.postgresVariants || [],
      backupProfiles:    state.backupProfiles || [],
      storageTiers:      state.storageTiers || [],
    });
    setLastRefresh(new Date());
  }, []);

  // WebSocket connection with automatic reconnect.
  useEffect(() => {
    let reconnectTimer = null;
    let alive = true;

    function connect() {
      if (!alive) return;
      try {
        const ws = new WebSocket(wsUrl());
        wsRef.current = ws;

        ws.onopen = () => {
          console.log('ws: connected');
          setWsConnected(true);
          stopPolling();
        };

        ws.onmessage = (e) => {
          try {
            const msg = JSON.parse(e.data);
            if (msg.type === 'full_state' && msg.data) {
              applyWsState(msg.data);
            }
          } catch {}
        };

        ws.onclose = () => {
          console.log('ws: disconnected, falling back to REST polling');
          setWsConnected(false);
          wsRef.current = null;
          startPolling();
          if (alive) {
            reconnectTimer = setTimeout(connect, 3000);
          }
        };

        ws.onerror = () => {
          // onclose will fire after this.
          ws.close();
        };
      } catch {
        // WebSocket constructor failed (e.g. bad URL). Stay on REST.
        startPolling();
      }
    }

    connect();

    return () => {
      alive = false;
      clearTimeout(reconnectTimer);
      stopPolling();
      if (wsRef.current) {
        wsRef.current.close();
        wsRef.current = null;
      }
    };
  }, [applyWsState, startPolling, stopPolling]);

  return (
    <DataContext.Provider value={{ ...data, lastRefresh, refresh, wsConnected }}>
      {children}
    </DataContext.Provider>
  );
}

export function useData() {
  return useContext(DataContext);
}
