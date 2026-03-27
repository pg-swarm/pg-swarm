package eventbus

import (
	"github.com/google/uuid"
	"google.golang.org/protobuf/types/known/timestamppb"

	pgswarmv1 "github.com/pg-swarm/pg-swarm/api/gen/v1"
)

// NewEvent creates a cluster-level event with the given type, cluster, namespace, and source.
func NewEvent(eventType, clusterName, namespace, source string) *pgswarmv1.Event {
	return &pgswarmv1.Event{
		Id:          uuid.New().String(),
		Type:        eventType,
		ClusterName: clusterName,
		Namespace:   namespace,
		Severity:    "info",
		Source:      source,
		Timestamp:   timestamppb.Now(),
		Data:        make(map[string]string),
	}
}

// NewPodEvent creates a pod-level event.
func NewPodEvent(eventType, clusterName, namespace, podName, source string) *pgswarmv1.Event {
	evt := NewEvent(eventType, clusterName, namespace, source)
	evt.PodName = podName
	return evt
}

// WithData sets a key-value pair on the event's data map and returns the event
// for chaining.
func WithData(evt *pgswarmv1.Event, key, value string) *pgswarmv1.Event {
	if evt.Data == nil {
		evt.Data = make(map[string]string)
	}
	evt.Data[key] = value
	return evt
}

// WithSeverity sets the severity and returns the event for chaining.
func WithSeverity(evt *pgswarmv1.Event, severity string) *pgswarmv1.Event {
	evt.Severity = severity
	return evt
}

// WithOperationID sets the operation ID and returns the event for chaining.
func WithOperationID(evt *pgswarmv1.Event, operationID string) *pgswarmv1.Event {
	evt.OperationId = operationID
	return evt
}
