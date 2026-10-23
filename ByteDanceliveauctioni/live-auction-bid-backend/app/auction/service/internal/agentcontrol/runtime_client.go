package agentcontrol

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	agentv1 "live-auction-bid/backend/api/agent/v1"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
)

const (
	defaultRuntimeUnaryTimeout  = 5 * time.Second
	defaultRuntimeStreamTimeout = 30 * time.Minute
)

var w3cTraceIDPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

type RuntimeClientConfig struct {
	Address       string
	Token         string
	UnaryTimeout  time.Duration
	StreamTimeout time.Duration
}

type GRPCRuntimeClient struct {
	connection    *grpc.ClientConn
	client        agentv1.AgentRuntimeServiceClient
	authorization string
	unaryTimeout  time.Duration
	streamTimeout time.Duration
}

func NewGRPCRuntimeClient(config RuntimeClientConfig, options ...grpc.DialOption) (*GRPCRuntimeClient, error) {
	address := strings.TrimSpace(config.Address)
	token := strings.TrimSpace(config.Token)
	if address == "" {
		return nil, errors.New("agent runtime address is required")
	}
	if token == "" {
		return nil, errors.New("agent runtime token is required")
	}
	if config.UnaryTimeout <= 0 {
		config.UnaryTimeout = defaultRuntimeUnaryTimeout
	}
	if config.StreamTimeout <= 0 {
		config.StreamTimeout = defaultRuntimeStreamTimeout
	}
	dialOptions := []grpc.DialOption{grpc.WithTransportCredentials(insecure.NewCredentials())}
	dialOptions = append(dialOptions, options...)
	connection, err := grpc.NewClient(address, dialOptions...)
	if err != nil {
		return nil, errors.New("create agent runtime client failed")
	}
	return &GRPCRuntimeClient{
		connection: connection, client: agentv1.NewAgentRuntimeServiceClient(connection),
		authorization: "Bearer " + token, unaryTimeout: config.UnaryTimeout, streamTimeout: config.StreamTimeout,
	}, nil
}

func (c *GRPCRuntimeClient) StartRun(ctx context.Context, request RuntimeStartRequest) (RuntimeRun, error) {
	callCtx, cancel, err := c.unaryContext(ctx, request.Context.TraceID)
	if err != nil {
		return RuntimeRun{}, err
	}
	defer cancel()
	reply, err := c.client.StartRun(callCtx, &agentv1.StartRunRequest{
		RunId: request.RunID, Context: runtimeContextProto(request.Context), Goal: request.Goal,
		RunType: request.RunType, SessionId: request.SessionID, SessionVersion: request.SessionVersion,
		TurnInputJson: request.TurnInputJSON,
	})
	if err != nil {
		return RuntimeRun{}, err
	}
	return runtimeRunFromProto(reply.GetRun()), nil
}

func (c *GRPCRuntimeClient) GetRun(ctx context.Context, request RuntimeGetRequest) (RuntimeRun, error) {
	callCtx, cancel, err := c.unaryContext(ctx, request.Context.TraceID)
	if err != nil {
		return RuntimeRun{}, err
	}
	defer cancel()
	reply, err := c.client.GetRun(callCtx, &agentv1.GetRunRequest{
		Context: runtimeContextProto(request.Context), RunId: request.RunID,
	})
	if err != nil {
		return RuntimeRun{}, err
	}
	return runtimeRunFromProto(reply.GetRun()), nil
}

func (c *GRPCRuntimeClient) StreamRunEvents(ctx context.Context, request RuntimeStreamRequest) (EventStream, error) {
	traceparent, err := newTraceparent(request.Context.TraceID)
	if err != nil {
		return nil, err
	}
	callCtx, cancel := context.WithTimeout(ctx, c.streamTimeout)
	callCtx = metadata.NewOutgoingContext(callCtx, metadata.Pairs(
		"authorization", c.authorization,
		"traceparent", traceparent,
	))
	stream, err := c.client.StreamRunEvents(callCtx, &agentv1.StreamRunEventsRequest{
		Context: runtimeContextProto(request.Context), RunId: request.RunID, AfterSeq: request.AfterSeq,
	})
	if err != nil {
		cancel()
		return nil, err
	}
	return &grpcRuntimeEventStream{stream: stream, cancel: cancel}, nil
}

func (c *GRPCRuntimeClient) Close() error {
	if c == nil || c.connection == nil {
		return nil
	}
	return c.connection.Close()
}

func (c *GRPCRuntimeClient) unaryContext(ctx context.Context, traceID string) (context.Context, context.CancelFunc, error) {
	traceparent, err := newTraceparent(traceID)
	if err != nil {
		return nil, nil, err
	}
	callCtx, cancel := context.WithTimeout(ctx, c.unaryTimeout)
	callCtx = metadata.NewOutgoingContext(callCtx, metadata.Pairs(
		"authorization", c.authorization,
		"traceparent", traceparent,
	))
	return callCtx, cancel, nil
}

func runtimeContextProto(value RuntimeRequestContext) *agentv1.RequestContext {
	return &agentv1.RequestContext{
		RequestId: value.RequestID, TraceId: value.TraceID, RunId: value.RunID,
		TenantId: value.TenantID, OperatorId: value.OperatorID,
	}
}

func runtimeRunFromProto(run *agentv1.AgentRun) RuntimeRun {
	if run == nil {
		return RuntimeRun{}
	}
	return RuntimeRun{
		RunID: run.GetRunId(), Goal: run.GetGoal(), Status: runStatusFromProto(run.GetStatus()),
		ProposalJSON: run.GetProposalJson(), ErrorCode: run.GetErrorCode(), ErrorMessage: run.GetErrorMessage(),
		CreatedAtUnixMs: run.GetCreatedAtUnixMs(), UpdatedAtUnixMs: run.GetUpdatedAtUnixMs(),
		RunType: run.GetRunType(), SessionID: run.GetSessionId(), SessionVersion: run.GetSessionVersion(),
		TurnResultJSON: run.GetTurnResultJson(),
	}
}

func runStatusFromProto(status agentv1.RunStatus) RunStatus {
	switch status {
	case agentv1.RunStatus_RUN_STATUS_CREATED:
		return StatusCreated
	case agentv1.RunStatus_RUN_STATUS_PLANNING:
		return StatusPlanning
	case agentv1.RunStatus_RUN_STATUS_COLLECTING_CONTEXT:
		return StatusCollectingContext
	case agentv1.RunStatus_RUN_STATUS_GENERATING_PROPOSAL:
		return StatusGeneratingProposal
	case agentv1.RunStatus_RUN_STATUS_VERIFYING:
		return StatusVerifying
	case agentv1.RunStatus_RUN_STATUS_COMPLETED:
		return StatusCompleted
	case agentv1.RunStatus_RUN_STATUS_FAILED:
		return StatusFailed
	case agentv1.RunStatus_RUN_STATUS_CANCELLED:
		return StatusCancelled
	default:
		return ""
	}
}

func newTraceparent(traceID string) (string, error) {
	traceID = strings.TrimSpace(traceID)
	if !w3cTraceIDPattern.MatchString(traceID) || strings.Trim(traceID, "0") == "" {
		return "", errors.New("invalid agent trace ID")
	}
	for {
		var span [8]byte
		if _, err := rand.Read(span[:]); err != nil {
			return "", fmt.Errorf("generate agent span ID: %w", err)
		}
		if span != [8]byte{} {
			return "00-" + traceID + "-" + hex.EncodeToString(span[:]) + "-01", nil
		}
	}
}

type grpcRuntimeEventStream struct {
	stream agentv1.AgentRuntimeService_StreamRunEventsClient
	cancel context.CancelFunc
}

func (s *grpcRuntimeEventStream) Recv() (RuntimeEvent, error) {
	event, err := s.stream.Recv()
	if err != nil {
		return RuntimeEvent{}, err
	}
	return RuntimeEvent{
		RunID: event.GetRunId(), Seq: event.GetSeq(), Type: event.GetType(),
		Status: runStatusFromProto(event.GetStatus()), PayloadJSON: event.GetPayloadJson(),
		CreatedAtUnixMs: event.GetCreatedAtUnixMs(),
	}, nil
}

func (s *grpcRuntimeEventStream) Close() error {
	if s.cancel != nil {
		s.cancel()
	}
	return nil
}
