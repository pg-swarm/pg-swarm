package health

import (
	"context"
	"net"
	"testing"
	"time"

	pgswarmv1 "github.com/pg-swarm/pg-swarm/api/gen/v1"
	"github.com/pg-swarm/pg-swarm/internal/satellite/sidecar"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// fakeSidecarServer implements SidecarStreamServiceServer for testing.
type fakeSidecarServer struct {
	pgswarmv1.UnimplementedSidecarStreamServiceServer
	manager *sidecar.SidecarStreamManager
}

func (f *fakeSidecarServer) Connect(stream grpc.BidiStreamingServer[pgswarmv1.SidecarMessage, pgswarmv1.SidecarCommand]) error {
	msg, err := stream.Recv()
	if err != nil {
		return err
	}
	identity := msg.GetIdentity()
	if identity == nil {
		return nil
	}

	ctx, cancel := context.WithCancel(stream.Context())
	defer cancel()

	ss := sidecar.NewSidecarStream(identity.PodName, identity.ClusterName, identity.Namespace, cancel)

	f.manager.Add(identity.Namespace, identity.PodName, ss)
	defer f.manager.Remove(identity.Namespace, identity.PodName)

	errCh := make(chan error, 1)
	go func() {
		for {
			msg, err := stream.Recv()
			if err != nil {
				errCh <- err
				return
			}
			if cr := msg.GetCommandResult(); cr != nil {
				ss.DeliverResult(cr)
			}
		}
	}()

	for {
		select {
		case cmd := <-ss.SendCh:
			if err := stream.Send(cmd); err != nil {
				return err
			}
		case err := <-errCh:
			return err
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// startFakeSidecarInfra starts a gRPC server and connects fake sidecars.
func startFakeSidecarInfra(t *testing.T, pods []string, ns string, handler func(podName string, cmd *pgswarmv1.SidecarCommand) *pgswarmv1.CommandResult) *sidecar.SidecarStreamManager {
	t.Helper()

	manager := sidecar.NewSidecarStreamManager()
	srv := grpc.NewServer()
	pgswarmv1.RegisterSidecarStreamServiceServer(srv, &fakeSidecarServer{manager: manager})

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(func() { srv.Stop() })

	addr := lis.Addr().String()

	for _, pod := range pods {
		pod := pod
		conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { conn.Close() })

		client := pgswarmv1.NewSidecarStreamServiceClient(conn)
		md := metadata.New(map[string]string{})
		ctx := metadata.NewOutgoingContext(context.Background(), md)
		stream, err := client.Connect(ctx)
		if err != nil {
			t.Fatal(err)
		}

		// Send identity
		if err := stream.Send(&pgswarmv1.SidecarMessage{
			Payload: &pgswarmv1.SidecarMessage_Identity{
				Identity: &pgswarmv1.SidecarIdentity{
					PodName:     pod,
					ClusterName: "testdb",
					Namespace:   ns,
				},
			},
		}); err != nil {
			t.Fatal(err)
		}

		// Read commands and respond
		go func() {
			for {
				cmd, err := stream.Recv()
				if err != nil {
					return
				}
				result := handler(pod, cmd)
				if result == nil {
					continue
				}
				_ = stream.Send(&pgswarmv1.SidecarMessage{
					Payload: &pgswarmv1.SidecarMessage_CommandResult{
						CommandResult: result,
					},
				})
			}
		}()
	}

	// Wait for streams to register
	time.Sleep(200 * time.Millisecond)
	return manager
}

func fakeK8sClient(t *testing.T, ns string) *fake.Clientset {
	t.Helper()
	return fake.NewSimpleClientset(
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "testdb-0",
				Namespace: ns,
				Labels: map[string]string{
					"pg-swarm.io/cluster": "testdb",
					"pg-swarm.io/role":    "primary",
				},
			},
		},
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "testdb-1",
				Namespace: ns,
				Labels: map[string]string{
					"pg-swarm.io/cluster": "testdb",
					"pg-swarm.io/role":    "replica",
				},
			},
		},
	)
}

func TestSwitchover_Success(t *testing.T) {
	ns := "default"

	manager := startFakeSidecarInfra(t, []string{"testdb-0", "testdb-1"}, ns,
		func(podName string, cmd *pgswarmv1.SidecarCommand) *pgswarmv1.CommandResult {
			result := &pgswarmv1.CommandResult{RequestId: cmd.RequestId, Success: true}
			switch cmd.Cmd.(type) {
			case *pgswarmv1.SidecarCommand_Status:
				result.InRecovery = (podName == "testdb-1")
			}
			return result
		},
	)

	client := fakeK8sClient(t, ns)

	req := &pgswarmv1.SwitchoverRequest{
		ClusterName: "testdb",
		Namespace:   ns,
		TargetPod:   "testdb-1",
	}

	result := Switchover(context.Background(), client, req, manager, nil)
	if !result.Success {
		t.Fatalf("expected success, got error: %s", result.ErrorMessage)
	}
}

func TestSwitchover_TargetNotReplica(t *testing.T) {
	ns := "default"
	manager := sidecar.NewSidecarStreamManager()
	client := fakeK8sClient(t, ns)

	req := &pgswarmv1.SwitchoverRequest{
		ClusterName: "testdb",
		Namespace:   ns,
		TargetPod:   "testdb-0", // this is the primary
	}

	result := Switchover(context.Background(), client, req, manager, nil)
	if result.Success {
		t.Fatal("expected failure for non-replica target")
	}
	if result.ErrorMessage == "" {
		t.Fatal("expected error message")
	}
}

func TestSwitchover_SidecarNotConnected(t *testing.T) {
	ns := "default"
	manager := sidecar.NewSidecarStreamManager()
	client := fakeK8sClient(t, ns)

	req := &pgswarmv1.SwitchoverRequest{
		ClusterName: "testdb",
		Namespace:   ns,
		TargetPod:   "testdb-1",
	}

	result := Switchover(context.Background(), client, req, manager, nil)
	if result.Success {
		t.Fatal("expected failure when sidecar not connected")
	}
}

func TestSwitchover_FenceFails_Rollback(t *testing.T) {
	ns := "default"

	manager := startFakeSidecarInfra(t, []string{"testdb-0", "testdb-1"}, ns,
		func(podName string, cmd *pgswarmv1.SidecarCommand) *pgswarmv1.CommandResult {
			result := &pgswarmv1.CommandResult{RequestId: cmd.RequestId, Success: true}
			switch cmd.Cmd.(type) {
			case *pgswarmv1.SidecarCommand_Status:
				result.InRecovery = (podName == "testdb-1")
			case *pgswarmv1.SidecarCommand_Fence:
				result.Success = false
				result.Error = "fence failed"
			}
			return result
		},
	)

	client := fakeK8sClient(t, ns)

	req := &pgswarmv1.SwitchoverRequest{
		ClusterName: "testdb",
		Namespace:   ns,
		TargetPod:   "testdb-1",
	}

	result := Switchover(context.Background(), client, req, manager, nil)
	if result.Success {
		t.Fatal("expected failure when fence fails")
	}
}
