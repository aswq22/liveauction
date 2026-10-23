package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	agentv1 "live-auction-bid/backend/api/agent/v1"
	"live-auction-bid/backend/app/auction/service/internal/agentcontrol"
	userbiz "live-auction-bid/backend/app/auction/service/internal/biz/user"
	"live-auction-bid/backend/app/auction/service/internal/pkg/apperr"
	"live-auction-bid/backend/app/auction/service/internal/pkg/auth"
	appsvc "live-auction-bid/backend/app/auction/service/internal/service"

	httptransport "github.com/go-kratos/kratos/v2/transport/http"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	agentRunsPath          = "/api/admin/agent/runs"
	maxAgentRequestBytes   = 64 * 1024
	defaultAgentHeartbeat  = 15 * time.Second
	safeStreamErrorCode    = "AGENT_EVENT_STREAM_UNAVAILABLE"
	safeStreamErrorMessage = "Agent event stream is temporarily unavailable."
)

type AgentHTTPOptions struct {
	HeartbeatInterval time.Duration
}

type agentHTTPHandler struct {
	service   *agentcontrol.Service
	auth      *auth.Manager
	heartbeat time.Duration
}

func NewAgentHTTPHandler(service *agentcontrol.Service, authManager *auth.Manager, options AgentHTTPOptions) http.Handler {
	heartbeat := options.HeartbeatInterval
	if heartbeat <= 0 {
		heartbeat = defaultAgentHeartbeat
	}
	return &agentHTTPHandler{service: service, auth: authManager, heartbeat: heartbeat}
}

func registerAgentHTTP(server *httptransport.Server, handler http.Handler) {
	if server == nil || handler == nil {
		return
	}
	server.HandlePrefix(agentRunsPath, handler)
}

func (h *agentHTTPHandler) ServeHTTP(w http.ResponseWriter, request *http.Request) {
	path := request.URL.Path
	if path == agentRunsPath {
		if request.Method != http.MethodPost {
			methodNotAllowed(w, http.MethodPost)
			return
		}
		h.createRun(w, request)
		return
	}
	if !strings.HasPrefix(path, agentRunsPath+"/") {
		http.NotFound(w, request)
		return
	}
	parts := strings.Split(strings.TrimPrefix(path, agentRunsPath+"/"), "/")
	switch {
	case len(parts) == 1 && parts[0] != "":
		if request.Method != http.MethodGet {
			methodNotAllowed(w, http.MethodGet)
			return
		}
		h.getRun(w, request, parts[0])
	case len(parts) == 2 && parts[0] != "" && parts[1] == "events":
		if request.Method != http.MethodGet {
			methodNotAllowed(w, http.MethodGet)
			return
		}
		h.streamEvents(w, request, parts[0])
	default:
		http.NotFound(w, request)
	}
}

func (h *agentHTTPHandler) createRun(w http.ResponseWriter, request *http.Request) {
	ctx, actor, ok := h.authorizedActor(w, request)
	if !ok {
		return
	}
	idempotencyKey := request.Header.Get("Idempotency-Key")
	if !validAgentIdempotencyKey(idempotencyKey) {
		writeAgentJSONResult(w, http.StatusBadRequest, appsvc.ErrorResult(ctx, fmt.Errorf("%w: invalid idempotency key", apperr.ErrInvalidArgument)), nil)
		return
	}
	request.Body = http.MaxBytesReader(w, request.Body, maxAgentRequestBytes)
	decoder := json.NewDecoder(request.Body)
	var body struct {
		Goal string `json:"goal"`
	}
	if err := decoder.Decode(&body); err != nil {
		h.writeInvalid(w, ctx, "invalid request body")
		return
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		h.writeInvalid(w, ctx, "request body must contain one JSON document")
		return
	}
	run, err := h.service.CreateRunIdempotent(ctx, actor, idempotencyKey, body.Goal)
	if err != nil {
		if failed, ok := agentcontrol.AdmissionRun(err); ok {
			writeAgentJSONResult(w, http.StatusTooManyRequests, appsvc.ResourceExhaustedResult(ctx, err.Error()), map[string]any{"run": agentRunHTTPResponse(failed)})
			return
		}
		if errors.Is(err, apperr.ErrConflict) {
			writeAgentJSONResult(w, http.StatusConflict, appsvc.ErrorResult(ctx, err), nil)
			return
		}
		writeJSONResult(w, appsvc.ErrorResult(ctx, err), nil)
		return
	}
	writeJSONResult(w, appsvc.OKResult(ctx), map[string]any{"run": agentRunHTTPResponse(run)})
}

func validAgentIdempotencyKey(value string) bool {
	if len(value) == 0 || len(value) > 128 {
		return false
	}
	for _, char := range value {
		if (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') || (char >= '0' && char <= '9') || char == '-' || char == '_' || char == '.' || char == '~' {
			continue
		}
		return false
	}
	return true
}

func writeAgentJSONResult(w http.ResponseWriter, statusCode int, result any, extra map[string]any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	body := map[string]any{"result": result}
	for key, value := range extra {
		body[key] = value
	}
	_ = json.NewEncoder(w).Encode(body)
}

func (h *agentHTTPHandler) getRun(w http.ResponseWriter, request *http.Request, runID string) {
	ctx, actor, ok := h.authorizedActor(w, request)
	if !ok {
		return
	}
	run, err := h.service.GetRun(ctx, actor, runID)
	if err != nil {
		writeJSONResult(w, appsvc.ErrorResult(ctx, err), nil)
		return
	}
	writeJSONResult(w, appsvc.OKResult(ctx), map[string]any{"run": agentRunHTTPResponse(run)})
}

func (h *agentHTTPHandler) streamEvents(w http.ResponseWriter, request *http.Request, runID string) {
	ctx, actor, ok := h.authorizedActor(w, request)
	if !ok {
		return
	}
	if err := h.service.AuthorizeRun(ctx, actor, runID); err != nil {
		writeJSONResult(w, appsvc.ErrorResult(ctx, err), nil)
		return
	}
	afterSeq, err := parseAfterSeq(request)
	if err != nil {
		writeJSONResult(w, appsvc.ErrorResult(ctx, err), nil)
		return
	}
	streamCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	stream, err := h.service.StreamRunEvents(streamCtx, actor, runID, afterSeq)
	if err != nil {
		writeJSONResult(w, appsvc.ErrorResult(ctx, err), nil)
		return
	}
	if stream == nil {
		writeJSONResult(w, appsvc.ErrorResult(ctx, errors.New("agent event stream unavailable")), nil)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		_ = stream.Close()
		writeJSONResult(w, appsvc.ErrorResult(ctx, errors.New("streaming response unsupported")), nil)
		return
	}
	header := w.Header()
	header.Set("Content-Type", "text/event-stream")
	header.Set("Cache-Control", "no-cache, no-store")
	header.Set("Connection", "keep-alive")
	header.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	type receiveResult struct {
		event agentcontrol.RuntimeEvent
		err   error
	}
	received := make(chan receiveResult, 1)
	receiverDone := make(chan struct{})
	go func() {
		defer close(receiverDone)
		for {
			event, recvErr := stream.Recv()
			select {
			case received <- receiveResult{event: event, err: recvErr}:
			case <-streamCtx.Done():
				return
			}
			if recvErr != nil {
				return
			}
		}
	}()
	defer func() {
		cancel()
		_ = stream.Close()
		<-receiverDone
	}()

	heartbeat := time.NewTicker(h.heartbeat)
	defer heartbeat.Stop()
	lastSeq := afterSeq
	for {
		select {
		case <-streamCtx.Done():
			return
		case <-heartbeat.C:
			if _, err := io.WriteString(w, ": heartbeat\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case result := <-received:
			if result.err != nil {
				if errors.Is(result.err, io.EOF) || errors.Is(result.err, context.Canceled) || streamCtx.Err() != nil {
					return
				}
				writeSafeSSEError(w, flusher, result.err)
				return
			}
			if result.event.RunID != runID || result.event.Seq <= lastSeq {
				writeSafeSSEError(w, flusher, status.Error(codes.InvalidArgument, "invalid event protocol"))
				return
			}
			if result.event.Seq-lastSeq != 1 {
				writeSafeSSEError(w, flusher, status.Error(codes.Unavailable, "event sequence handoff"))
				return
			}
			if err := writeAgentSSEEvent(w, result.event); err != nil {
				return
			}
			lastSeq = result.event.Seq
			flusher.Flush()
		}
	}
}

func (h *agentHTTPHandler) authorizedActor(w http.ResponseWriter, request *http.Request) (context.Context, agentcontrol.Actor, bool) {
	ctx := request.Context()
	if h.auth != nil {
		ctx = h.auth.WithAuthContextFromBearer(ctx, request.Header.Get("Authorization"))
	}
	claims, err := auth.RequirePermission(ctx, userbiz.PermissionLotViewAdmin)
	if err != nil {
		writeJSONResult(w, appsvc.ErrorResult(ctx, err), nil)
		return ctx, agentcontrol.Actor{}, false
	}
	tenantID := auth.EffectiveMainAccountID(claims)
	operatorID := strings.TrimSpace(claims.UserID)
	if tenantID == "" || operatorID == "" {
		writeJSONResult(w, appsvc.ErrorResult(ctx, apperr.ErrPermissionDenied), nil)
		return ctx, agentcontrol.Actor{}, false
	}
	return ctx, agentcontrol.Actor{TenantID: tenantID, OperatorID: operatorID}, true
}

func (h *agentHTTPHandler) writeInvalid(w http.ResponseWriter, ctx context.Context, message string) {
	writeJSONResult(w, appsvc.ErrorResult(ctx, fmt.Errorf("%w: %s", apperr.ErrInvalidArgument, message)), nil)
}

func parseAfterSeq(request *http.Request) (int64, error) {
	values, exists := request.URL.Query()["after_seq"]
	if !exists {
		return 0, nil
	}
	if len(values) != 1 {
		return 0, fmt.Errorf("%w: after_seq must appear once", apperr.ErrInvalidArgument)
	}
	raw := strings.TrimSpace(values[0])
	if raw == "" {
		return 0, fmt.Errorf("%w: after_seq must be nonnegative", apperr.ErrInvalidArgument)
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || value < 0 {
		return 0, fmt.Errorf("%w: after_seq must be nonnegative", apperr.ErrInvalidArgument)
	}
	return value, nil
}

func methodNotAllowed(w http.ResponseWriter, allowed string) {
	w.Header().Set("Allow", allowed)
	w.WriteHeader(http.StatusMethodNotAllowed)
}

type agentRunResponse struct {
	RunID           string                   `json:"runId"`
	RunType         string                   `json:"runType"`
	Goal            string                   `json:"goal"`
	Status          string                   `json:"status"`
	Proposal        *sessionProposalResponse `json:"proposal,omitempty"`
	ErrorCode       string                   `json:"errorCode,omitempty"`
	ErrorMessage    string                   `json:"errorMessage,omitempty"`
	CreatedAtUnixMs int64                    `json:"createdAtUnixMs"`
	UpdatedAtUnixMs int64                    `json:"updatedAtUnixMs"`
}

func agentRunHTTPResponse(run agentcontrol.Run) agentRunResponse {
	errorCode, errorMessage := safeBrowserRunError(run)
	return agentRunResponse{
		RunID: run.ID, RunType: browserRunType(run.RunType), Goal: run.Goal, Status: string(run.Status),
		Proposal: safeSessionProposal(run.ProposalJSON), ErrorCode: errorCode, ErrorMessage: errorMessage,
		CreatedAtUnixMs: run.CreatedAtUnixMs, UpdatedAtUnixMs: run.UpdatedAtUnixMs,
	}
}

func browserRunType(runType agentv1.RunType) string {
	switch runType {
	case agentv1.RunType_RUN_TYPE_SESSION_PLAN:
		return "SESSION_PLAN"
	case agentv1.RunType_RUN_TYPE_LOT_DRAFT_ASSIST:
		return "LOT_DRAFT_ASSIST"
	default:
		return ""
	}
}

func safeBrowserRunError(run agentcontrol.Run) (string, string) {
	switch run.Status {
	case agentcontrol.StatusFailed:
		code := safeBrowserErrorCode(run.ErrorCode)
		if code == agentcontrol.ErrorCodeRuntimeUnavailable {
			return code, agentcontrol.RuntimeUnavailableMessage
		}
		if code == agentcontrol.ErrorCodeNoEligibleInventory {
			return code, agentcontrol.NoEligibleInventoryMessage
		}
		if code == agentcontrol.ErrorCodeUngroundedProposal {
			return code, agentcontrol.UngroundedProposalMessage
		}
		return code, "Agent run failed. Please try again."
	case agentcontrol.StatusCancelled:
		return agentcontrol.ErrorCodeRunCancelled, "Agent run cancelled."
	default:
		return "", ""
	}
}

func safeBrowserErrorCode(value string) string {
	switch strings.TrimSpace(value) {
	case agentcontrol.ErrorCodeRuntimeUnavailable:
		return agentcontrol.ErrorCodeRuntimeUnavailable
	case agentcontrol.ErrorCodeRuntimeCapacity:
		return agentcontrol.ErrorCodeRuntimeCapacity
	case agentcontrol.ErrorCodeModelError:
		return agentcontrol.ErrorCodeModelError
	case agentcontrol.ErrorCodeToolError:
		return agentcontrol.ErrorCodeToolError
	case agentcontrol.ErrorCodeRunCancelled:
		return agentcontrol.ErrorCodeRunCancelled
	case agentcontrol.ErrorCodeNoEligibleInventory:
		return agentcontrol.ErrorCodeNoEligibleInventory
	case agentcontrol.ErrorCodeUngroundedProposal:
		return agentcontrol.ErrorCodeUngroundedProposal
	case agentcontrol.ErrorCodeRuntimeError:
		return agentcontrol.ErrorCodeRuntimeError
	default:
		return agentcontrol.ErrorCodeRuntimeError
	}
}

type agentSSEEvent struct {
	RunID   string         `json:"runId"`
	Seq     int64          `json:"seq"`
	Type    string         `json:"type"`
	Message string         `json:"message"`
	Payload map[string]any `json:"payload"`
}

func writeAgentSSEEvent(w io.Writer, event agentcontrol.RuntimeEvent) error {
	payload, err := json.Marshal(agentSSEEvent{
		RunID: event.RunID, Seq: event.Seq, Type: safeEventType(event.Type),
		Message: safeEventMessage(event), Payload: safeEventPayload(event),
	})
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "id: %d\nevent: agent.run.event\ndata: %s\n\n", event.Seq, payload)
	return err
}

func writeSafeSSEError(w io.Writer, flusher http.Flusher, cause error) {
	retriable := retriableSSEError(cause)
	code := safeStreamErrorCode
	message := safeStreamErrorMessage
	if !retriable {
		code = "AGENT_EVENT_STREAM_PROTOCOL_ERROR"
		message = "Agent event stream request cannot be resumed."
	}
	payload, _ := json.Marshal(map[string]any{"code": code, "message": message, "retriable": retriable})
	_, _ = fmt.Fprintf(w, "event: agent.run.error\ndata: %s\n\n", payload)
	flusher.Flush()
}

func retriableSSEError(err error) bool {
	switch status.Code(err) {
	case codes.PermissionDenied, codes.Unauthenticated, codes.NotFound, codes.InvalidArgument, codes.FailedPrecondition, codes.Unimplemented:
		return false
	case codes.Canceled:
		return false
	default:
		return true
	}
}

func safeEventType(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 64 {
		return "run.updated"
	}
	for _, char := range value {
		if (char < 'a' || char > 'z') && (char < '0' || char > '9') && char != '.' && char != '_' && char != '-' {
			return "run.updated"
		}
	}
	return value
}

func safeEventMessage(event agentcontrol.RuntimeEvent) string {
	if strings.TrimSpace(event.Message) == agentcontrol.RuntimeUnavailableMessage {
		return agentcontrol.RuntimeUnavailableMessage
	}
	switch safeEventType(event.Type) {
	case "run.created":
		return "Run created."
	case "run.planning":
		return "Planning started."
	case "tool.started":
		return "Tool started."
	case "tool.completed":
		return "Tool completed."
	case "run.completed":
		return "Proposal completed."
	case "run.failed":
		return "Agent run failed."
	case "run.cancelled":
		return "Agent run cancelled."
	default:
		return "Agent run updated."
	}
}

const (
	maxProposalJSONBytes   = 1024 * 1024
	maxProposalTextBytes   = 2000
	maxProposalIDBytes     = 256
	maxProposalToolBytes   = 128
	maxProposalRoleBytes   = 128
	maxProposalLots        = 100
	maxProposalEvidence    = 200
	maxProposalRisks       = 100
	maxProposalSequence    = 1_000_000
	maxEstimatedMinutes    = 1440
	maxLotDurationSeconds  = 86_400
	maxLotExtensionSeconds = 3_600
)

type sessionProposalResponse struct {
	Summary                  string                `json:"summary"`
	Strategy                 string                `json:"strategy"`
	EstimatedDurationMinutes int64                 `json:"estimated_duration_minutes"`
	Lots                     []lotPlanResponse     `json:"lots"`
	Evidence                 []evidenceRefResponse `json:"evidence"`
	Risks                    []string              `json:"risks"`
}

type lotPlanResponse struct {
	LotID            string `json:"lot_id"`
	Sequence         int64  `json:"sequence"`
	Role             string `json:"role"`
	StartPriceAmount *int64 `json:"start_price_amount"`
	IncrementAmount  *int64 `json:"increment_amount"`
	DurationSeconds  int64  `json:"duration_seconds"`
	ExtensionSeconds int64  `json:"extension_seconds"`
	Reason           string `json:"reason"`
}

type evidenceRefResponse struct {
	Tool        string `json:"tool"`
	ReferenceID string `json:"reference_id"`
	Summary     string `json:"summary"`
}

func safeSessionProposal(raw string) *sessionProposalResponse {
	if len(raw) == 0 || len(raw) > maxProposalJSONBytes || !utf8.ValidString(raw) {
		return nil
	}
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	var proposal sessionProposalResponse
	if err := decoder.Decode(&proposal); err != nil {
		return nil
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return nil
	}
	if !validateSessionProposal(&proposal) {
		return nil
	}
	return &proposal
}

func validateSessionProposal(proposal *sessionProposalResponse) bool {
	if proposal == nil || !validateProposalText(&proposal.Summary, maxProposalTextBytes) {
		return false
	}
	if proposal.Strategy != "conservative" && proposal.Strategy != "balanced" && proposal.Strategy != "aggressive" {
		return false
	}
	if proposal.EstimatedDurationMinutes <= 0 || proposal.EstimatedDurationMinutes > maxEstimatedMinutes {
		return false
	}
	if len(proposal.Lots) == 0 || len(proposal.Lots) > maxProposalLots || proposal.Evidence == nil || len(proposal.Evidence) > maxProposalEvidence || proposal.Risks == nil || len(proposal.Risks) > maxProposalRisks {
		return false
	}
	for index := range proposal.Lots {
		lot := &proposal.Lots[index]
		if !validateProposalText(&lot.LotID, maxProposalIDBytes) || lot.Sequence <= 0 || lot.Sequence > maxProposalSequence ||
			!validateProposalText(&lot.Role, maxProposalRoleBytes) || lot.StartPriceAmount == nil || *lot.StartPriceAmount < 0 || lot.IncrementAmount == nil || *lot.IncrementAmount < 0 ||
			lot.DurationSeconds <= 0 || lot.DurationSeconds > maxLotDurationSeconds || lot.ExtensionSeconds <= 0 || lot.ExtensionSeconds > maxLotExtensionSeconds ||
			!validateProposalText(&lot.Reason, maxProposalTextBytes) {
			return false
		}
	}
	for index := range proposal.Evidence {
		evidence := &proposal.Evidence[index]
		if !validateProposalText(&evidence.Tool, maxProposalToolBytes) || !validateProposalText(&evidence.ReferenceID, maxProposalIDBytes) || !validateProposalText(&evidence.Summary, maxProposalTextBytes) {
			return false
		}
	}
	for index := range proposal.Risks {
		if !validateProposalText(&proposal.Risks[index], maxProposalTextBytes) {
			return false
		}
	}
	return true
}

func safeEventPayload(event agentcontrol.RuntimeEvent) map[string]any {
	if safeEventType(event.Type) != "run.failed" || len(event.PayloadJSON) == 0 || len(event.PayloadJSON) > 1024 {
		return map[string]any{}
	}
	decoder := json.NewDecoder(strings.NewReader(event.PayloadJSON))
	decoder.DisallowUnknownFields()
	var payload struct {
		ErrorCode string `json:"errorCode"`
	}
	if err := decoder.Decode(&payload); err != nil || strings.TrimSpace(payload.ErrorCode) == "" {
		return map[string]any{}
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return map[string]any{}
	}
	return map[string]any{"errorCode": safeBrowserErrorCode(payload.ErrorCode)}
}
