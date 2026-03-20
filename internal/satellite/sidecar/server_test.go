package sidecar

import (
	"context"
	"net"
	"testing"
	"time"

	pgswarmv1 "github.com/pg-swarm/pg-swarm/api/gen/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
)

func startTestServer(t *testing.T, validate TokenValidator) (*Server, string) {
	t.Helper()
	manager := NewSidecarStreamManager()
	srv := NewServer(manager, validate)

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := lis.Addr().String()

	go func() {
		_ = srv.server.Serve(lis)
	}()
	t.Cleanup(func() { srv.Stop() })
	return srv, addr
}

func dialTestServer(t *testing.T, addr, token string) pgswarmv1.SidecarStreamService_ConnectClient {
	t.Helper()
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })

	client := pgswarmv1.NewSidecarStreamServiceClient(conn)
	md := metadata.New(map[string]string{"authorization": token})
	ctx := metadata.NewOutgoingContext(context.Background(), md)
	stream, err := client.Connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return stream
}

func TestServer_ConnectAndRegister(t *testing.T) {
	srv, addr := startTestServer(t, func(token string) bool {
		return token == "test-token"
	})

	stream := dialTestServer(t, addr, "test-token")

	// Send identity
	if err := stream.Send(&pgswarmv1.SidecarMessage{
		Payload: &pgswarmv1.SidecarMessage_Identity{
			Identity: &pgswarmv1.SidecarIdentity{
				PodName:     "mydb-0",
				ClusterName: "mydb",
				Namespace:   "default",
			},
		},
	}); err != nil {
		t.Fatal(err)
	}

	// Give the server a moment to register the stream
	time.Sleep(100 * time.Millisecond)

	// Verify stream is registered
	s := srv.Manager().Get("default", "mydb-0")
	if s == nil {
		t.Fatal("expected sidecar stream to be registered")
	}
	if s.Cluster != "mydb" {
		t.Errorf("expected cluster=mydb, got %s", s.Cluster)
	}
}

func TestServer_AuthReject(t *testing.T) {
	_, addr := startTestServer(t, func(token string) bool {
		return token == "valid"
	})

	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	client := pgswarmv1.NewSidecarStreamServiceClient(conn)
	md := metadata.New(map[string]string{"authorization": "bad-token"})
	ctx := metadata.NewOutgoingContext(context.Background(), md)
	stream, err := client.Connect(ctx)
	if err != nil {
		// Connection may fail immediately
		return
	}

	// Try to receive — should get an auth error
	_, err = stream.Recv()
	if err == nil {
		t.Fatal("expected error for invalid token")
	}
}

func TestStreamManager_SendCommandAndWait(t *testing.T) {
	srv, addr := startTestServer(t, nil)

	stream := dialTestServer(t, addr, "any")

	// Send identity
	if err := stream.Send(&pgswarmv1.SidecarMessage{
		Payload: &pgswarmv1.SidecarMessage_Identity{
			Identity: &pgswarmv1.SidecarIdentity{
				PodName:     "db-0",
				ClusterName: "db",
				Namespace:   "ns1",
			},
		},
	}); err != nil {
		t.Fatal(err)
	}

	time.Sleep(100 * time.Millisecond)

	// In a goroutine, simulate the sidecar responding to commands
	go func() {
		for {
			cmd, err := stream.Recv()
			if err != nil {
				return
			}
			// Echo back a success result
			_ = stream.Send(&pgswarmv1.SidecarMessage{
				Payload: &pgswarmv1.SidecarMessage_CommandResult{
					CommandResult: &pgswarmv1.CommandResult{
						RequestId: cmd.RequestId,
						Success:   true,
					},
				},
			})
		}
	}()

	// Send a command and wait for response
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	result, err := srv.Manager().SendCommandAndWait(ctx, "ns1", "db-0", &pgswarmv1.SidecarCommand{
		Cmd: &pgswarmv1.SidecarCommand_Status{Status: &pgswarmv1.StatusCmd{}},
	})
	if err != nil {
		t.Fatalf("SendCommandAndWait failed: %v", err)
	}
	if !result.Success {
		t.Error("expected success=true")
	}
}

func TestStreamManager_NotConnected(t *testing.T) {
	manager := NewSidecarStreamManager()

	ctx := context.Background()
	_, err := manager.SendCommandAndWait(ctx, "ns", "pod", &pgswarmv1.SidecarCommand{
		Cmd: &pgswarmv1.SidecarCommand_Status{Status: &pgswarmv1.StatusCmd{}},
	})
	if err == nil {
		t.Fatal("expected error for disconnected sidecar")
	}
}
