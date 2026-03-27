//go:build e2e

// Phase 2: Chaos tests — kill pods, delete PGDATA, corrupt files, rapid kills.

package e2e

import "time"

func (s *E2ESuite) Test_10_Chaos_KillPrimaryPod() {
	primary, err := s.k8s.GetPrimaryPod(clusterName)
	s.Require().NoError(err)
	primaryUID, err := s.k8s.GetPodUID(primary)
	s.Require().NoError(err)
	s.T().Logf("killing primary pod: %s (uid=%s)", primary, primaryUID)

	stopMonitor := s.startLabelMonitor(3 * time.Minute)

	err = s.k8s.DeletePod(primary)
	s.Require().NoError(err)

	newPrimary, err := s.k8s.WatchForNewPrimary(clusterName, primaryUID, 90*time.Second)
	s.Require().NoError(err)
	s.T().Logf("new primary: %s", newPrimary)

	err = s.k8s.WatchPodsReady(clusterName, 1, 3*time.Minute)
	s.Require().NoError(err)

	history := stopMonitor()
	max, _ := history.MaxPrimaries()
	s.Assert().LessOrEqual(max, 1, "SPLIT-BRAIN detected: %s", history.Report())
	s.logHealth()
}

func (s *E2ESuite) Test_11_Chaos_DeletePGDATA() {
	primary, err := s.k8s.GetPrimaryPod(clusterName)
	s.Require().NoError(err)
	primaryUID, err := s.k8s.GetPodUID(primary)
	s.Require().NoError(err)
	s.T().Logf("deleting PGDATA on primary: %s (uid=%s)", primary, primaryUID)

	stopMonitor := s.startLabelMonitor(5 * time.Minute)

	err = s.k8s.DeletePGDATA(primary)
	s.Require().NoError(err)

	newPrimary, err := s.k8s.WatchForNewPrimary(clusterName, primaryUID, 2*time.Minute)
	s.Require().NoError(err)
	s.T().Logf("new primary: %s", newPrimary)

	err = s.k8s.WatchPodsReady(clusterName, 1, 5*time.Minute)
	s.Require().NoError(err)

	history := stopMonitor()
	max, _ := history.MaxPrimaries()
	s.Assert().LessOrEqual(max, 1, "SPLIT-BRAIN detected: %s", history.Report())
	s.logHealth()
	s.logEvents(10)
}

func (s *E2ESuite) Test_12_Chaos_StopPostgres() {
	primary, err := s.k8s.GetPrimaryPod(clusterName)
	s.Require().NoError(err)
	s.T().Logf("stopping postgres on primary: %s (pg_ctl stop -m immediate)", primary)

	_ = s.k8s.StopPostgres(primary)

	err = s.k8s.WatchPodsReady(clusterName, 1, 2*time.Minute)
	s.Require().NoError(err)
	s.T().Log("postgres recovered in-place")
}

func (s *E2ESuite) Test_13_Chaos_DeleteReplicaPGDATA() {
	replica, err := s.k8s.GetReplicaPod(clusterName)
	s.Require().NoError(err)
	s.T().Logf("deleting PGDATA on replica: %s", replica)

	err = s.k8s.DeletePGDATA(replica)
	s.Require().NoError(err)

	err = s.k8s.WatchPodsReady(clusterName, 1, 5*time.Minute)
	s.Require().NoError(err)
	s.T().Log("replica recovered via re-basebackup")
	s.assertNoPrimaryDuplicates()
}

func (s *E2ESuite) Test_14_Chaos_DeleteFilenode() {
	replica, err := s.k8s.GetReplicaPod(clusterName)
	s.Require().NoError(err)
	s.T().Logf("deleting pg_filenode.map on replica: %s", replica)

	err = s.k8s.DeleteFile(replica, "/var/lib/postgresql/data/pgdata/global/pg_filenode.map")
	s.Require().NoError(err)

	err = s.k8s.WatchPodsReady(clusterName, 1, 5*time.Minute)
	s.Require().NoError(err)
	s.T().Log("replica recovered after pg_filenode.map deletion")
}

func (s *E2ESuite) Test_15_Chaos_RapidPrimaryKills() {
	for i := 1; i <= 3; i++ {
		primary, err := s.k8s.GetPrimaryPod(clusterName)
		if err != nil {
			s.T().Logf("kill %d/3: no primary found yet, waiting...", i)
			time.Sleep(15 * time.Second)
			continue
		}
		s.T().Logf("kill %d/3: killing %s", i, primary)
		_ = s.k8s.DeletePod(primary)
		time.Sleep(10 * time.Second)
	}

	err := s.k8s.WatchPodsReady(clusterName, 1, 5*time.Minute)
	s.Require().NoError(err)
	s.T().Log("cluster stabilized after 3 rapid kills")
	s.assertNoPrimaryDuplicates()
}
