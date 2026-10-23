package agentv1

import "testing"

func TestPhaseOneProtoDefaultsAreSafe(t *testing.T) {
	if RunStatus_RUN_STATUS_UNSPECIFIED != 0 {
		t.Fatal("run status zero value must be unspecified")
	}
	req := &QueryInventoryRequest{Context: &RequestContext{RunId: "run-1"}}
	if req.GetContext().GetRunId() != "run-1" {
		t.Fatal("request context did not round trip")
	}
}
