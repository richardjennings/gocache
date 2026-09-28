// Package api defines the HTTP interface between the client and the server.
//
//	GET /a/{actionID}  the Action stored under an action ID, or 404
//	PUT /a/{actionID}  store an Action (JSON)
//	GET /o/{outputID}  the output bytes, or 404
//	PUT /o/{outputID}  store output bytes; the server checks their SHA-256
//	GET /stats         counters (JSON)
//
// IDs are the 64-character lowercase hex form of the go command's 32-byte
// action and output IDs.
package api

import (
	"encoding/hex"
	"time"
)

// Action is what the server stores under an action ID.
type Action struct {
	OutputID string    `json:"output"`
	Size     int64     `json:"size"`
	Time     time.Time `json:"time"`
}

// Stats are the server's counters since it started.
type Stats struct {
	ActionHits   int64 `json:"action_hits"`
	ActionMisses int64 `json:"action_misses"`
	OutputPuts   int64 `json:"output_puts"`
}

// ActionPath and OutputPath give the URL path of an action or an output.
func ActionPath(id string) string { return "/a/" + id }
func OutputPath(id string) string { return "/o/" + id }

// ValidID reports whether id is the hex form of a 32-byte ID.
func ValidID(id string) bool {
	if len(id) != 64 {
		return false
	}
	for _, c := range id {
		if !('0' <= c && c <= '9' || 'a' <= c && c <= 'f') {
			return false
		}
	}
	return true
}

// ID gives the hex form of a raw ID.
func ID(raw []byte) string { return hex.EncodeToString(raw) }
