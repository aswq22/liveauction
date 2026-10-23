package server

import (
	"testing"

	agentv1 "live-auction-bid/backend/api/agent/v1"
)

func TestNewAgentToolGRPCServerRegistersOnlyToolBusinessService(t *testing.T) {
	srv := NewAgentToolGRPCServer(":0", testAgentToolService{})
	services := srv.GetServiceInfo()
	tool, ok := services["agent.v1.AuctionAgentToolService"]
	if !ok {
		t.Fatal("AuctionAgentToolService is not registered")
	}
	if len(tool.Methods) != 3 {
		t.Fatalf("AuctionAgentToolService methods = %d, want 3", len(tool.Methods))
	}
	for _, forbidden := range []string{"Create", "Update", "Delete", "Execute"} {
		for _, method := range tool.Methods {
			if method.Name == forbidden {
				t.Fatalf("write-capable method %q is registered", forbidden)
			}
		}
	}
}

type testAgentToolService struct {
	agentv1.UnimplementedAuctionAgentToolServiceServer
}
