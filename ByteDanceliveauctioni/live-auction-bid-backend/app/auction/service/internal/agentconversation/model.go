// Package agentconversation contains the pure domain model for a recoverable
// agent-assisted auction drafting session. It deliberately has no transport,
// storage, or model-provider dependencies.
package agentconversation

type SessionStatus string

const (
	StatusCollectingInput SessionStatus = "COLLECTING_INPUT"
	StatusReadyForReview  SessionStatus = "READY_FOR_REVIEW"
	StatusApplying        SessionStatus = "APPLYING"
	StatusApplied         SessionStatus = "APPLIED"
	StatusNeedsRevision   SessionStatus = "NEEDS_REVISION"
	StatusCancelled       SessionStatus = "CANCELLED"
)

type SourceType string

const (
	SourceExisting SourceType = "EXISTING"
	SourceNew      SourceType = "NEW"
)

type RequiredInput string

const (
	RequiredTitle              RequiredInput = "TITLE"
	RequiredStartPrice         RequiredInput = "START_PRICE"
	RequiredDuration           RequiredInput = "DURATION"
	RequiredMainImage          RequiredInput = "MAIN_IMAGE"
	RequiredAmbiguousCandidate RequiredInput = "AMBIGUOUS_CANDIDATE"
)

// Candidate is a proposal item, not a business lot. New candidates remain
// proposals until a later approval transaction creates the corresponding lot.
// Existing candidates retain the lot identity and the version read while
// drafting so that approval can detect a concurrent change.
type Candidate struct {
	ID                     string         `json:"id"`
	SourceType             SourceType     `json:"sourceType"`
	ExistingLotID          string         `json:"existingLotId,omitempty"`
	ExistingLotReadVersion *int64         `json:"existingLotReadVersion,omitempty"`
	StartPriceSpecified    bool           `json:"startPriceSpecified"`
	Draft                  CandidateDraft `json:"draft"`
}

type CandidateDraft struct {
	Title                  string   `json:"title"`
	StartPriceAmount       int64    `json:"startPriceAmount"`
	DurationSeconds        int32    `json:"durationSeconds"`
	MainImageAssetID       string   `json:"mainImageAssetId"`
	MainImageURL           string   `json:"mainImageUrl"`
	MinIncrementAmount     int64    `json:"minIncrementAmount"`
	CapPriceAmount         *int64   `json:"capPriceAmount,omitempty"`
	Category               string   `json:"category"`
	Description            string   `json:"description"`
	Tags                   []string `json:"tags"`
	Stock                  int32    `json:"stock"`
	AntiSnipeWindowSeconds int32    `json:"antiSnipeWindowSeconds"`
	AntiSnipeExtendSeconds int32    `json:"antiSnipeExtendSeconds"`
	MaxExtendCount         int32    `json:"maxExtendCount"`
	DepositAmount          int64    `json:"depositAmount"`
}
