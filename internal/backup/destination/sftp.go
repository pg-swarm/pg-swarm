package destination

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"strings"
)

// SFTP stores backups on a remote server via SFTP.
type SFTP struct {
	Host     string
	Port     string
	User     string
	BasePath string
}

func NewSFTPFromEnv() *SFTP {
	port := os.Getenv("SFTP_PORT")
	if port == "" {
		port = "22"
	}
	return &SFTP{
		Host:     os.Getenv("SFTP_HOST"),
		Port:     port,
		User:     os.Getenv("SFTP_USER"),
		BasePath: os.Getenv("SFTP_BASE_PATH"),
	}
}

func (s *SFTP) remotePath(p string) string {
	return path.Join(s.BasePath, p)
}

func (s *SFTP) sftpTarget() string {
	return fmt.Sprintf("%s@%s", s.User, s.Host)
}

func (s *SFTP) Upload(ctx context.Context, remotePath string, r io.Reader) error {
	full := s.remotePath(remotePath)
	dir := path.Dir(full)
	// Create remote directory and upload via sftp batch commands
	batchCmd := fmt.Sprintf("-mkdir %s\nput /dev/stdin %s", dir, full)
	cmd := exec.CommandContext(ctx, "sftp", "-P", s.Port, "-b", "-", s.sftpTarget())
	cmd.Stdin = strings.NewReader(batchCmd)
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func (s *SFTP) Download(ctx context.Context, remotePath string, w io.Writer) error {
	full := s.remotePath(remotePath)
	cmd := exec.CommandContext(ctx, "sftp", "-P", s.Port, fmt.Sprintf("%s:%s", s.sftpTarget(), full))
	cmd.Stdout = w
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func (s *SFTP) List(ctx context.Context, prefix string) ([]string, error) {
	full := s.remotePath(prefix)
	cmd := exec.CommandContext(ctx, "sftp", "-P", s.Port, "-b", "-", s.sftpTarget())
	cmd.Stdin = strings.NewReader(fmt.Sprintf("ls %s", full))
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	var keys []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line != "" {
			keys = append(keys, line)
		}
	}
	return keys, nil
}

func (s *SFTP) Delete(ctx context.Context, remotePath string) error {
	full := s.remotePath(remotePath)
	cmd := exec.CommandContext(ctx, "sftp", "-P", s.Port, "-b", "-", s.sftpTarget())
	cmd.Stdin = strings.NewReader(fmt.Sprintf("rm %s", full))
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func (s *SFTP) Exists(ctx context.Context, remotePath string) (bool, error) {
	full := s.remotePath(remotePath)
	cmd := exec.CommandContext(ctx, "sftp", "-P", s.Port, "-b", "-", s.sftpTarget())
	cmd.Stdin = strings.NewReader(fmt.Sprintf("stat %s", full))
	err := cmd.Run()
	if err != nil {
		return false, nil
	}
	return true, nil
}
