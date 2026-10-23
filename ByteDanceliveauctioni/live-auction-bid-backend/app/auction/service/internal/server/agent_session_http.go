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

	agentv1 "live-auction-bid/backend/api/agent/v1"
	"live-auction-bid/backend/app/auction/service/internal/agentapproval"
	"live-auction-bid/backend/app/auction/service/internal/agentcontrol"
	"live-auction-bid/backend/app/auction/service/internal/agentconversation"
	userbiz "live-auction-bid/backend/app/auction/service/internal/biz/user"
	"live-auction-bid/backend/app/auction/service/internal/pkg/apperr"
	"live-auction-bid/backend/app/auction/service/internal/pkg/auth"
	"live-auction-bid/backend/app/auction/service/internal/pkg/requestctx"
	appsvc "live-auction-bid/backend/app/auction/service/internal/service"

	httptransport "github.com/go-kratos/kratos/v2/transport/http"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const agentSessionsPath = "/api/admin/agent/sessions"

type agentSessionHTTPHandler struct {
	service   *agentconversation.Service
	approval  *agentapproval.Service
	auth      *auth.Manager
	heartbeat time.Duration
}

func NewAgentSessionHTTPHandler(service *agentconversation.Service, authManager *auth.Manager, options AgentHTTPOptions, approvals ...*agentapproval.Service) http.Handler {
	heartbeat := options.HeartbeatInterval
	if heartbeat <= 0 {
		heartbeat = defaultAgentHeartbeat
	}
	var approval *agentapproval.Service
	if len(approvals) > 0 {
		approval = approvals[0]
	}
	return &agentSessionHTTPHandler{service: service, approval: approval, auth: authManager, heartbeat: heartbeat}
}

// NewAgentAdminHTTPHandler keeps the pre-existing run endpoints and the
// session endpoints behind one registered admin agent handler.
func NewAgentAdminHTTPHandler(runs, sessions http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if strings.HasPrefix(request.URL.Path, agentSessionsPath) && sessions != nil {
			sessions.ServeHTTP(w, request)
			return
		}
		if runs != nil {
			runs.ServeHTTP(w, request)
			return
		}
		http.NotFound(w, request)
	})
}

func registerAgentSessionHTTP(server *httptransport.Server, handler http.Handler) {
	if server != nil && handler != nil {
		server.HandlePrefix(agentSessionsPath, handler)
	}
}

func (h *agentSessionHTTPHandler) ServeHTTP(w http.ResponseWriter, request *http.Request) {
	if h.service == nil {
		http.NotFound(w, request)
		return
	}
	if request.URL.Path == agentSessionsPath {
		if request.Method != http.MethodPost {
			methodNotAllowed(w, http.MethodPost)
			return
		}
		h.create(w, request)
		return
	}
	if !strings.HasPrefix(request.URL.Path, agentSessionsPath+"/") {
		http.NotFound(w, request)
		return
	}
	parts := strings.Split(strings.TrimPrefix(request.URL.Path, agentSessionsPath+"/"), "/")
	if len(parts) == 0 || parts[0] == "" {
		http.NotFound(w, request)
		return
	}
	switch {
	case len(parts) == 1 && request.Method == http.MethodGet:
		h.get(w, request, parts[0])
	case len(parts) == 1:
		methodNotAllowed(w, http.MethodGet)
	case len(parts) == 2 && parts[1] == "messages" && request.Method == http.MethodPost:
		h.message(w, request, parts[0])
	case len(parts) == 2 && parts[1] == "messages":
		methodNotAllowed(w, http.MethodPost)
	case len(parts) == 2 && parts[1] == "events" && request.Method == http.MethodGet:
		h.events(w, request, parts[0])
	case len(parts) == 2 && parts[1] == "events":
		methodNotAllowed(w, http.MethodGet)
	case len(parts) == 2 && parts[1] == "cancel" && request.Method == http.MethodPost:
		h.cancel(w, request, parts[0])
	case len(parts) == 2 && parts[1] == "cancel":
		methodNotAllowed(w, http.MethodPost)
	case len(parts) == 2 && parts[1] == "review-snapshots" && request.Method == http.MethodPost:
		h.createReviewSnapshot(w, request, parts[0])
	case len(parts) == 2 && parts[1] == "review-snapshots":
		methodNotAllowed(w, http.MethodPost)
	case len(parts) == 3 && parts[1] == "review-snapshots" && request.Method == http.MethodGet:
		h.getReviewSnapshot(w, request, parts[0], parts[2])
	case len(parts) == 3 && parts[1] == "review-snapshots":
		methodNotAllowed(w, http.MethodGet)
	case len(parts) == 2 && parts[1] == "apply" && request.Method == http.MethodPost:
		h.apply(w, request, parts[0])
	case len(parts) == 2 && parts[1] == "apply":
		methodNotAllowed(w, http.MethodPost)
	case len(parts) == 3 && parts[1] == "candidates" && request.Method == http.MethodPatch:
		h.patchCandidate(w, request, parts[0], parts[2])
	case len(parts) == 3 && parts[1] == "candidates":
		methodNotAllowed(w, http.MethodPatch)
	case len(parts) == 4 && parts[1] == "candidates" && parts[3] == "images" && request.Method == http.MethodPost:
		h.bindCandidateImage(w, request, parts[0], parts[2])
	case len(parts) == 4 && parts[1] == "candidates" && parts[3] == "images":
		methodNotAllowed(w, http.MethodPost)
	default:
		http.NotFound(w, request)
	}
}

func (h *agentSessionHTTPHandler) apply(w http.ResponseWriter, r *http.Request, sessionID string) {
	ctx, actor, ok := h.actor(w, r)
	if !ok {
		return
	}
	key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if len(key) < 8 || !validAgentIdempotencyKey(key) {
		writeJSONResult(w, appsvc.ErrorResult(ctx, fmt.Errorf("%w: invalid idempotency key", apperr.ErrInvalidArgument)), nil)
		return
	}
	var body struct {
		SnapshotVersion      int64    `json:"snapshotVersion"`
		ContentHash          string   `json:"contentHash"`
		SelectedCandidateIDs []string `json:"selectedCandidateIds"`
		OrderedCandidateIDs  []string `json:"orderedCandidateIds"`
		Target               string   `json:"target"`
	}
	if !decodeSessionBody(w, r, &body) {
		return
	}
	if h.approval == nil {
		writeJSONResult(w, appsvc.ErrorResult(ctx, apperr.ErrAgentBatchApplyFailed), nil)
		return
	}
	command := agentapproval.ApplyCommand{
		TenantID: actor.TenantID, OperatorID: actor.OperatorID, SessionID: sessionID,
		SnapshotVersion: body.SnapshotVersion, ContentHash: body.ContentHash,
		SelectedCandidateIDs: body.SelectedCandidateIDs, OrderedCandidateIDs: body.OrderedCandidateIDs,
		IdempotencyKey: key, TraceID: requestctx.TraceID(ctx),
		Target: agentapproval.ApplyTarget(body.Target),
	}
	if command.Target == "" {
		command.Target = agentapproval.ApplyTargetQueue
	}
	if command.Target == agentapproval.ApplyTargetQueue {
		if _, err := auth.RequirePermission(ctx, userbiz.PermissionLotQueue); err != nil {
			writeJSONResult(w, appsvc.ErrorResult(ctx, err), nil)
			return
		}
	}
	snapshot, requiresCreate, err := h.approval.Requirements(ctx, command)
	if err != nil {
		writeJSONResult(w, appsvc.ErrorResult(ctx, agentApplyHTTPError(err)), nil)
		return
	}
	if requiresCreate {
		if _, err := auth.RequirePermission(ctx, userbiz.PermissionLotCreate); err != nil {
			writeJSONResult(w, appsvc.ErrorResult(ctx, err), nil)
			return
		}
	}
	execution, err := h.approval.Apply(ctx, command)
	if err != nil {
		writeJSONResult(w, appsvc.ErrorResult(ctx, agentApplyHTTPError(err)), nil)
		return
	}
	queued, err := approvalQueuedLots(snapshot, execution, command.Target)
	if err != nil {
		writeJSONResult(w, appsvc.ErrorResult(ctx, apperr.ErrAgentBatchApplyFailed), nil)
		return
	}
	writeJSONResult(w, appsvc.OKResult(ctx), map[string]any{
		"executionId": execution.ID, "createdLotIds": execution.CreatedLotIDs, "queuedLots": queued, "target": command.Target,
	})
}

func approvalQueuedLots(snapshot agentconversation.ReviewSnapshot, execution agentapproval.Execution, target agentapproval.ApplyTarget) ([]map[string]any, error) {
	if target == agentapproval.ApplyTargetInventoryOnly {
		if len(execution.QueuePositions) != 0 || len(execution.CreatedLotIDs) != len(snapshot.Candidates) {
			return nil, apperr.ErrAgentBatchApplyFailed
		}
		for _, candidate := range snapshot.Candidates {
			if candidate.SourceType != agentconversation.SourceNew {
				return nil, apperr.ErrAgentBatchApplyFailed
			}
		}
		return []map[string]any{}, nil
	}
	if len(snapshot.Candidates) != len(execution.QueuePositions) {
		return nil, apperr.ErrAgentBatchApplyFailed
	}
	created := 0
	queued := make([]map[string]any, 0, len(snapshot.Candidates))
	for i, candidate := range snapshot.Candidates {
		lotID := candidate.ExistingLotID
		if candidate.SourceType == agentconversation.SourceNew {
			if created >= len(execution.CreatedLotIDs) {
				return nil, apperr.ErrAgentBatchApplyFailed
			}
			lotID = execution.CreatedLotIDs[created]
			created++
		}
		if strings.TrimSpace(lotID) == "" {
			return nil, apperr.ErrAgentBatchApplyFailed
		}
		queued = append(queued, map[string]any{"lotId": lotID, "queuePosition": execution.QueuePositions[i]})
	}
	if created != len(execution.CreatedLotIDs) {
		return nil, apperr.ErrAgentBatchApplyFailed
	}
	return queued, nil
}

func (h *agentSessionHTTPHandler) createReviewSnapshot(w http.ResponseWriter, r *http.Request, sessionID string) {
	ctx, actor, ok := h.actor(w, r)
	if !ok {
		return
	}
	var body struct {
		ExpectedSessionVersion int64    `json:"expectedSessionVersion"`
		SelectedCandidateIDs   []string `json:"selectedCandidateIds"`
		OrderedCandidateIDs    []string `json:"orderedCandidateIds"`
	}
	if !decodeSessionBody(w, r, &body) {
		return
	}
	if body.ExpectedSessionVersion <= 0 || len(body.SelectedCandidateIDs) == 0 || len(body.OrderedCandidateIDs) == 0 {
		writeJSONResult(w, appsvc.ErrorResult(ctx, fmt.Errorf("%w: explicit selection, order and session version are required", apperr.ErrInvalidArgument)), nil)
		return
	}
	snapshot, err := h.service.CreateReviewSnapshot(ctx, actor, agentconversation.CreateReviewSnapshotInput{
		SessionID: sessionID, ExpectedSessionVersion: body.ExpectedSessionVersion,
		SelectedCandidateIDs: body.SelectedCandidateIDs, OrderedCandidateIDs: body.OrderedCandidateIDs,
	})
	if err != nil {
		writeJSONResult(w, appsvc.ErrorResult(ctx, agentSessionHTTPError(err)), nil)
		return
	}
	writeJSONResult(w, appsvc.OKResult(ctx), reviewSnapshotResponse(snapshot))
}

func (h *agentSessionHTTPHandler) getReviewSnapshot(w http.ResponseWriter, r *http.Request, sessionID, rawVersion string) {
	ctx, actor, ok := h.actor(w, r)
	if !ok {
		return
	}
	if rawVersion == "" || strings.Trim(rawVersion, "0123456789") != "" {
		writeJSONResult(w, appsvc.ErrorResult(ctx, fmt.Errorf("%w: invalid snapshot version", apperr.ErrInvalidArgument)), nil)
		return
	}
	version, err := strconv.ParseInt(rawVersion, 10, 64)
	if err != nil || version <= 0 {
		writeJSONResult(w, appsvc.ErrorResult(ctx, fmt.Errorf("%w: invalid snapshot version", apperr.ErrInvalidArgument)), nil)
		return
	}
	snapshot, err := h.service.GetReviewSnapshot(ctx, actor, sessionID, version)
	if err != nil {
		writeJSONResult(w, appsvc.ErrorResult(ctx, agentSessionHTTPError(err)), nil)
		return
	}
	writeJSONResult(w, appsvc.OKResult(ctx), reviewSnapshotResponse(snapshot))
}

func reviewSnapshotResponse(snapshot agentconversation.ReviewSnapshot) map[string]any {
	return map[string]any{"snapshotVersion": snapshot.SnapshotVersion, "contentHash": snapshot.ContentHash, "candidates": snapshot.Candidates}
}

func (h *agentSessionHTTPHandler) bindCandidateImage(w http.ResponseWriter, r *http.Request, sessionID, candidateID string) {
	ctx, actor, ok := h.actor(w, r)
	if !ok {
		return
	}
	if _, err := auth.RequirePermission(ctx, userbiz.PermissionUploadImage); err != nil {
		writeJSONResult(w, appsvc.ErrorResult(ctx, err), nil)
		return
	}
	var body struct {
		ExpectedSessionVersion   int64  `json:"expectedSessionVersion"`
		ExpectedCandidateVersion int64  `json:"expectedCandidateVersion"`
		AssetID                  string `json:"assetId"`
	}
	if !decodeSessionBody(w, r, &body) {
		return
	}
	aggregate, err := h.service.BindCandidateImage(ctx, actor, agentconversation.BindCandidateImageCommand{
		SessionID: sessionID, CandidateID: candidateID, AssetID: body.AssetID,
		ExpectedSessionVersion: body.ExpectedSessionVersion, ExpectedCandidateVersion: body.ExpectedCandidateVersion,
	})
	if err != nil {
		writeJSONResult(w, appsvc.ErrorResult(ctx, agentSessionHTTPError(err)), nil)
		return
	}
	writeJSONResult(w, appsvc.OKResult(ctx), aggregateResponse(aggregate))
}

func (h *agentSessionHTTPHandler) patchCandidate(w http.ResponseWriter, r *http.Request, sessionID, candidateID string) {
	ctx, actor, ok := h.actor(w, r)
	if !ok {
		return
	}
	candidateVersion, err := parseCandidateIfMatch(r.Header.Get("If-Match"))
	if err != nil {
		writeJSONResult(w, appsvc.ErrorResult(ctx, err), nil)
		return
	}
	var body struct {
		ExpectedSessionVersion int64                      `json:"expectedSessionVersion"`
		Fields                 map[string]json.RawMessage `json:"fields"`
	}
	if !decodeSessionBody(w, r, &body) {
		return
	}
	if body.ExpectedSessionVersion <= 0 || len(body.Fields) == 0 {
		writeJSONResult(w, appsvc.ErrorResult(ctx, fmt.Errorf("%w: candidate patch fields and session version are required", apperr.ErrInvalidArgument)), nil)
		return
	}
	aggregate, err := h.service.PatchCandidate(ctx, actor, agentconversation.PatchCandidateCommand{
		SessionID: sessionID, CandidateID: candidateID,
		ExpectedSessionVersion: body.ExpectedSessionVersion, ExpectedCandidateVersion: candidateVersion,
		Fields: body.Fields,
	})
	if err != nil {
		writeJSONResult(w, appsvc.ErrorResult(ctx, agentSessionHTTPError(err)), nil)
		return
	}
	writeJSONResult(w, appsvc.OKResult(ctx), aggregateResponse(aggregate))
}

func parseCandidateIfMatch(value string) (int64, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" || strings.ContainsAny(trimmed, `",`) {
		return 0, fmt.Errorf("%w: If-Match must be a candidate version", apperr.ErrInvalidArgument)
	}
	version, err := strconv.ParseInt(trimmed, 10, 64)
	if err != nil || version <= 0 {
		return 0, fmt.Errorf("%w: If-Match must be a candidate version", apperr.ErrInvalidArgument)
	}
	return version, nil
}

func (h *agentSessionHTTPHandler) create(w http.ResponseWriter, r *http.Request) {
	ctx, actor, ok := h.actor(w, r)
	if !ok {
		return
	}
	var body struct {
		RoomID string `json:"roomId"`
	}
	if !decodeSessionBody(w, r, &body) {
		return
	}
	aggregate, err := h.service.CreateSession(ctx, actor, body.RoomID)
	if err != nil {
		writeJSONResult(w, appsvc.ErrorResult(ctx, err), nil)
		return
	}
	writeJSONResult(w, appsvc.OKResult(ctx), map[string]any{"session": sessionResponse(aggregate.Session)})
}

func (h *agentSessionHTTPHandler) get(w http.ResponseWriter, r *http.Request, id string) {
	ctx, actor, ok := h.actor(w, r)
	if !ok {
		return
	}
	aggregate, err := h.service.Recover(ctx, actor, id)
	if err != nil {
		writeJSONResult(w, appsvc.ErrorResult(ctx, agentSessionHTTPError(err)), nil)
		return
	}
	writeJSONResult(w, appsvc.OKResult(ctx), aggregateResponse(aggregate))
}

func (h *agentSessionHTTPHandler) message(w http.ResponseWriter, r *http.Request, id string) {
	ctx, actor, ok := h.actor(w, r)
	if !ok {
		return
	}
	key := r.Header.Get("Idempotency-Key")
	if !validAgentIdempotencyKey(key) {
		writeAgentJSONResult(w, http.StatusBadRequest, appsvc.ErrorResult(ctx, fmt.Errorf("%w: invalid idempotency key", apperr.ErrInvalidArgument)), nil)
		return
	}
	var body struct {
		ExpectedVersion int64  `json:"expectedVersion"`
		Content         string `json:"content"`
		RunType         string `json:"runType"`
	}
	if !decodeSessionBody(w, r, &body) {
		return
	}
	runType, err := sessionRunType(body.RunType)
	if err != nil {
		writeJSONResult(w, appsvc.ErrorResult(ctx, err), nil)
		return
	}
	aggregate, run, err := h.service.SubmitMessage(ctx, actor, agentconversation.SubmitCommand{SessionID: id, ExpectedVersion: body.ExpectedVersion, Content: body.Content, RunType: runType, IdempotencyKey: key})
	if err != nil {
		if failed, admission := agentcontrol.AdmissionRun(err); admission {
			writeAgentJSONResult(w, http.StatusTooManyRequests, appsvc.ResourceExhaustedResult(ctx, err.Error()), map[string]any{"session": sessionResponse(aggregate.Session), "run": agentRunHTTPResponse(failed)})
			return
		}
		writeJSONResult(w, appsvc.ErrorResult(ctx, agentSessionHTTPError(err)), nil)
		return
	}
	writeJSONResult(w, appsvc.OKResult(ctx), map[string]any{"session": sessionResponse(aggregate.Session), "run": agentRunHTTPResponse(run)})
}

func (h *agentSessionHTTPHandler) cancel(w http.ResponseWriter, r *http.Request, id string) {
	ctx, actor, ok := h.actor(w, r)
	if !ok {
		return
	}
	var body struct {
		ExpectedVersion int64 `json:"expectedVersion"`
	}
	if !decodeSessionBody(w, r, &body) {
		return
	}
	if err := h.service.Cancel(ctx, actor, id, body.ExpectedVersion); err != nil {
		writeJSONResult(w, appsvc.ErrorResult(ctx, agentSessionHTTPError(err)), nil)
		return
	}
	writeJSONResult(w, appsvc.OKResult(ctx), nil)
}

func agentSessionHTTPError(err error) error {
	if errors.Is(err, apperr.ErrAgentSessionVersionConflict) {
		return fmt.Errorf("%w: %v", apperr.ErrConflict, err)
	}
	if errors.Is(err, apperr.ErrAgentReviewSnapshotStale) || errors.Is(err, apperr.ErrAgentCandidateChanged) {
		return fmt.Errorf("%w: review snapshot is stale", apperr.ErrConflict)
	}
	if errors.Is(err, apperr.ErrAgentRequiredInputMissing) {
		return fmt.Errorf("%w: required candidate input is missing", apperr.ErrInvalidArgument)
	}
	if errors.Is(err, apperr.ErrAgentImageAssetInvalid) {
		return fmt.Errorf("%w: invalid image asset", apperr.ErrInvalidArgument)
	}
	return err
}

func agentApplyHTTPError(err error) error {
	if errors.Is(err, apperr.ErrAgentSessionVersionConflict) {
		return apperr.ErrAgentReviewSnapshotStale
	}
	return err
}

func (h *agentSessionHTTPHandler) events(w http.ResponseWriter, r *http.Request, id string) {
	ctx, actor, ok := h.actor(w, r)
	if !ok {
		return
	}
	after, err := parseAfterSeq(r)
	if err != nil {
		writeJSONResult(w, appsvc.ErrorResult(ctx, err), nil)
		return
	}
	streamCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	stream, err := h.service.StreamEvents(streamCtx, actor, id, after)
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
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache, no-store")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()
	type received struct {
		event agentcontrol.RuntimeEvent
		err   error
	}
	channel := make(chan received, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			event, recvErr := stream.Recv()
			select {
			case channel <- received{event, recvErr}:
			case <-streamCtx.Done():
				return
			}
			if recvErr != nil {
				return
			}
		}
	}()
	defer func() { cancel(); _ = stream.Close(); <-done }()
	ticker := time.NewTicker(h.heartbeat)
	defer ticker.Stop()
	last := after
	for {
		select {
		case <-streamCtx.Done():
			return
		case <-ticker.C:
			if _, err := io.WriteString(w, ": heartbeat\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case result := <-channel:
			if result.err != nil {
				if errors.Is(result.err, io.EOF) || errors.Is(result.err, context.Canceled) || streamCtx.Err() != nil {
					return
				}
				writeSafeSSEError(w, flusher, result.err)
				return
			}
			if result.event.Seq != last+1 || result.event.Seq <= last {
				writeSafeSSEError(w, flusher, status.Error(codes.Unavailable, "event sequence handoff"))
				return
			}
			if err := writeAgentSSEEvent(w, result.event); err != nil {
				return
			}
			last = result.event.Seq
			flusher.Flush()
		}
	}
}

func (h *agentSessionHTTPHandler) actor(w http.ResponseWriter, r *http.Request) (context.Context, agentconversation.Actor, bool) {
	ctx := r.Context()
	if h.auth != nil {
		ctx = h.auth.WithAuthContextFromBearer(ctx, r.Header.Get("Authorization"))
	}
	claims, err := auth.RequirePermission(ctx, userbiz.PermissionLotViewAdmin)
	if err != nil {
		writeJSONResult(w, appsvc.ErrorResult(ctx, err), nil)
		return ctx, agentconversation.Actor{}, false
	}
	actor := agentconversation.Actor{TenantID: auth.EffectiveMainAccountID(claims), OperatorID: strings.TrimSpace(claims.UserID)}
	if actor.TenantID == "" || actor.OperatorID == "" {
		writeJSONResult(w, appsvc.ErrorResult(ctx, apperr.ErrPermissionDenied), nil)
		return ctx, agentconversation.Actor{}, false
	}
	return ctx, actor, true
}
func decodeSessionBody(w http.ResponseWriter, r *http.Request, target any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxAgentRequestBytes)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if err := d.Decode(target); err != nil {
		writeJSONResult(w, appsvc.ErrorResult(r.Context(), fmt.Errorf("%w: invalid request body", apperr.ErrInvalidArgument)), nil)
		return false
	}
	var trailing any
	if err := d.Decode(&trailing); !errors.Is(err, io.EOF) {
		writeJSONResult(w, appsvc.ErrorResult(r.Context(), fmt.Errorf("%w: request body must contain one JSON document", apperr.ErrInvalidArgument)), nil)
		return false
	}
	return true
}
func sessionRunType(value string) (agentv1.RunType, error) {
	switch strings.TrimSpace(value) {
	case "SESSION_PLAN":
		return agentv1.RunType_RUN_TYPE_SESSION_PLAN, nil
	case "LOT_DRAFT_ASSIST":
		return agentv1.RunType_RUN_TYPE_LOT_DRAFT_ASSIST, nil
	default:
		return 0, fmt.Errorf("%w: invalid run type", apperr.ErrInvalidArgument)
	}
}

type sessionHTTPResponse struct {
	ID                    string `json:"id"`
	RoomID                string `json:"roomId"`
	Status                string `json:"status"`
	Version               int64  `json:"version"`
	ActiveSnapshotVersion int64  `json:"activeSnapshotVersion"`
	ConversationSummary   string `json:"conversationSummary"`
	CreatedAtUnixMs       int64  `json:"createdAtUnixMs"`
	UpdatedAtUnixMs       int64  `json:"updatedAtUnixMs"`
}

func sessionResponse(session agentconversation.Session) sessionHTTPResponse {
	return sessionHTTPResponse{ID: session.ID, RoomID: session.RoomID, Status: string(session.Status), Version: session.Version, ActiveSnapshotVersion: session.ActiveSnapshotVersion, ConversationSummary: session.ConversationSummary, CreatedAtUnixMs: session.CreatedAtUnixMs, UpdatedAtUnixMs: session.UpdatedAtUnixMs}
}
func aggregateResponse(aggregate agentconversation.SessionAggregate) map[string]any {
	var active any
	lastRunType := ""
	for i := range aggregate.Runs {
		run := aggregate.Runs[i]
		if value := browserRunType(run.RunType); value != "" {
			lastRunType = value
		}
		if run.Status != agentcontrol.StatusCompleted && run.Status != agentcontrol.StatusFailed && run.Status != agentcontrol.StatusCancelled {
			active = agentRunHTTPResponse(run)
		}
	}
	return map[string]any{"session": sessionResponse(aggregate.Session), "messages": aggregate.Messages, "candidates": aggregate.Candidates, "activeRun": active, "lastRunType": lastRunType, "appliedExecution": aggregate.AppliedExecution}
}
