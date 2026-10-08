package common

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"net/http"
	"strconv"
	"strings"
)

// CodexTicketSummary identifies a ticket without retaining its reusable value.
type CodexTicketSummary struct {
	Length      int    `json:"length"`
	Fingerprint string `json:"fingerprint"`
	IssuedAt    int64  `json:"issued_at,omitempty"`
}

// CodexTicketObservation distinguishes a returned state from gateway-reported use.
// A missing returned state does not establish that no ticket was used upstream.
type CodexTicketObservation struct {
	Returned *CodexTicketSummary `json:"returned,omitempty"`
	Used     *CodexTicketSummary `json:"used,omitempty"`
}

func (info *RelayInfo) ObserveCodexTicketHeaders(headers http.Header) {
	observation := &CodexTicketObservation{}
	state := strings.TrimSpace(headers.Get("X-Codex-Turn-State"))
	if state != "" && len(state) <= 2048 {
		digest := sha256.Sum256([]byte(state))
		observation.Returned = &CodexTicketSummary{
			Length: len(state), Fingerprint: hex.EncodeToString(digest[:]),
		}
		// This only parses the envelope timestamp; it does not authenticate the ticket.
		raw, err := base64.RawURLEncoding.Strict().DecodeString(strings.TrimRight(state, "="))
		if err == nil && len(raw) >= 73 && raw[0] == 0x80 && (len(raw)-57)%16 == 0 {
			issued := binary.BigEndian.Uint64(raw[1:9])
			if issued >= 1577836800 && issued < 4102444800 {
				observation.Returned.IssuedAt = int64(issued)
			}
		}
	}
	// These optional headers are an upstream report, not a local observation of
	// a downstream gateway's internal request. Do not infer use from returned state.
	length, err := strconv.Atoi(headers.Get("X-Sub2api-Ticket-Used-Length"))
	fingerprint := strings.ToLower(strings.TrimSpace(headers.Get("X-Sub2api-Ticket-Used-Fingerprint")))
	if err == nil && length > 0 && length <= 2048 && len(fingerprint) == 64 {
		if _, err := hex.DecodeString(fingerprint); err == nil {
			observation.Used = &CodexTicketSummary{Length: length, Fingerprint: fingerprint}
		}
	}
	info.CodexTicket = observation
}
