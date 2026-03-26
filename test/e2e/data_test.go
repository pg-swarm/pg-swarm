//go:build e2e

// Data integrity tests: create database via API, insert data via RW service,
// verify data survives chaos testing.

package e2e

import (
	"fmt"
	"time"
)

// ---------- Phase 1.5: Data Setup (before chaos) ----------

func (s *E2ESuite) Test_06_CreateDatabaseViaAPI() {
	s.Require().NotEmpty(s.clusterID, "cluster ID not set")

	_, err := s.api.CreateClusterDatabase(s.clusterID, map[string]interface{}{
		"db_name":  testDBName,
		"db_user":  testDBUser,
		"password": testDBPass,
	})
	s.Require().NoError(err)
	s.T().Logf("database %s creation requested via API", testDBName)

	// Wait for database to be created on the cluster
	err = WaitFor(2*time.Minute, "database created", func() bool {
		dbs, err := s.api.ListClusterDatabases(s.clusterID)
		if err != nil {
			return false
		}
		for _, db := range dbs {
			if db.DBName == testDBName && db.Status == "created" {
				return true
			}
		}
		return false
	})
	s.Require().NoError(err)

	// Verify via API
	dbs, err := s.api.ListClusterDatabases(s.clusterID)
	s.Require().NoError(err)
	var found bool
	for _, db := range dbs {
		if db.DBName == testDBName {
			s.T().Logf("database created: name=%s user=%s status=%s", db.DBName, db.DBUser, db.Status)
			s.Assert().Equal("created", db.Status)
			found = true
		}
	}
	s.Assert().True(found, "database %s not found in API response", testDBName)
}

func (s *E2ESuite) Test_07_ConnectAndInsertData() {
	var err error
	s.pg, err = NewPGClient(s.k8s, clusterName, pgPort)
	s.Require().NoError(err, "failed to port-forward to RW service")
	s.T().Log("connected to RW service via port-forward")

	// Create table
	_, err = s.pg.Exec("CREATE TABLE IF NOT EXISTS e2e_test (id serial PRIMARY KEY, value text, created_at timestamptz DEFAULT now())")
	s.Require().NoError(err, "failed to create table")

	// Insert 1000 rows
	_, err = s.pg.Exec("INSERT INTO e2e_test (value) SELECT 'row-' || generate_series(1, 1000)")
	s.Require().NoError(err, "failed to insert data")

	// Verify row count
	count, err := s.pg.Exec("SELECT count(*) FROM e2e_test")
	s.Require().NoError(err)
	s.Assert().Equal("1000", count, "expected 1000 rows after insert")
	s.rowsBefore = 1000
	s.T().Logf("inserted %s rows into e2e_test", count)

	// Show table stats
	stats, err := s.pg.Exec("SELECT pg_size_pretty(pg_total_relation_size('e2e_test')), (SELECT count(*) FROM e2e_test)")
	s.Require().NoError(err)
	s.T().Logf("table stats: %s", stats)

	// Close PG port-forward before chaos (it will break during failover)
	s.pg.Close()
	s.pg = nil
}

// ---------- Phase 5: Data Integrity Verification (after chaos) ----------

func (s *E2ESuite) Test_40_VerifyDataAfterChaos() {
	if s.rowsBefore == 0 {
		s.T().Skip("no data was inserted (Test_07 must pass first)")
	}

	// Wait for cluster to be fully healthy
	err := s.k8s.WatchPodsReady(clusterName, 1, 3*time.Minute)
	s.Require().NoError(err, "cluster not healthy before data verification")

	// Reconnect to RW service
	s.pg, err = NewPGClient(s.k8s, clusterName, pgPort)
	s.Require().NoError(err, "failed to reconnect to RW service after chaos")
	s.T().Log("reconnected to RW service via port-forward")

	// Check row count
	count, err := s.pg.Exec("SELECT count(*) FROM e2e_test")
	s.Require().NoError(err, "failed to query row count after chaos")
	s.Assert().Equal(fmt.Sprintf("%d", s.rowsBefore), count,
		"DATA LOSS: expected %d rows, got %s", s.rowsBefore, count)
	s.T().Logf("row count after chaos: %s (expected %d)", count, s.rowsBefore)

	// Spot-check first and last rows
	first, err := s.pg.Exec("SELECT value FROM e2e_test ORDER BY id LIMIT 1")
	s.Require().NoError(err)
	s.Assert().Equal("row-1", first, "first row mismatch")

	last, err := s.pg.Exec("SELECT value FROM e2e_test ORDER BY id DESC LIMIT 1")
	s.Require().NoError(err)
	s.Assert().Equal("row-1000", last, "last row mismatch")

	// Checksum — deterministic hash of all data
	checksum, err := s.pg.Exec("SELECT md5(string_agg(value, ',' ORDER BY id)) FROM e2e_test")
	s.Require().NoError(err)
	s.T().Logf("data checksum after chaos: %s", checksum)

	// Table stats
	stats, err := s.pg.Exec("SELECT pg_size_pretty(pg_total_relation_size('e2e_test'))")
	s.Require().NoError(err)
	s.T().Logf("table size: %s", stats)

	// Verify replication — read from a replica via kubectl exec
	roCount, err := s.pg.ExecOnReplica("SELECT count(*) FROM e2e_test")
	if err == nil {
		s.Assert().Equal(fmt.Sprintf("%d", s.rowsBefore), roCount,
			"REPLICATION LAG: replica has %s rows, expected %d", roCount, s.rowsBefore)
		s.T().Logf("replica row count: %s (matches primary)", roCount)
	} else {
		s.T().Logf("could not verify replica data: %v", err)
	}

	s.pg.Close()
	s.pg = nil
	s.T().Log("DATA INTEGRITY VERIFIED: all rows survived chaos testing")
}
