package caddyconsul

import (
	"fmt"
	"strconv"
	"strings"
)

// StatusMatcher efficiently matches HTTP status codes against a configured set
// of individual codes and class wildcards (e.g., "3xx", "502", "503").
type StatusMatcher struct {
	classes [6]bool    // index 1-5 → 1xx-5xx class wildcards
	codes   map[int]bool // individual codes
}

// ParseStatusMatcher parses a comma-separated status code spec into a StatusMatcher.
// Returns nil for empty input (opt-out / no modification).
// Accepts: "3xx", "3XX", "301", "4xx,502,503", etc.
func ParseStatusMatcher(spec string) (*StatusMatcher, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return nil, nil
	}

	sm := &StatusMatcher{
		codes: make(map[int]bool),
	}

	tokens := strings.Split(spec, ",")
	for _, token := range tokens {
		token = strings.TrimSpace(token)
		if token == "" {
			continue
		}

		lower := strings.ToLower(token)
		// Check for class wildcard: Nxx
		if len(lower) == 3 && lower[1] == 'x' && lower[2] == 'x' {
			digit := lower[0] - '0'
			if digit < 1 || digit > 5 {
				return nil, fmt.Errorf("invalid status class %q: must be 1xx-5xx", token)
			}
			sm.classes[digit] = true
			continue
		}

		// Try individual status code
		code, err := strconv.Atoi(token)
		if err != nil {
			return nil, fmt.Errorf("invalid status code %q: must be a 3-digit number or class wildcard (e.g., 3xx)", token)
		}
		if code < 100 || code > 599 {
			return nil, fmt.Errorf("invalid status code %d: must be between 100 and 599", code)
		}
		sm.codes[code] = true
	}

	return sm, nil
}

// Matches returns true if the given status code is in the configured set.
func (sm *StatusMatcher) Matches(statusCode int) bool {
	if sm == nil {
		return false
	}
	// Check class wildcard first
	class := statusCode / 100
	if class >= 1 && class <= 5 && sm.classes[class] {
		return true
	}
	// Check individual codes
	return sm.codes[statusCode]
}
