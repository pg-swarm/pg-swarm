import { createContext, useContext, useState, useEffect, useCallback } from 'react';
import { api } from '../api';

const DataContext = createContext(null);

export function DataProvider({ children }) {
  const [data, setData] = useState({
    satellites: [], clusters: [], health: [], events: [], profiles: [], deploymentRules: [], postgresVersions: [], postgresVariants: [], backupProfiles: [], storageTiers: [],
  });
  const [lastRefresh, setLastRefresh] = useState(null);

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

  useEffect(() => {
    refresh();
    const id = setInterval(refresh, 10000);
    return () => clearInterval(id);
  }, [refresh]);

  return (
    <DataContext.Provider value={{ ...data, lastRefresh, refresh }}>
      {children}
    </DataContext.Provider>
  );
}

export function useData() {
  return useContext(DataContext);
}
