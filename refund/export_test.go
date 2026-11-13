package refund

import (
	"context"
	"time"
)

// The crash points of the sender and the ledger resolutions, for the crash table.
const (
	StepPreChecks = stepPreChecks
	StepPending   = stepPending
	StepMarker    = stepMarker
	StepSent      = stepSent
	StepPost      = stepPost
	StepVoid      = stepVoid
)

// SendRound runs one round of the sender.
func SendRound(s *Service, ctx context.Context) { s.sendRound(ctx) }

// ResolveRound runs one round of the resolver.
func ResolveRound(s *Service, ctx context.Context) { s.resolveRound(ctx) }

// SetCrash sets the crash hook: f runs after each step; true stops the work there. Set it before any round runs.
func SetCrash(s *Service, f func(step string) bool) { s.crash = f }

// SetTiming sets the workers' periods and the resolver's lookup gap. Set it before Run.
func SetTiming(s *Service, send, resolve, gap time.Duration) {
	s.sendEvery, s.resolveEvery, s.lookupGap = send, resolve, gap
}

// LogRetry is the retry log line.
const LogRetry = logRetry

// StepVoidPending is the crash point right after the void creates the refund's pending transfer.
const StepVoidPending = stepVoidPending

// VoidRow is the row the void gives a refund in the store kind ("claimed" or "sent") and the ledger letter ('N', 'P',
// 'S' or 'V'): "V1a" to "V1e", or "" for a refusal.
func VoidRow(k string, letter byte) string {
	kinds := map[string]kind{"claimed": kindClaimed, "sent": kindSent}
	return voidRow(kinds[k], letter)
}

// Waiting reports whether a decided result of the refund waits for its write (Service.retry).
func Waiting(s *Service, id string) bool {
	s.transition.Lock()
	defer s.transition.Unlock()
	_, waiting := s.retry[id]
	return waiting
}
