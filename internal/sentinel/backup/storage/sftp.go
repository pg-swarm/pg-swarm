package storage

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"path"
	"sort"

	pgswarmv1 "github.com/pg-swarm/pg-swarm/api/gen/v1"
	"github.com/pkg/sftp"
	"github.com/rs/zerolog"
	"golang.org/x/crypto/ssh"
)

// sftpBackend implements Backend for SFTP storage.
type sftpBackend struct {
	sshClient  *ssh.Client
	sftpClient *sftp.Client
	basePath   string
	log        zerolog.Logger
}

func newSFTP(dest *pgswarmv1.SFTPDestination, log zerolog.Logger) (*sftpBackend, error) {
	if dest.Host == "" {
		return nil, fmt.Errorf("sftp host is required")
	}
	if dest.User == "" {
		return nil, fmt.Errorf("sftp user is required")
	}
	if dest.Password == "" && dest.PrivateKey == "" {
		return nil, fmt.Errorf("sftp password or private_key is required")
	}

	port := dest.Port
	if port == 0 {
		port = 22
	}

	var authMethods []ssh.AuthMethod

	// Private key takes precedence over password.
	if dest.PrivateKey != "" {
		signer, err := ssh.ParsePrivateKey([]byte(dest.PrivateKey))
		if err != nil {
			return nil, fmt.Errorf("parsing sftp private key: %w", err)
		}
		authMethods = append(authMethods, ssh.PublicKeys(signer))
	}
	if dest.Password != "" {
		authMethods = append(authMethods, ssh.Password(dest.Password))
	}

	sshConfig := &ssh.ClientConfig{
		User:            dest.User,
		Auth:            authMethods,
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
	}

	addr := net.JoinHostPort(dest.Host, fmt.Sprintf("%d", port))
	sshClient, err := ssh.Dial("tcp", addr, sshConfig)
	if err != nil {
		return nil, fmt.Errorf("ssh dial %s: %w", addr, err)
	}

	sftpClient, err := sftp.NewClient(sshClient)
	if err != nil {
		sshClient.Close()
		return nil, fmt.Errorf("creating sftp client: %w", err)
	}

	return &sftpBackend{
		sshClient:  sshClient,
		sftpClient: sftpClient,
		basePath:   dest.BasePath,
		log:        log.With().Str("backend", "sftp").Str("host", dest.Host).Logger(),
	}, nil
}

// fullPath joins the configured base path with the relative key.
func (s *sftpBackend) fullPath(key string) string {
	return path.Join(s.basePath, key)
}

func (s *sftpBackend) Upload(_ context.Context, key string, r io.Reader) error {
	fp := s.fullPath(key)

	// Ensure parent directory exists.
	dir := path.Dir(fp)
	if err := s.sftpClient.MkdirAll(dir); err != nil {
		return fmt.Errorf("creating directory %s: %w", dir, err)
	}

	f, err := s.sftpClient.Create(fp)
	if err != nil {
		return fmt.Errorf("creating file %s: %w", fp, err)
	}
	defer f.Close()

	if _, err := io.Copy(f, r); err != nil {
		return fmt.Errorf("uploading %s: %w", key, err)
	}

	s.log.Debug().Str("key", key).Msg("uploaded file")
	return nil
}

func (s *sftpBackend) Download(_ context.Context, key string) (io.ReadCloser, error) {
	fp := s.fullPath(key)
	f, err := s.sftpClient.Open(fp)
	if err != nil {
		return nil, fmt.Errorf("downloading %s: %w", key, err)
	}
	return f, nil
}

func (s *sftpBackend) List(_ context.Context, prefix string) ([]ObjectInfo, error) {
	dir := s.fullPath(prefix)

	// Walk the directory tree under the prefix.
	walker := s.sftpClient.Walk(dir)
	var objects []ObjectInfo
	for walker.Step() {
		if err := walker.Err(); err != nil {
			continue
		}
		info := walker.Stat()
		if info.IsDir() {
			continue
		}
		objects = append(objects, ObjectInfo{
			Key:          walker.Path(),
			Size:         info.Size(),
			LastModified: info.ModTime(),
		})
	}

	sort.Slice(objects, func(i, j int) bool {
		return objects[i].Key < objects[j].Key
	})
	return objects, nil
}

func (s *sftpBackend) Delete(_ context.Context, key string) error {
	fp := s.fullPath(key)
	err := s.sftpClient.Remove(fp)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("deleting %s: %w", key, err)
	}
	s.log.Debug().Str("key", key).Msg("deleted file")
	return nil
}

func (s *sftpBackend) Stat(_ context.Context, key string) (*ObjectInfo, error) {
	fp := s.fullPath(key)
	info, err := s.sftpClient.Stat(fp)
	if err != nil {
		return nil, fmt.Errorf("stat %s: %w", key, err)
	}
	return &ObjectInfo{
		Key:          fp,
		Size:         info.Size(),
		LastModified: info.ModTime(),
	}, nil
}

func (s *sftpBackend) Close() error {
	s.sftpClient.Close()
	return s.sshClient.Close()
}
