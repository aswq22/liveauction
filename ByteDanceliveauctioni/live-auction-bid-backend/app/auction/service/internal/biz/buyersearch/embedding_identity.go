package buyersearch

import (
	"errors"
	"fmt"
)

var ErrEmbeddingIdentity = errors.New("embedding generation identity mismatch")

func ResolveEmbedder(e Embedder, g Generation) (Embedder, error) {
	if e == nil {
		return nil, ErrUnavailable
	}
	if r, ok := e.(interface {
		ForGeneration(Generation) (Embedder, error)
	}); ok {
		resolved, err := r.ForGeneration(g)
		if err != nil {
			return nil, fmt.Errorf("%w: unavailable deployment", ErrEmbeddingIdentity)
		}
		e = resolved
	}
	if identity, ok := e.(interface {
		Model() string
		Dimensions() int
	}); ok && (identity.Model() != g.Model || identity.Dimensions() != g.Dimensions) {
		return nil, ErrEmbeddingIdentity
	}
	return e, nil
}
