package agentconversation

import (
	"fmt"

	"live-auction-bid/backend/app/auction/service/internal/pkg/apperr"
)

// MissingRequiredInputs returns stable, user-actionable gaps in the candidate.
// StartPriceSpecified distinguishes an explicit CNY 0-cent start price from an
// amount left at Go's zero value.
func MissingRequiredInputs(candidate Candidate) []RequiredInput {
	missing := make([]RequiredInput, 0, 5)
	if candidate.Draft.Title == "" {
		missing = append(missing, RequiredTitle)
	}
	if !candidate.StartPriceSpecified {
		missing = append(missing, RequiredStartPrice)
	}
	if candidate.Draft.DurationSeconds == 0 {
		missing = append(missing, RequiredDuration)
	}
	if candidate.Draft.MainImageAssetID == "" && candidate.Draft.MainImageURL == "" {
		missing = append(missing, RequiredMainImage)
	}
	if candidate.SourceType != SourceNew && candidate.SourceType != SourceExisting ||
		candidate.SourceType == SourceExisting && (candidate.ExistingLotID == "" || candidate.ExistingLotReadVersion == nil) ||
		candidate.SourceType == SourceNew && (candidate.ExistingLotID != "" || candidate.ExistingLotReadVersion != nil) {
		missing = append(missing, RequiredAmbiguousCandidate)
	}
	return missing
}

// ValidateCandidate applies deterministic pre-approval validation. Monetary
// values are CNY cents; CandidateDraft intentionally has no currency field so
// it cannot introduce cross-currency draft rules.
func ValidateCandidate(candidate Candidate) error {
	if missing := MissingRequiredInputs(candidate); len(missing) > 0 {
		return fmt.Errorf("%w: %v", apperr.ErrAgentRequiredInputMissing, missing)
	}

	draft := candidate.Draft
	if draft.StartPriceAmount < 0 {
		return fmt.Errorf("%w: start price amount must be >= 0", apperr.ErrInvalidArgument)
	}
	if draft.MinIncrementAmount <= 0 {
		return fmt.Errorf("%w: min increment amount must be > 0", apperr.ErrInvalidArgument)
	}
	if draft.DurationSeconds < 60 {
		return fmt.Errorf("%w: duration seconds must be >= 60", apperr.ErrInvalidArgument)
	}
	if draft.AntiSnipeWindowSeconds <= 0 {
		return fmt.Errorf("%w: anti-snipe window seconds must be > 0", apperr.ErrInvalidArgument)
	}
	if draft.AntiSnipeExtendSeconds < 10 || draft.AntiSnipeExtendSeconds > 30 {
		return fmt.Errorf("%w: anti-snipe extend seconds must be between 10 and 30", apperr.ErrInvalidArgument)
	}
	if draft.MaxExtendCount <= 0 {
		return fmt.Errorf("%w: max extend count must be > 0", apperr.ErrInvalidArgument)
	}
	if draft.DepositAmount < 0 {
		return fmt.Errorf("%w: deposit amount must be >= 0", apperr.ErrInvalidArgument)
	}
	if draft.CapPriceAmount != nil {
		if *draft.CapPriceAmount < 0 {
			return fmt.Errorf("%w: cap price amount must be >= 0", apperr.ErrInvalidArgument)
		}
		if *draft.CapPriceAmount <= draft.StartPriceAmount {
			return fmt.Errorf("%w: cap price amount must be greater than start price amount", apperr.ErrInvalidArgument)
		}
	}
	return nil
}

// CanTransition reports whether the session lifecycle permits the requested
// transition. Applied and cancelled sessions are terminal.
func CanTransition(from, to SessionStatus) bool {
	switch from {
	case StatusCollectingInput:
		return to == StatusReadyForReview || to == StatusCancelled
	case StatusReadyForReview:
		return to == StatusApplying || to == StatusNeedsRevision || to == StatusCancelled
	case StatusApplying:
		return to == StatusApplied || to == StatusNeedsRevision
	case StatusNeedsRevision:
		return to == StatusCollectingInput || to == StatusReadyForReview || to == StatusCancelled
	default:
		return false
	}
}
