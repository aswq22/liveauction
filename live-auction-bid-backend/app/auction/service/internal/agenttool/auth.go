package agenttool

import (
	"context"
	"crypto/subtle"
	"errors"
	"strings"

	agentv1 "live-auction-bid/backend/api/agent/v1"
	"live-auction-bid/backend/app/auction/service/internal/agentcontrol"
	"live-auction-bid/backend/app/auction/service/internal/pkg/apperr"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

func (s *Service) authorize(ctx context.Context, requestContext *agentv1.RequestContext) (agentcontrol.Run, error) {
	if !validBearerToken(ctx, s.token) {
		return agentcontrol.Run{}, status.Error(codes.Unauthenticated, "valid internal bearer token is required")
	}
	runID := strings.TrimSpace(requestContext.GetRunId())
	if runID == "" {
		return agentcontrol.Run{}, status.Error(codes.InvalidArgument, "run_id is required")
	}
	run, err := s.runs.Find(ctx, runID)
	if err != nil {
		return agentcontrol.Run{}, toolError(err)
	}
	if strings.TrimSpace(run.ID) == "" {
		return agentcontrol.Run{}, status.Error(codes.NotFound, "agent run not found")
	}
	if strings.TrimSpace(run.TenantID) == "" || strings.TrimSpace(run.OperatorID) == "" {
		return agentcontrol.Run{}, status.Error(codes.PermissionDenied, "agent run has no persisted owner")
	}
	return run, nil
}

func validBearerToken(ctx context.Context, expected string) bool {
	values := metadata.ValueFromIncomingContext(ctx, "authorization")
	if len(values) != 1 {
		return false
	}
	value := values[0]
	if !strings.HasPrefix(value, "Bearer ") {
		return false
	}
	provided := strings.TrimPrefix(value, "Bearer ")
	if provided == "" || strings.ContainsAny(provided, " \t\r\n") {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(provided), []byte(expected)) == 1
}

func toolError(err error) error {
	switch {
	case errors.Is(err, apperr.ErrUnauthenticated):
		return status.Error(codes.Unauthenticated, "unauthenticated")
	case errors.Is(err, apperr.ErrPermissionDenied):
		return status.Error(codes.PermissionDenied, "permission denied")
	case errors.Is(err, apperr.ErrNotFound):
		return status.Error(codes.NotFound, "not found")
	case errors.Is(err, apperr.ErrInvalidArgument):
		return status.Error(codes.InvalidArgument, "invalid argument")
	default:
		return status.Error(codes.Internal, "internal server error")
	}
}
