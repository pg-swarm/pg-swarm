//go:build e2e

// Phase 1: Satellite registration, cluster creation, replication verification.

package e2e

import (
	"encoding/json"
	"time"
)

func (s *E2ESuite) Test_01_WaitForSatelliteRegistration() {
	var sat Satellite
	err := WaitForInterval(2*time.Minute, 10*time.Second, "satellite registration", func() bool {
		sats, err := s.api.ListSatellites()
		if err != nil || len(sats) == 0 {
			s.T().Log("  polling: no satellites yet...")
			return false
		}
		sat = sats[0]
		return true
	})
	s.Require().NoError(err)
	s.satelliteID = sat.ID
	s.T().Logf("satellite registered: id=%s state=%s", sat.ID, sat.State)
}

func (s *E2ESuite) Test_02_ApproveSatellite() {
	s.Require().NotEmpty(s.satelliteID, "satellite ID not set")

	sats, err := s.api.ListSatellites()
	s.Require().NoError(err)

	var sat *Satellite
	for i := range sats {
		if sats[i].ID == s.satelliteID {
			sat = &sats[i]
			break
		}
	}
	s.Require().NotNil(sat, "satellite %s not found", s.satelliteID)

	if sat.State == "connected" {
		s.T().Log("satellite already connected, skipping approval")
		return
	}

	s.Require().Equal("pending", sat.State, "unexpected satellite state")

	resp, err := s.api.ApproveSatellite(s.satelliteID, "e2e-minikube")
	s.Require().NoError(err)
	s.Require().NotEmpty(resp.AuthToken)

	err = WaitFor(60*time.Second, "satellite connected", func() bool {
		sats, _ := s.api.ListSatellites()
		for _, sat := range sats {
			if sat.ID == s.satelliteID && sat.State == "connected" {
				return true
			}
		}
		return false
	})
	s.Require().NoError(err)
	s.T().Log("satellite approved and connected")
}

func (s *E2ESuite) Test_03_CreateCluster() {
	profiles, err := s.api.ListProfiles()
	s.Require().NoError(err)
	var profileID string
	for _, p := range profiles {
		if p.Name == profileName {
			profileID = p.ID
		}
	}
	s.Require().NotEmpty(profileID, "profile %q not found", profileName)

	// Check if already exists
	clusters, err := s.api.ListClusters()
	s.Require().NoError(err)
	for _, c := range clusters {
		if c.Name == clusterName {
			s.clusterID = c.ID
			s.T().Logf("cluster %s already exists (id=%s)", clusterName, c.ID)
			return
		}
	}

	// Use the profile's config as the cluster config
	var profileConfig interface{}
	for _, p := range profiles {
		if p.ID == profileID {
			_ = json.Unmarshal(p.Config, &profileConfig)
		}
	}
	s.Require().NotNil(profileConfig, "could not parse profile config")

	cfg, err := s.api.CreateCluster(map[string]interface{}{
		"name":         clusterName,
		"namespace":    clusterNS,
		"satellite_id": s.satelliteID,
		"profile_id":   profileID,
		"config":       profileConfig,
	})
	s.Require().NoError(err)
	s.clusterID = cfg.ID
	s.T().Logf("cluster created: id=%s", cfg.ID)
}

func (s *E2ESuite) Test_04_WaitForClusterRunning() {
	err := s.k8s.WatchPodsReady(clusterName, 1, 5*time.Minute)
	s.Require().NoError(err)
	s.T().Log("all pods Running+Ready via K8s watch")
	s.logHealth()
}

func (s *E2ESuite) Test_05_VerifyReplication() {
	err := WaitFor(2*time.Minute, "health API shows primary+replica ready", func() bool {
		health := s.getHealth()
		if health == nil || len(health.Instances) < 2 {
			return false
		}
		hasPrimary, hasReplica := false, false
		for _, inst := range health.Instances {
			if inst.Role == "primary" && inst.Ready {
				hasPrimary = true
			}
			if inst.Role == "replica" && inst.Ready {
				hasReplica = true
			}
		}
		return hasPrimary && hasReplica
	})
	s.Require().NoError(err)
	s.logHealth()
	s.T().Log("replication verified: primary + replica healthy")
}
