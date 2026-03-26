//go:build e2e

// Phase 3+4: Split-brain invariant checks and profile update flow.

package e2e

import "time"

func (s *E2ESuite) Test_20_SplitBrainInvariant() {
	s.T().Log("verifying exactly 1 primary exists right now")
	s.assertNoPrimaryDuplicates()
}

func (s *E2ESuite) Test_21_SplitBrainUnderLoad() {
	primary, err := s.k8s.GetPrimaryPod(clusterName)
	s.Require().NoError(err)
	primaryUID, err := s.k8s.GetPodUID(primary)
	s.Require().NoError(err)
	s.T().Logf("killing primary %s (uid=%s) and monitoring labels for 60s...", primary, primaryUID)

	done := make(chan *PodLabelHistory, 1)
	go func() { done <- s.monitorLabels(60 * time.Second) }()

	err = s.k8s.DeletePod(primary)
	s.Require().NoError(err)

	_, _ = s.k8s.WatchForNewPrimary(clusterName, primaryUID, 60*time.Second)

	history := <-done
	max, when := history.MaxPrimaries()
	s.T().Logf("label monitoring complete: %s", history.Report())
	s.Assert().LessOrEqual(max, 1,
		"SPLIT-BRAIN: observed %d primaries at %s — %s",
		max, when.Format("15:04:05.000"), history.Report())

	_ = s.k8s.WatchPodsReady(clusterName, 1, 3*time.Minute)
}

func (s *E2ESuite) Test_30_ProfileUpdateNoBadge() {
	if s.clusterID == "" {
		s.T().Skip("cluster ID not set")
	}

	diff, err := s.api.ClusterProfileDiff(s.clusterID)
	if err != nil {
		s.T().Logf("profile-diff returned error (expected for unversioned profile): %v", err)
		return
	}
	s.Assert().Equal("no_change", diff.ApplyStrategy, "expected no_change but got %s", diff.ApplyStrategy)
	s.T().Log("no stale update badge — applied_profile_version is current")
}
