package common

import "time"

// EarlyResponsesState belongs to one downstream HTTP response. Header waiting and
// stream scanning take turns owning Timer; stream writes use the scanner lock.
type EarlyResponsesState struct {
	Chat           bool
	Timer          *time.Timer
	ID             string
	UpstreamID     string
	CreatedAt      int64
	Sequence       int
	SentAt         time.Time
	LastProgressAt time.Time
	Finished       bool
	FirstEvent     bool
	Response       map[string]any
	FailureEvent   string
}

func (s *EarlyResponsesState) Pending() <-chan time.Time {
	if s == nil || s.Timer == nil || s.Finished || s.FirstEvent || !s.SentAt.IsZero() {
		return nil
	}
	return s.Timer.C
}

func (s *EarlyResponsesState) Stop() {
	if s != nil && s.Timer != nil {
		s.Timer.Stop()
	}
}
