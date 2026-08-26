package web

import (
	"encoding/base64"
	"encoding/json"
	"net/http"

	"github.com/vesal1/avaswebsite/internal/betting"
)

// maxSlipLegs bounds what a cookie may carry, independent of the book's own
// parlay limit. A cookie is attacker-controlled input, so it is bounded before
// anything reads it.
const maxSlipLegs = 30

// slipEntry is one selection held on the bet slip.
type slipEntry struct {
	SelectionID int64 `json:"s"`
	// OddsMilli is the price the customer was shown when they added the
	// selection. It is never trusted as a price; it is only compared against
	// the live price so a move can be surfaced before the bet is struck.
	OddsMilli int64 `json:"o"`
}

// readSlip decodes the bet slip cookie. A malformed cookie yields an empty
// slip rather than an error: the slip is UI state, and every value in it is
// revalidated against the database before a bet is placed.
func readSlip(r *http.Request) []slipEntry {
	cookie, err := r.Cookie(betslipCookieName)
	if err != nil || cookie.Value == "" {
		return nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(cookie.Value)
	if err != nil {
		return nil
	}
	var entries []slipEntry
	if err := json.Unmarshal(raw, &entries); err != nil {
		return nil
	}
	if len(entries) > maxSlipLegs {
		entries = entries[:maxSlipLegs]
	}
	// Drop anything that could not name a real selection.
	filtered := entries[:0]
	seen := make(map[int64]bool, len(entries))
	for _, entry := range entries {
		if entry.SelectionID <= 0 || seen[entry.SelectionID] {
			continue
		}
		seen[entry.SelectionID] = true
		filtered = append(filtered, entry)
	}
	return filtered
}

// writeSlip stores the bet slip cookie.
func (s *Server) writeSlip(w http.ResponseWriter, entries []slipEntry) {
	if len(entries) == 0 {
		http.SetCookie(w, &http.Cookie{
			Name: betslipCookieName, Value: "", Path: "/", HttpOnly: true,
			Secure: s.cfg.IsProduction(), SameSite: http.SameSiteLaxMode, MaxAge: -1,
		})
		return
	}
	if len(entries) > maxSlipLegs {
		entries = entries[:maxSlipLegs]
	}
	encoded, err := json.Marshal(entries)
	if err != nil {
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     betslipCookieName,
		Value:    base64.RawURLEncoding.EncodeToString(encoded),
		Path:     "/",
		HttpOnly: true,
		Secure:   s.cfg.IsProduction(),
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int((7 * 24 * 60 * 60)),
	})
}

// slipToLegs converts the cookie into the betting service's input.
func slipToLegs(entries []slipEntry) []betting.SlipLeg {
	legs := make([]betting.SlipLeg, 0, len(entries))
	for _, entry := range entries {
		legs = append(legs, betting.SlipLeg{
			SelectionID: entry.SelectionID, ExpectedOddsMilli: entry.OddsMilli,
		})
	}
	return legs
}
