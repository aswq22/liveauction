package agentcontrol

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestAgentRuntimeClientLivePythonRuntime(t *testing.T) {
	address := strings.TrimSpace(os.Getenv("AGENT_RUNTIME_LIVE_ADDR"))
	token := strings.TrimSpace(os.Getenv("AGENT_RUNTIME_LIVE_TOKEN"))
	if address == "" || token == "" {
		t.Skip("set AGENT_RUNTIME_LIVE_ADDR and AGENT_RUNTIME_LIVE_TOKEN for the Python runtime smoke test")
	}
	client, err := NewGRPCRuntimeClient(RuntimeClientConfig{
		Address: address, Token: token, UnaryTimeout: 2 * time.Second, StreamTimeout: 10 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewGRPCRuntimeClient() error = %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	traceID := "1234567890abcdef1234567890abcdef"
	runID := fmt.Sprintf("live-run-%d", time.Now().UnixNano())
	requestContext := RuntimeRequestContext{
		RequestID: "live-request-1", TraceID: traceID, RunID: runID,
		TenantID: "merchant-live", OperatorID: "operator-live",
	}

	created, err := client.StartRun(context.Background(), RuntimeStartRequest{
		RunID: runID, Context: requestContext, Goal: "verify Go gateway to Python runtime",
	})
	if err != nil {
		t.Fatalf("StartRun() error = %v", err)
	}
	if created.RunID != runID {
		t.Fatalf("StartRun() run ID = %q", created.RunID)
	}
	stream, err := client.StreamRunEvents(context.Background(), RuntimeStreamRequest{
		Context: requestContext, RunID: runID, AfterSeq: 0,
	})
	if err != nil {
		t.Fatalf("StreamRunEvents() error = %v", err)
	}
	defer stream.Close()
	var sequences []int64
	var eventTypes []string
	for {
		event, recvErr := stream.Recv()
		if errors.Is(recvErr, io.EOF) {
			break
		}
		if recvErr != nil {
			t.Fatalf("Recv() error = %v", recvErr)
		}
		sequences = append(sequences, event.Seq)
		eventTypes = append(eventTypes, event.Type)
	}
	if !reflect.DeepEqual(sequences, []int64{1, 2, 3}) {
		t.Fatalf("durable event sequences = %v, want [1 2 3]", sequences)
	}
	if !reflect.DeepEqual(eventTypes, []string{"run.created", "run.planning", "run.failed"}) {
		t.Fatalf("durable event types = %v", eventTypes)
	}
	current, err := client.GetRun(context.Background(), RuntimeGetRequest{Context: requestContext, RunID: runID})
	if err != nil {
		t.Fatalf("GetRun() error = %v", err)
	}
	if current.Status != StatusFailed || current.ErrorCode != "LIVE_TEST_COMPLETE" {
		t.Fatalf("GetRun() = %+v", current)
	}
}

func TestAgentRuntimeClientLivePythonReconciledRequestID(t *testing.T) {
	address := strings.TrimSpace(os.Getenv("AGENT_RUNTIME_LIVE_ADDR"))
	token := strings.TrimSpace(os.Getenv("AGENT_RUNTIME_LIVE_TOKEN"))
	if address == "" || token == "" {
		t.Skip("set AGENT_RUNTIME_LIVE_ADDR and AGENT_RUNTIME_LIVE_TOKEN for the Python runtime smoke test")
	}
	client, err := NewGRPCRuntimeClient(RuntimeClientConfig{
		Address: address, Token: token, UnaryTimeout: 2 * time.Second, StreamTimeout: 20 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewGRPCRuntimeClient() error = %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	runID := strings.TrimSpace(os.Getenv("AGENT_RUNTIME_LIVE_RUN_ID"))
	if runID == "" {
		runID = fmt.Sprintf("live-reconcile-run-%d", time.Now().UnixNano())
	}
	requestContext := RuntimeRequestContext{
		RequestID: "req-original", TraceID: "1234567890abcdef1234567890abcdef", RunID: runID,
		TenantID: "merchant-live", OperatorID: "operator-live",
	}
	request := RuntimeStartRequest{RunID: runID, Context: requestContext, Goal: "verify reconciled request IDs"}

	created, err := client.StartRun(context.Background(), request)
	if err != nil {
		t.Fatalf("original StartRun() error = %v", err)
	}
	request.Context.RequestID = "req-reconcile"
	replayed, err := client.StartRun(context.Background(), request)
	if err != nil {
		t.Fatalf("reconciled StartRun() error = %v", err)
	}
	if replayed.RunID != created.RunID || replayed.CreatedAtUnixMs != created.CreatedAtUnixMs {
		t.Fatalf("reconciled StartRun() = %+v, want original run %+v", replayed, created)
	}

	stream, err := client.StreamRunEvents(context.Background(), RuntimeStreamRequest{
		Context: request.Context, RunID: runID, AfterSeq: 0,
	})
	if err != nil {
		t.Fatalf("StreamRunEvents() error = %v", err)
	}
	defer stream.Close()
	var eventTypes []string
	var lastSeq int64
	for {
		event, recvErr := stream.Recv()
		if errors.Is(recvErr, io.EOF) {
			break
		}
		if recvErr != nil {
			t.Fatalf("Recv() error = %v", recvErr)
		}
		if event.Seq != lastSeq+1 {
			t.Fatalf("event sequence jumped from %d to %d", lastSeq, event.Seq)
		}
		lastSeq = event.Seq
		eventTypes = append(eventTypes, event.Type)
	}
	if countEventType(eventTypes, "run.created") != 1 || countEventType(eventTypes, "run.planning") != 1 {
		t.Fatalf("reconciled run executed more than once: %v", eventTypes)
	}
	terminalCount := countEventType(eventTypes, "run.completed") + countEventType(eventTypes, "run.failed") + countEventType(eventTypes, "run.cancelled")
	if terminalCount != 1 {
		t.Fatalf("terminal event count = %d; events=%v", terminalCount, eventTypes)
	}
}

func countEventType(events []string, want string) int {
	count := 0
	for _, event := range events {
		if event == want {
			count++
		}
	}
	return count
}
