// Package buyersearch owns buyer retrieval rules. Adapters must revalidate
// authoritative facts; an index or cache is never a permission boundary.
package buyersearch

import (
	"context"
	"errors"
	v1 "live-auction-bid/backend/api/auction/service/v1"
	"time"
)

var (
	ErrInvalidQuery  = errors.New("invalid search query")
	ErrUnavailable   = errors.New("search unavailable")
	ErrCursorExpired = errors.New("search cursor expired")
	ErrInvalidCursor = errors.New("invalid search cursor")
	ErrCapacity      = errors.New("search capacity exhausted")
)

type Query struct {
	Text, RoomID, Cursor string
	MinPrice, MaxPrice   *int64
	Statuses             []v1.LotStatus
	PageSize             int
}
type Filters struct {
	RoomID   string         `json:"roomId"`
	MinPrice *int64         `json:"minPriceAmount,omitempty"`
	MaxPrice *int64         `json:"maxPriceAmount,omitempty"`
	Statuses []v1.LotStatus `json:"statuses"`
}
type ParsedQuery struct {
	SemanticText, CanonicalText string
	Filters                     Filters
	Tokens, Warnings            []string
	PageSize                    int
}
type Generation struct {
	ID, Table, Model, ProviderVersion, TemplateVersion string
	Dimensions                                         int
	MaxDistance                                        float64
}
type Revision struct{ Semantic, Metadata uint64 }
type Document struct {
	LotID, RoomID, MainAccountID, Text, Title, Category string
	Tags                                                []string
	Public                                              bool
	Status                                              v1.LotStatus
	Revision                                            Revision
	Hash                                                string
	Deleted                                             bool
}
type Candidate struct {
	LotID         string
	Distance      *float64
	LexicalScore  float64
	MatchedTokens []string
	Rank          int
}
type Fact struct {
	Lot          *v1.Lot
	Public       bool
	ReadAtUnixMs int64
}
type Hit struct {
	Fact        Fact
	MatchReason string
}
type Page struct {
	Hits                             []Hit
	Applied                          Filters
	Warnings                         []string
	Mode, DegradedReason, NextCursor string
}
type Index interface {
	Active(context.Context) (Generation, error)
	Vector(context.Context, Generation, []float64, Filters, int) ([]Candidate, error)
	Lexical(context.Context, Generation, ParsedQuery, int) ([]Candidate, error)
	Upsert(context.Context, Generation, Document, []float64) error
}
type Embedder interface {
	Embed(context.Context, []string) ([][]float64, error)
}
type Reader interface {
	ReadPublicFacts(context.Context, []string) (map[string]Fact, error)
}
type Cache interface {
	Vector(context.Context, string) ([]float64, bool, error)
	PutVector(context.Context, string, []float64, time.Duration) error
	Candidates(context.Context, string) ([]Candidate, bool, error)
	PutCandidates(context.Context, string, []Candidate, time.Duration) error
	Snapshot(context.Context, string) (Snapshot, bool, error)
	PutSnapshot(context.Context, string, Snapshot, time.Duration) error
}
type Snapshot struct {
	QueryHash, GenerationID string
	IDs                     []string
	Mode, DegradedReason    string
	ExpiresAtUnixMs         int64
}
type CursorPayload struct {
	SnapshotID      string `json:"s"`
	QueryHash       string `json:"q"`
	GenerationID    string `json:"g"`
	Offset          int    `json:"o"`
	ExpiresAtUnixMs int64  `json:"e"`
}

func PublicStatus(s v1.LotStatus) bool {
	return s == v1.LotStatus_LOT_STATUS_QUEUED || s == v1.LotStatus_LOT_STATUS_LIVE || s == v1.LotStatus_LOT_STATUS_EXTENDED
}
