package api

import (
	"context"
	"crypto/sha256"
	"errors"
)

type Principal struct {
	Actor string `json:"actor"`
	Task  string `json:"task,omitempty"`
}
type principalKey struct{}

func AuthenticatedPrincipal(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(principalKey{}).(Principal)
	return p, ok
}

// AddPrincipal installs a distinct capability before the server starts listening.
// Principal identity comes exclusively from this server configuration.
func (s *Server) AddPrincipal(credential []byte, p Principal) error {
	if len(credential) != 32 {
		return errors.New("credential must contain 32 bytes")
	}
	switch p.Actor {
	case "controller", "prophet", "workflow-role", "qa":
	default:
		return errors.New("invalid delegated principal")
	}
	digest := sha256.Sum256(credential)
	if digest == s.token {
		return errors.New("delegated credential duplicates operator")
	}
	if s.principals == nil {
		s.principals = map[[32]byte]Principal{}
	}
	if _, ok := s.principals[digest]; ok {
		return errors.New("duplicate delegated credential")
	}
	s.principals[digest] = p
	return nil
}
