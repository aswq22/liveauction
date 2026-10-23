package server

import (
	"strings"
	"testing"
)

var proposalAssignmentScanResult bool

func TestProposalSensitiveAssignmentScannerUsesLinearAllocationBound(t *testing.T) {
	delimiterDense := strings.Repeat("x:", maxProposalTextBytes/2)
	if len(delimiterDense) != maxProposalTextBytes {
		t.Fatalf("fixture length = %d, want %d", len(delimiterDense), maxProposalTextBytes)
	}

	result := testing.Benchmark(func(b *testing.B) {
		for index := 0; index < b.N; index++ {
			proposalAssignmentScanResult = containsProposalSensitiveAssignment(delimiterDense)
		}
	})
	if proposalAssignmentScanResult {
		t.Fatal("benign delimiter-dense text was classified as sensitive")
	}
	t.Logf("allocated bytes per max-length scan: %d", result.AllocedBytesPerOp())
	maxAllocatedBytes := int64(len(delimiterDense) * 32)
	if allocated := result.AllocedBytesPerOp(); allocated > maxAllocatedBytes {
		t.Fatalf("allocated bytes per scan = %d, want <= %d (linear input bound)", allocated, maxAllocatedBytes)
	}
}

func TestProposalSensitiveAssignmentScannerRequiresAnAssignedValue(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  bool
	}{
		{name: "empty", value: "password:", want: false},
		{name: "Unicode whitespace only", value: "password:\u00a0", want: false},
		{name: "assigned value", value: "password: value", want: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := containsProposalSensitiveAssignment(test.value); got != test.want {
				t.Fatalf("containsProposalSensitiveAssignment(%q) = %t, want %t", test.value, got, test.want)
			}
		})
	}
}
