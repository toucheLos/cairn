package schema

import (
	"fmt"
	"strconv"
	"strings"
)

// JobID is a parsed scheduler job identifier.
//
// It is a struct rather than a string because Slurm job identifiers are not
// integers and the join depends on their internal structure. All of these are
// real, and all of them appear in ordinary sacct output:
//
//	12345           a plain job
//	12345.batch     the batch step
//	12345.extern    the external step (cgroup accounting for the whole allocation)
//	12345.0         a numbered step, e.g. one srun inside the script
//	12345_7         task 7 of a job array
//	12345_[8-20]    the pending remainder of an array, as a mask rather than a task
//	12345+0         component 0 of a heterogeneous job
//	12345_7.batch   combinations of the above
//
// CLAUDE.md §6 names array and heterogeneous jobs as Phase 1 join work, so the
// schema has to carry them from the start. Storing these as opaque strings would
// make "every event bearing on job 12345" impossible to answer correctly: the
// batch step, the extern step, and each array task must all resolve to the same
// base job.
type JobID struct {
	// Raw is the identifier exactly as the producer emitted it. It is preserved
	// verbatim so that output can be matched against what an admin sees in
	// sacct, and so that a parse we get wrong is still traceable.
	Raw string

	// Base is the numeric job identifier common to every step and array task.
	Base uint64

	// ArrayTask is the array task index, nil when this is not a single array
	// task. Nil does not imply "not an array" — see ArrayRange.
	ArrayTask *uint32

	// ArrayRange holds an unexpanded array mask such as "[8-20]" or "[8-20%4]",
	// which Slurm reports for the still-pending portion of an array. It is kept
	// unexpanded on purpose: expanding it would invent task identifiers for jobs
	// that may never run.
	ArrayRange string

	// HetOffset is the component index of a heterogeneous job, nil otherwise.
	HetOffset *uint32

	// Step is the step suffix without its dot: "", "batch", "extern", "0", ...
	// Empty means the identifier refers to the job rather than to a step.
	Step string
}

// ParseJobID parses a scheduler job identifier.
//
// It is strict: an identifier it cannot parse is an error rather than a
// best-effort partial result. Invariant §2.6 (never hard-fail on an unknown
// stack) applies to collectors, which should log and skip the record; it does
// not license the schema to silently produce a JobID whose Base is wrong,
// because that would corrupt the join rather than degrade it.
func ParseJobID(raw string) (*JobID, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return nil, fmt.Errorf("%w: empty job id", ErrInvalidEvent)
	}

	j := &JobID{Raw: s}
	head := s

	// Split the step suffix at the last dot that is not inside an array mask.
	// Masks such as [8-20%4] contain no dots today, but scanning at bracket
	// depth costs nothing and removes the assumption.
	if i := lastIndexAtDepth(head, '.'); i >= 0 {
		j.Step = head[i+1:]
		head = head[:i]
		if j.Step == "" {
			return nil, fmt.Errorf("%w: job id %q has an empty step suffix", ErrInvalidEvent, s)
		}
	}

	// Array and heterogeneous markers are mutually exclusive in Slurm: a job is
	// addressed either as base_task or as base+component, never both.
	arrIdx := indexAtDepth(head, '_')
	hetIdx := indexAtDepth(head, '+')
	switch {
	case arrIdx >= 0 && hetIdx >= 0:
		return nil, fmt.Errorf("%w: job id %q has both an array and a heterogeneous marker", ErrInvalidEvent, s)

	case arrIdx >= 0:
		suffix := head[arrIdx+1:]
		head = head[:arrIdx]
		if suffix == "" {
			return nil, fmt.Errorf("%w: job id %q has an empty array suffix", ErrInvalidEvent, s)
		}
		if strings.HasPrefix(suffix, "[") {
			if !strings.HasSuffix(suffix, "]") {
				return nil, fmt.Errorf("%w: job id %q has an unterminated array mask", ErrInvalidEvent, s)
			}
			j.ArrayRange = suffix
		} else {
			n, err := strconv.ParseUint(suffix, 10, 32)
			if err != nil {
				return nil, fmt.Errorf("%w: job id %q has a non-numeric array task %q", ErrInvalidEvent, s, suffix)
			}
			v := uint32(n)
			j.ArrayTask = &v
		}

	case hetIdx >= 0:
		suffix := head[hetIdx+1:]
		head = head[:hetIdx]
		n, err := strconv.ParseUint(suffix, 10, 32)
		if err != nil {
			return nil, fmt.Errorf("%w: job id %q has a non-numeric heterogeneous offset %q", ErrInvalidEvent, s, suffix)
		}
		v := uint32(n)
		j.HetOffset = &v
	}

	base, err := strconv.ParseUint(head, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("%w: job id %q has a non-numeric base %q", ErrInvalidEvent, s, head)
	}
	j.Base = base

	return j, nil
}

// indexAtDepth returns the first index of c outside any bracketed region, or -1.
func indexAtDepth(s string, c byte) int {
	depth := 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '[':
			depth++
		case ']':
			if depth > 0 {
				depth--
			}
		case c:
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

// lastIndexAtDepth returns the last index of c outside any bracketed region, or -1.
func lastIndexAtDepth(s string, c byte) int {
	depth, found := 0, -1
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '[':
			depth++
		case ']':
			if depth > 0 {
				depth--
			}
		case c:
			if depth == 0 {
				found = i
			}
		}
	}
	return found
}

// Validate checks internal consistency. A JobID built by hand rather than by
// ParseJobID can still be wrong, and it reaches the encoder either way.
func (j *JobID) Validate() error {
	if j == nil {
		return nil
	}
	if j.Raw == "" {
		return fmt.Errorf("%w: jobid.raw is empty", ErrInvalidEvent)
	}
	if j.ArrayTask != nil && j.ArrayRange != "" {
		return fmt.Errorf("%w: jobid %q has both an array task and an array mask", ErrInvalidEvent, j.Raw)
	}
	if (j.ArrayTask != nil || j.ArrayRange != "") && j.HetOffset != nil {
		return fmt.Errorf("%w: jobid %q is both an array task and a heterogeneous component", ErrInvalidEvent, j.Raw)
	}
	return nil
}

// RawOrEmpty returns the raw identifier, or "" for a nil JobID. It exists so
// that sorting and grouping do not need a nil check at every call site; a nil
// JobID is ordinary (node-without-jobid), not exceptional.
func (j *JobID) RawOrEmpty() string {
	if j == nil {
		return ""
	}
	return j.Raw
}

// BaseOrZero returns the base job number, or 0 for a nil JobID.
func (j *JobID) BaseOrZero() uint64 {
	if j == nil {
		return 0
	}
	return j.Base
}

// IsStep reports whether the identifier addresses a step rather than the job.
func (j *JobID) IsStep() bool { return j != nil && j.Step != "" }

// SameJob reports whether two identifiers belong to the same base job, ignoring
// step and array task. This is the predicate the join uses to answer "every
// event bearing on job N": 12345.batch, 12345.extern, and 12345_7 are all the
// same job for that purpose.
func (j *JobID) SameJob(other *JobID) bool {
	if j == nil || other == nil {
		return false
	}
	return j.Base == other.Base
}

func (j *JobID) String() string { return j.RawOrEmpty() }
