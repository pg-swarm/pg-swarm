package destination

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
)

// S3 stores backups in an S3-compatible bucket using the aws CLI.
type S3 struct {
	Bucket         string
	Region         string
	Endpoint       string
	Prefix         string
	ForcePathStyle bool
}

func NewS3FromEnv() *S3 {
	return &S3{
		Bucket:         os.Getenv("S3_BUCKET"),
		Region:         os.Getenv("S3_REGION"),
		Endpoint:       os.Getenv("S3_ENDPOINT"),
		Prefix:         os.Getenv("S3_PREFIX"),
		ForcePathStyle: os.Getenv("S3_FORCE_PATH_STYLE") == "true",
	}
}

func (s *S3) s3URI(remotePath string) string {
	return fmt.Sprintf("s3://%s/%s%s", s.Bucket, s.Prefix, remotePath)
}

func (s *S3) endpointArgs() []string {
	if s.Endpoint != "" {
		return []string{"--endpoint-url", s.Endpoint}
	}
	return nil
}

func (s *S3) Upload(ctx context.Context, remotePath string, r io.Reader) error {
	args := []string{"s3", "cp", "-", s.s3URI(remotePath)}
	args = append(args, s.endpointArgs()...)
	cmd := exec.CommandContext(ctx, "aws", args...)
	cmd.Stdin = r
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func (s *S3) Download(ctx context.Context, remotePath string, w io.Writer) error {
	args := []string{"s3", "cp", s.s3URI(remotePath), "-"}
	args = append(args, s.endpointArgs()...)
	cmd := exec.CommandContext(ctx, "aws", args...)
	cmd.Stdout = w
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func (s *S3) List(ctx context.Context, prefix string) ([]string, error) {
	uri := fmt.Sprintf("s3://%s/%s%s", s.Bucket, s.Prefix, prefix)
	args := []string{"s3", "ls", uri, "--recursive"}
	args = append(args, s.endpointArgs()...)
	cmd := exec.CommandContext(ctx, "aws", args...)
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	var keys []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == "" {
			continue
		}
		parts := strings.Fields(line)
		if len(parts) >= 4 {
			keys = append(keys, parts[3])
		}
	}
	return keys, nil
}

func (s *S3) Delete(ctx context.Context, remotePath string) error {
	args := []string{"s3", "rm", s.s3URI(remotePath)}
	args = append(args, s.endpointArgs()...)
	cmd := exec.CommandContext(ctx, "aws", args...)
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func (s *S3) Exists(ctx context.Context, remotePath string) (bool, error) {
	args := []string{"s3", "ls", s.s3URI(remotePath)}
	args = append(args, s.endpointArgs()...)
	cmd := exec.CommandContext(ctx, "aws", args...)
	err := cmd.Run()
	if err != nil {
		return false, nil
	}
	return true, nil
}
