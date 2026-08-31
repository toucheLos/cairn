package slurm

import (
	"fmt"
	"time"
)

// slurmTimeLayout is what sacct prints: ISO-like, with no zone.
//
// The missing zone is the whole problem. sacct renders in the local time of the
// host it runs on, so the same job looks like it ended at a different instant
// depending on who asked. Every timestamp must therefore be interpreted in an
// explicitly supplied location and converted to UTC — never parsed as if it were
// already UTC, which would silently shift every Slurm event by the site's offset
// and quietly misorder the join against journald.
const slurmTimeLayout = "2006-01-02T15:04:05"

// parseSlurmTime interprets a sacct timestamp in loc and returns it in UTC.
func parseSlurmTime(s string, loc *time.Location) (time.Time, error) {
	switch s {
	case "", "Unknown", "None", "N/A":
		return time.Time{}, fmt.Errorf("no timestamp")
	}
	if loc == nil {
		loc = time.UTC
	}
	t, err := time.ParseInLocation(slurmTimeLayout, s, loc)
	if err != nil {
		// Some versions include a zone; accept that too rather than discarding
		// the row.
		if t2, err2 := time.Parse(time.RFC3339, s); err2 == nil {
			return t2.UTC(), nil
		}
		return time.Time{}, err
	}
	return t.UTC(), nil
}
