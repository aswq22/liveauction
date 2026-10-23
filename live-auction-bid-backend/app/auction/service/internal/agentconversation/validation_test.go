package agentconversation

import (
	"errors"
	"reflect"
	"testing"

	"live-auction-bid/backend/app/auction/service/internal/pkg/apperr"
)

func TestMissingRequiredInputsReturnsStableInputsForNewCandidate(t *testing.T) {
	valid := validCandidate()
	valid.Draft.StartPriceAmount = 0
	valid.StartPriceSpecified = true

	tests := []struct {
		name      string
		candidate Candidate
		want      []RequiredInput
	}{
		{
			name: "title",
			candidate: func() Candidate {
				candidate := valid
				candidate.Draft.Title = ""
				return candidate
			}(),
			want: []RequiredInput{RequiredTitle},
		},
		{
			name: "explicit start price",
			candidate: func() Candidate {
				candidate := valid
				candidate.StartPriceSpecified = false
				return candidate
			}(),
			want: []RequiredInput{RequiredStartPrice},
		},
		{
			name: "duration",
			candidate: func() Candidate {
				candidate := valid
				candidate.Draft.DurationSeconds = 0
				return candidate
			}(),
			want: []RequiredInput{RequiredDuration},
		},
		{
			name: "main image",
			candidate: func() Candidate {
				candidate := valid
				candidate.Draft.MainImageAssetID = ""
				candidate.Draft.MainImageURL = ""
				return candidate
			}(),
			want: []RequiredInput{RequiredMainImage},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := MissingRequiredInputs(test.candidate); !reflect.DeepEqual(got, test.want) {
				t.Fatalf("MissingRequiredInputs() = %v, want %v", got, test.want)
			}
		})
	}
}

func TestMissingRequiredInputsRetainsExplicitZeroStartPrice(t *testing.T) {
	candidate := validCandidate()
	candidate.Draft.StartPriceAmount = 0
	candidate.StartPriceSpecified = true

	if got := MissingRequiredInputs(candidate); len(got) != 0 {
		t.Fatalf("MissingRequiredInputs() = %v, want no missing inputs", got)
	}
}

func TestValidateCandidateRejectsExistingCandidateWithoutImmutableLotReference(t *testing.T) {
	version := int64(3)
	tests := []struct {
		name      string
		candidate Candidate
	}{
		{
			name: "lot id",
			candidate: func() Candidate {
				candidate := validCandidate()
				candidate.SourceType = SourceExisting
				candidate.ExistingLotReadVersion = &version
				return candidate
			}(),
		},
		{
			name: "read version",
			candidate: func() Candidate {
				candidate := validCandidate()
				candidate.SourceType = SourceExisting
				candidate.ExistingLotID = "lot-1"
				return candidate
			}(),
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := ValidateCandidate(test.candidate)
			if !errors.Is(err, apperr.ErrAgentRequiredInputMissing) {
				t.Fatalf("ValidateCandidate() error = %v, want ErrAgentRequiredInputMissing", err)
			}
			if got := MissingRequiredInputs(test.candidate); !reflect.DeepEqual(got, []RequiredInput{RequiredAmbiguousCandidate}) {
				t.Fatalf("MissingRequiredInputs() = %v, want %v", got, []RequiredInput{RequiredAmbiguousCandidate})
			}
		})
	}
}

func TestValidateCandidateRejectsNewCandidateWithExistingLotReference(t *testing.T) {
	version := int64(3)
	tests := []struct {
		name      string
		candidate Candidate
	}{
		{
			name: "lot id",
			candidate: func() Candidate {
				candidate := validCandidate()
				candidate.ExistingLotID = "lot-1"
				return candidate
			}(),
		},
		{
			name: "read version",
			candidate: func() Candidate {
				candidate := validCandidate()
				candidate.ExistingLotReadVersion = &version
				return candidate
			}(),
		},
		{
			name: "lot id and read version",
			candidate: func() Candidate {
				candidate := validCandidate()
				candidate.ExistingLotID = "lot-1"
				candidate.ExistingLotReadVersion = &version
				return candidate
			}(),
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := ValidateCandidate(test.candidate)
			if !errors.Is(err, apperr.ErrAgentRequiredInputMissing) {
				t.Fatalf("ValidateCandidate() error = %v, want ErrAgentRequiredInputMissing", err)
			}
			if got := MissingRequiredInputs(test.candidate); !reflect.DeepEqual(got, []RequiredInput{RequiredAmbiguousCandidate}) {
				t.Fatalf("MissingRequiredInputs() = %v, want %v", got, []RequiredInput{RequiredAmbiguousCandidate})
			}
		})
	}
}

func TestValidateCandidateEnforcesAuctionRules(t *testing.T) {
	capBelowStart := int64(100)
	tests := []struct {
		name      string
		candidate Candidate
	}{
		{
			name: "negative start price",
			candidate: func() Candidate {
				candidate := validCandidate()
				candidate.Draft.StartPriceAmount = -1
				return candidate
			}(),
		},
		{
			name: "negative deposit",
			candidate: func() Candidate {
				candidate := validCandidate()
				candidate.Draft.DepositAmount = -1
				return candidate
			}(),
		},
		{
			name: "minimum increment must be positive",
			candidate: func() Candidate {
				candidate := validCandidate()
				candidate.Draft.MinIncrementAmount = 0
				return candidate
			}(),
		},
		{
			name: "duration is at least one minute",
			candidate: func() Candidate {
				candidate := validCandidate()
				candidate.Draft.DurationSeconds = 59
				return candidate
			}(),
		},
		{
			name: "cap price exceeds start price",
			candidate: func() Candidate {
				candidate := validCandidate()
				candidate.Draft.CapPriceAmount = &capBelowStart
				return candidate
			}(),
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := ValidateCandidate(test.candidate); !errors.Is(err, apperr.ErrInvalidArgument) {
				t.Fatalf("ValidateCandidate() error = %v, want ErrInvalidArgument", err)
			}
		})
	}
}

func TestCanTransitionAllowsOnlySessionWorkflowTransitions(t *testing.T) {
	tests := []struct {
		from, to SessionStatus
		want     bool
	}{
		{StatusCollectingInput, StatusReadyForReview, true},
		{StatusCollectingInput, StatusCancelled, true},
		{StatusReadyForReview, StatusApplying, true},
		{StatusReadyForReview, StatusNeedsRevision, true},
		{StatusApplying, StatusApplied, true},
		{StatusApplying, StatusNeedsRevision, true},
		{StatusNeedsRevision, StatusCollectingInput, true},
		{StatusApplied, StatusCollectingInput, false},
		{StatusCancelled, StatusReadyForReview, false},
		{StatusReadyForReview, StatusApplied, false},
		{StatusCollectingInput, StatusApplying, false},
	}

	for _, test := range tests {
		if got := CanTransition(test.from, test.to); got != test.want {
			t.Errorf("CanTransition(%q, %q) = %t, want %t", test.from, test.to, got, test.want)
		}
	}
}

func validCandidate() Candidate {
	return Candidate{
		ID:                  "candidate-1",
		SourceType:          SourceNew,
		StartPriceSpecified: true,
		Draft: CandidateDraft{
			Title:                  "Ocean Heart",
			StartPriceAmount:       1000,
			DurationSeconds:        300,
			MainImageAssetID:       "asset-1",
			MinIncrementAmount:     100,
			AntiSnipeWindowSeconds: 30,
			AntiSnipeExtendSeconds: 10,
			MaxExtendCount:         3,
			DepositAmount:          0,
		},
	}
}
