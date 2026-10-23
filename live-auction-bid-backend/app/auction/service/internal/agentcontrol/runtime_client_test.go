package agentcontrol

import (
	"context"
	"net"
	"regexp"
	"strings"
	"testing"
	"time"

	agentv1 "live-auction-bid/backend/api/agent/v1"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

func TestAgentRuntimeClientReplacesCallerMetadataWithExactAuthorizationAndTraceparent(t *testing.T) {
	server := &runtimeCaptureServer{}
	client := newTestRuntimeClient(t, server, RuntimeClientConfig{
		Address: "passthrough:///agent-runtime", Token: "internal-secret", UnaryTimeout: time.Second, StreamTimeout: time.Minute,
	})
	ctx := metadata.NewOutgoingContext(context.Background(), metadata.Pairs(
		"authorization", "Bearer attacker",
		"traceparent", "00-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-bbbbbbbbbbbbbbbb-01",
		"x-provider-key", "must-not-cross",
	))
	traceID := "11111111111111111111111111111111"

	_, err := client.StartRun(ctx, RuntimeStartRequest{
		RunID: "run-1", Goal: "清理库存",
		Context: RuntimeRequestContext{RequestID: "req-1", TraceID: traceID, RunID: "run-1", TenantID: "merchant-a", OperatorID: "user-a"},
	})
	if err != nil {
		t.Fatalf("StartRun() error = %v", err)
	}
	values := server.startMetadata
	if got := values.Get("authorization"); len(got) != 1 || got[0] != "Bearer internal-secret" {
		t.Fatalf("authorization metadata = %q", got)
	}
	if got := values.Get("x-provider-key"); len(got) != 0 {
		t.Fatalf("caller metadata crossed runtime boundary: %q", got)
	}
	traceparents := values.Get("traceparent")
	if len(traceparents) != 1 || !regexp.MustCompile(`^00-11111111111111111111111111111111-[0-9a-f]{16}-01$`).MatchString(traceparents[0]) {
		t.Fatalf("traceparent metadata = %q", traceparents)
	}
	if strings.HasSuffix(traceparents[0], "-0000000000000000-01") {
		t.Fatalf("traceparent uses an invalid zero span: %q", traceparents[0])
	}
	if server.startRequest.GetRunId() != "run-1" || server.startRequest.GetContext().GetRunId() != "run-1" {
		t.Fatalf("StartRun request correlation = %+v", server.startRequest)
	}
}

func TestAgentRuntimeClientSessionTurnContract(t *testing.T) {
	server := &runtimeCaptureServer{}
	client := newTestRuntimeClient(t, server, RuntimeClientConfig{
		Address: "passthrough:///agent-runtime", Token: "internal-secret", UnaryTimeout: time.Second, StreamTimeout: time.Minute,
	})

	run, err := client.StartRun(context.Background(), RuntimeStartRequest{
		RunID: "run-session", Goal: "", RunType: agentv1.RunType_RUN_TYPE_SESSION_PLAN,
		SessionID: "session-1", SessionVersion: 7, TurnInputJSON: `{"message":"draft lots"}`,
		Context: RuntimeRequestContext{TraceID: strings.Repeat("4", 32), RunID: "run-session"},
	})
	if err != nil {
		t.Fatalf("StartRun() error = %v", err)
	}
	if got := server.startRequest; got.GetRunType() != agentv1.RunType_RUN_TYPE_SESSION_PLAN || got.GetSessionId() != "session-1" || got.GetSessionVersion() != 7 || got.GetTurnInputJson() != `{"message":"draft lots"}` {
		t.Fatalf("StartRun request session turn contract = %+v", got)
	}
	if run.RunType != agentv1.RunType_RUN_TYPE_LOT_DRAFT_ASSIST || run.SessionID != "session-1" || run.SessionVersion != 8 || run.TurnResultJSON != `{"candidates":[]}` {
		t.Fatalf("StartRun run session turn contract = %+v", run)
	}
}

func TestAgentRuntimeClientLegacyGoalStartsPlanning(t *testing.T) {
	server := &runtimeCaptureServer{}
	client := newTestRuntimeClient(t, server, RuntimeClientConfig{
		Address: "passthrough:///agent-runtime", Token: "internal-secret", UnaryTimeout: time.Second, StreamTimeout: time.Minute,
	})

	_, err := client.StartRun(context.Background(), RuntimeStartRequest{
		RunID: "run-legacy", Goal: "Plan existing lots",
		Context: RuntimeRequestContext{TraceID: strings.Repeat("5", 32), RunID: "run-legacy"},
	})
	if err != nil {
		t.Fatalf("StartRun() error = %v", err)
	}
	if got := server.startRequest; got.GetGoal() != "Plan existing lots" || got.GetRunType() != agentv1.RunType_RUN_TYPE_UNSPECIFIED || got.GetSessionId() != "" || got.GetSessionVersion() != 0 || got.GetTurnInputJson() != "" {
		t.Fatalf("legacy StartRun request = %+v", got)
	}
}

func TestAgentRuntimeClientAppliesFiniteUnaryDeadline(t *testing.T) {
	server := &runtimeCaptureServer{blockStart: true}
	client := newTestRuntimeClient(t, server, RuntimeClientConfig{
		Address: "passthrough:///agent-runtime", Token: "internal-secret", UnaryTimeout: 25 * time.Millisecond, StreamTimeout: time.Minute,
	})
	started := time.Now()

	_, err := client.StartRun(context.Background(), RuntimeStartRequest{
		RunID: "run-1", Goal: "清理库存", Context: RuntimeRequestContext{TraceID: strings.Repeat("1", 32), RunID: "run-1"},
	})
	if status.Code(err) != codes.DeadlineExceeded {
		t.Fatalf("StartRun() error = %v, want DeadlineExceeded", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("StartRun() deadline took %s", elapsed)
	}
}

func TestAgentRuntimeClientConstructionDoesNotConnect(t *testing.T) {
	started := time.Now()
	client, err := NewGRPCRuntimeClient(RuntimeClientConfig{
		Address: "127.0.0.1:1", Token: "internal-secret", UnaryTimeout: time.Second, StreamTimeout: time.Minute,
	})
	if err != nil {
		t.Fatalf("NewGRPCRuntimeClient() error = %v", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("lazy client construction took %s", elapsed)
	}
	if err := client.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
}

func TestAgentRuntimeClientMapsRunAndDurableStreamAndCloseCancelsRPC(t *testing.T) {
	server := &runtimeCaptureServer{streamEvent: &agentv1.AgentRunEvent{
		RunId: "run-1", Seq: 12, Type: "tool.completed", Status: agentv1.RunStatus_RUN_STATUS_COLLECTING_CONTEXT,
		PayloadJson: `{"tool":"list_lots"}`, CreatedAtUnixMs: 1234,
	}}
	client := newTestRuntimeClient(t, server, RuntimeClientConfig{
		Address: "passthrough:///agent-runtime", Token: "internal-secret", UnaryTimeout: time.Second, StreamTimeout: time.Minute,
	})

	run, err := client.GetRun(context.Background(), RuntimeGetRequest{
		RunID: "run-1", Context: RuntimeRequestContext{TraceID: strings.Repeat("2", 32), RunID: "run-1"},
	})
	if err != nil {
		t.Fatalf("GetRun() error = %v", err)
	}
	if run.RunID != "run-1" || run.Status != StatusPlanning || run.ProposalJSON != `{"summary":"safe"}` {
		t.Fatalf("GetRun() = %+v", run)
	}
	stream, err := client.StreamRunEvents(context.Background(), RuntimeStreamRequest{
		RunID: "run-1", AfterSeq: 11, Context: RuntimeRequestContext{TraceID: strings.Repeat("3", 32), RunID: "run-1"},
	})
	if err != nil {
		t.Fatalf("StreamRunEvents() error = %v", err)
	}
	event, err := stream.Recv()
	if err != nil {
		t.Fatalf("Recv() error = %v", err)
	}
	if event.RunID != "run-1" || event.Seq != 12 || event.Type != "tool.completed" || event.Status != StatusCollectingContext || event.PayloadJSON != `{"tool":"list_lots"}` {
		t.Fatalf("runtime event = %+v", event)
	}
	if server.streamRequest.GetAfterSeq() != 11 {
		t.Fatalf("runtime after_seq = %d, want 11", server.streamRequest.GetAfterSeq())
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	select {
	case <-server.streamCancelled:
	case <-time.After(time.Second):
		t.Fatal("Close() did not cancel the runtime stream RPC")
	}
}

func newTestRuntimeClient(t *testing.T, service agentv1.AgentRuntimeServiceServer, config RuntimeClientConfig) *GRPCRuntimeClient {
	t.Helper()
	listener := bufconn.Listen(1024 * 1024)
	grpcServer := grpc.NewServer()
	agentv1.RegisterAgentRuntimeServiceServer(grpcServer, service)
	go func() { _ = grpcServer.Serve(listener) }()
	t.Cleanup(grpcServer.Stop)

	client, err := NewGRPCRuntimeClient(config, grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) {
		return listener.Dial()
	}))
	if err != nil {
		t.Fatalf("NewGRPCRuntimeClient() error = %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}

type runtimeCaptureServer struct {
	agentv1.UnimplementedAgentRuntimeServiceServer
	startMetadata   metadata.MD
	startRequest    *agentv1.StartRunRequest
	streamRequest   *agentv1.StreamRunEventsRequest
	streamEvent     *agentv1.AgentRunEvent
	streamCancelled chan struct{}
	blockStart      bool
}

func (s *runtimeCaptureServer) StartRun(ctx context.Context, request *agentv1.StartRunRequest) (*agentv1.StartRunReply, error) {
	s.startMetadata, _ = metadata.FromIncomingContext(ctx)
	s.startRequest = request
	if s.blockStart {
		<-ctx.Done()
		return nil, status.Error(codes.DeadlineExceeded, "deadline")
	}
	return &agentv1.StartRunReply{Run: &agentv1.AgentRun{
		RunId: request.GetRunId(), Goal: request.GetGoal(), Status: agentv1.RunStatus_RUN_STATUS_CREATED,
		RunType: agentv1.RunType_RUN_TYPE_LOT_DRAFT_ASSIST, SessionId: request.GetSessionId(),
		SessionVersion: request.GetSessionVersion() + 1, TurnResultJson: `{"candidates":[]}`,
	}}, nil
}

func (s *runtimeCaptureServer) GetRun(context.Context, *agentv1.GetRunRequest) (*agentv1.GetRunReply, error) {
	return &agentv1.GetRunReply{Run: &agentv1.AgentRun{
		RunId: "run-1", Goal: "清理库存", Status: agentv1.RunStatus_RUN_STATUS_PLANNING,
		ProposalJson: `{"summary":"safe"}`, CreatedAtUnixMs: 100, UpdatedAtUnixMs: 200,
	}}, nil
}

func (s *runtimeCaptureServer) StreamRunEvents(request *agentv1.StreamRunEventsRequest, stream grpc.ServerStreamingServer[agentv1.AgentRunEvent]) error {
	s.streamRequest = request
	if s.streamCancelled == nil {
		s.streamCancelled = make(chan struct{})
	}
	if s.streamEvent != nil {
		if err := stream.Send(s.streamEvent); err != nil {
			return err
		}
	}
	<-stream.Context().Done()
	close(s.streamCancelled)
	return stream.Context().Err()
}
