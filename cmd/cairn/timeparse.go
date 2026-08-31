package main

import "time"

// parseIn parses s in the given layout, interpreting a zone-less timestamp as
// UTC rather than local. A miss log entry is compared against bundle timestamps,
// which are always UTC, so guessing the operator's zone here would misalign them.
func parseIn(layout, s string) (time.Time, error) {
	t, err := time.ParseInLocation(layout, s, time.UTC)
	if err != nil {
		return time.Time{}, err
	}
	return t.UTC(), nil
}
