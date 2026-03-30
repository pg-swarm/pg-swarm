//go:build e2e

package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// CentralClient is a typed REST API client for the central server.
type CentralClient struct {
	BaseURL    string
	HTTPClient *http.Client
}

// NewCentralClient creates a client pointing at the given base URL.
func NewCentralClient(baseURL string) *CentralClient {
	return &CentralClient{
		BaseURL:    baseURL,
		HTTPClient: &http.Client{Timeout: 30 * time.Second},
	}
}

// --- Response types ---

type Satellite struct {
	ID               string            `json:"id"`
	Name             string            `json:"name"`
	Hostname         string            `json:"hostname"`
	K8sClusterName   string            `json:"k8s_cluster_name"`
	Region           string            `json:"region"`
	State            string            `json:"state"`
	Labels           map[string]string `json:"labels"`
	LastHeartbeat    *string           `json:"last_heartbeat"`
	StorageClasses   json.RawMessage   `json:"storage_classes"`
}

type ClusterConfig struct {
	ID                    string          `json:"id"`
	Name                  string          `json:"name"`
	Namespace             string          `json:"namespace"`
	SatelliteID           *string         `json:"satellite_id"`
	ProfileID             *string         `json:"profile_id"`
	Config                json.RawMessage `json:"config"`
	ConfigVersion         int64           `json:"config_version"`
	AppliedProfileVersion int             `json:"applied_profile_version"`
	State                 string          `json:"state"`
	Paused                bool            `json:"paused"`
}

type ClusterHealth struct {
	SatelliteID string          `json:"satellite_id"`
	ClusterName string          `json:"cluster_name"`
	State       string          `json:"state"`
	Instances   []InstanceInfo  `json:"instances"`
}

type InstanceInfo struct {
	PodName        string  `json:"pod_name"`
	Role           string  `json:"role"`
	Ready          bool    `json:"ready"`
	ReplicationLag float64 `json:"replication_lag_bytes"`
}

type Profile struct {
	ID          string          `json:"id"`
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Config      json.RawMessage `json:"config"`
}

type Event struct {
	ID          string `json:"id"`
	SatelliteID string `json:"satellite_id"`
	ClusterName string `json:"cluster_name"`
	Severity    string `json:"severity"`
	Message     string `json:"message"`
	Source      string `json:"source"`
}

type ApproveResponse struct {
	AuthToken            string  `json:"auth_token"`
	ReplacedSatelliteID  *string `json:"replaced_satellite_id"`
}

type DiffResponse struct {
	ApplyStrategy         string `json:"apply_strategy"`
	ClusterName           string `json:"cluster_name"`
	AppliedProfileVersion int    `json:"applied_profile_version"`
	LatestProfileVersion  int    `json:"latest_profile_version"`
}

// --- API methods ---

func (c *CentralClient) ListSatellites() ([]Satellite, error) {
	var result []Satellite
	return result, c.get("/satellites", &result)
}

func (c *CentralClient) ApproveSatellite(id, name string) (*ApproveResponse, error) {
	var result ApproveResponse
	return &result, c.post(fmt.Sprintf("/satellites/%s/approve", id), map[string]string{"name": name}, &result)
}

func (c *CentralClient) ListClusters() ([]ClusterConfig, error) {
	var result []ClusterConfig
	return result, c.get("/clusters", &result)
}

func (c *CentralClient) CreateCluster(body map[string]interface{}) (*ClusterConfig, error) {
	var result ClusterConfig
	return &result, c.postExpect("/clusters", body, http.StatusCreated, &result)
}

func (c *CentralClient) GetCluster(id string) (*ClusterConfig, error) {
	var result ClusterConfig
	return &result, c.get(fmt.Sprintf("/clusters/%s", id), &result)
}

func (c *CentralClient) DeleteCluster(id string) error {
	return c.delete(fmt.Sprintf("/clusters/%s", id))
}

func (c *CentralClient) ClusterProfileDiff(id string) (*DiffResponse, error) {
	var result DiffResponse
	return &result, c.get(fmt.Sprintf("/clusters/%s/profile-diff", id), &result)
}

func (c *CentralClient) ApplyCluster(id string) error {
	return c.post(fmt.Sprintf("/clusters/%s/apply", id), map[string]bool{"confirmed": true}, nil)
}

func (c *CentralClient) ListHealth() ([]ClusterHealth, error) {
	var result []ClusterHealth
	return result, c.get("/health", &result)
}

func (c *CentralClient) ListProfiles() ([]Profile, error) {
	var result []Profile
	return result, c.get("/profiles", &result)
}

func (c *CentralClient) UpdateProfile(id string, body map[string]interface{}) error {
	return c.put(fmt.Sprintf("/profiles/%s", id), body)
}

func (c *CentralClient) ListEvents(limit int) ([]Event, error) {
	var result []Event
	return result, c.get(fmt.Sprintf("/events?limit=%d", limit), &result)
}

type ClusterDatabase struct {
	ID           string   `json:"id"`
	ClusterID    string   `json:"cluster_id"`
	DBName       string   `json:"db_name"`
	DBUser       string   `json:"db_user"`
	Status       string   `json:"status"`
	ErrorMessage string   `json:"error_message,omitempty"`
	AllowedCIDRs []string `json:"allowed_cidrs"`
}

func (c *CentralClient) ListClusterDatabases(clusterID string) ([]ClusterDatabase, error) {
	var result []ClusterDatabase
	return result, c.get(fmt.Sprintf("/clusters/%s/databases", clusterID), &result)
}

func (c *CentralClient) CreateClusterDatabase(clusterID string, body map[string]interface{}) (*ClusterDatabase, error) {
	var result ClusterDatabase
	return &result, c.postExpect(fmt.Sprintf("/clusters/%s/databases", clusterID), body, http.StatusCreated, &result)
}

// --- Backup/Restore types ---

type BackupInventoryItem struct {
	ID          string `json:"id"`
	ClusterName string `json:"cluster_name"`
	BackupType  string `json:"backup_type"`
	Status      string `json:"status"`
	BackupPath  string `json:"backup_path"`
	SizeBytes   int64  `json:"size_bytes"`
	PGVersion   string `json:"pg_version"`
	ErrorMsg    string `json:"error_message"`
	StartedAt   string `json:"started_at"`
}

type TriggerBackupResponse struct {
	Status      string `json:"status"`
	BackupType  string `json:"backup_type"`
	OperationID string `json:"operation_id"`
}

type TriggerRestoreResponse struct {
	Status      string `json:"status"`
	RestoreID   string `json:"restore_id"`
	OperationID string `json:"operation_id"`
}

// --- Backup/Restore API methods ---

func (c *CentralClient) ListBackups(clusterID string, limit int) ([]BackupInventoryItem, error) {
	var result []BackupInventoryItem
	return result, c.get(fmt.Sprintf("/clusters/%s/backups?limit=%d", clusterID, limit), &result)
}

func (c *CentralClient) TriggerBackup(clusterID, backupType string) (*TriggerBackupResponse, error) {
	var result TriggerBackupResponse
	return &result, c.post(fmt.Sprintf("/clusters/%s/trigger-backup", clusterID),
		map[string]string{"backup_type": backupType}, &result)
}

func (c *CentralClient) TriggerRestore(clusterID string, body map[string]interface{}) (*TriggerRestoreResponse, error) {
	var result TriggerRestoreResponse
	return &result, c.post(fmt.Sprintf("/clusters/%s/restore", clusterID), body, &result)
}

type RestoreOperation struct {
	ID             string `json:"id"`
	ClusterName    string `json:"cluster_name"`
	RestoreType    string `json:"restore_type"`
	Status         string `json:"status"`
	TargetDatabase string `json:"target_database"`
	ErrorMessage   string `json:"error_message"`
}

func (c *CentralClient) ListRestoreOperations(clusterID string) ([]RestoreOperation, error) {
	var result []RestoreOperation
	return result, c.get(fmt.Sprintf("/clusters/%s/restores", clusterID), &result)
}

type BackupStore struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	StoreType string `json:"store_type"`
}

func (c *CentralClient) CreateBackupStore(body map[string]interface{}) (*BackupStore, error) {
	var result BackupStore
	return &result, c.postExpect("/backup-stores", body, http.StatusCreated, &result)
}

func (c *CentralClient) UpdateCluster(id string, body map[string]interface{}) error {
	return c.put(fmt.Sprintf("/clusters/%s", id), body)
}

func (c *CentralClient) IsReady() bool {
	resp, err := c.HTTPClient.Get(c.BaseURL + "/api/v1/satellites")
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == 200
}

// --- HTTP helpers ---

func (c *CentralClient) get(path string, out interface{}) error {
	resp, err := c.HTTPClient.Get(c.BaseURL + "/api/v1" + path)
	if err != nil {
		return fmt.Errorf("GET %s: %w", path, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return fmt.Errorf("GET %s: %d: %s", path, resp.StatusCode, body)
	}
	if out != nil {
		return json.Unmarshal(body, out)
	}
	return nil
}

func (c *CentralClient) post(path string, data interface{}, out interface{}) error {
	return c.postExpect(path, data, http.StatusOK, out)
}

func (c *CentralClient) postExpect(path string, data interface{}, expectStatus int, out interface{}) error {
	buf, _ := json.Marshal(data)
	resp, err := c.HTTPClient.Post(c.BaseURL+"/api/v1"+path, "application/json", bytes.NewReader(buf))
	if err != nil {
		return fmt.Errorf("POST %s: %w", path, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != expectStatus {
		return fmt.Errorf("POST %s: expected %d, got %d: %s", path, expectStatus, resp.StatusCode, body)
	}
	if out != nil {
		return json.Unmarshal(body, out)
	}
	return nil
}

func (c *CentralClient) put(path string, data interface{}) error {
	buf, _ := json.Marshal(data)
	req, _ := http.NewRequest(http.MethodPut, c.BaseURL+"/api/v1"+path, bytes.NewReader(buf))
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("PUT %s: %w", path, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return fmt.Errorf("PUT %s: %d: %s", path, resp.StatusCode, body)
	}
	return nil
}

func (c *CentralClient) delete(path string) error {
	req, _ := http.NewRequest(http.MethodDelete, c.BaseURL+"/api/v1"+path, nil)
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("DELETE %s: %w", path, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return fmt.Errorf("DELETE %s: %d: %s", path, resp.StatusCode, body)
	}
	return nil
}
