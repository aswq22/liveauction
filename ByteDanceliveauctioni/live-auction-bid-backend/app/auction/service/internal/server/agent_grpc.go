package server

import (
	agentv1 "live-auction-bid/backend/api/agent/v1"

	"github.com/go-kratos/kratos/v2/middleware/recovery"
	grpc "github.com/go-kratos/kratos/v2/transport/grpc"
)

func NewAgentToolGRPCServer(addr string, service agentv1.AuctionAgentToolServiceServer) *grpc.Server {
	server := grpc.NewServer(
		grpc.Address(addr),
		grpc.Middleware(recovery.Recovery()),
	)
	agentv1.RegisterAuctionAgentToolServiceServer(server, service)
	return server
}
