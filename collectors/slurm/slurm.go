// Package slurm reads sacct, sacctmgr, scontrol, and slurm.conf.
//
// It emits observations of what the scheduler recorded, never conclusions about
// why a job died (schema/DESIGN.md §1). A sacct row reading OUT_OF_MEMORY is
// evidence that the scheduler saw an OOM kill; deciding that the OOM is the
// reason the *job* failed is the taxonomy's job in Phase 4.
package slurm

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/touchelos/cairn/collectors"
	"github.com/touchelos/cairn/schema"
)

// Collector reads Slurm accounting.
type Collector struct{}

func New() *Collector { return &Collector{} }

func (c *Collector) Source() schema.Source { return schema.SourceSlurm }

// sacctFields is the format cairn asks for, in order.
//
// An explicit --format is not optional. sacct's default columns vary by site
// configuration, so parsing whatever it happens to print would make the
// collector's behaviour depend on a setting nobody remembers changing.
var sacctFields = []string{
	"JobID", "JobName", "Partition", "Account", "AllocCPUS", "State", "ExitCode",
	"ReqMem", "MaxRSS", "Timelimit", "Elapsed", "NodeList", "Start", "End", "User",
}

// stateClass maps a Slurm job state to the class of the observation.
//
// These are observations, not diagnoses. CANCELLED means the scheduler cancelled
// the job — it does not say who or why, and a job cancelled because it hit its
// time limit produces both a TIMEOUT row and a CANCELLED row. Both are recorded;
// resolving them into one cause is Phase 4's problem.
var stateClass = map[string]schema.Class{
	"OUT_OF_MEMORY": schema.ClassResourceOOM,
	"TIMEOUT":       schema.ClassResourceWalltimeExceeded,
	"NODE_FAIL":     schema.ClassSchedNodeFail,
	"PREEMPTED":     schema.ClassSchedPreempted,
	"CANCELLED":     schema.ClassSchedCancelled,
	"REQUEUED":      schema.ClassSchedRequeued,
	"FAILED":        schema.ClassAppNonzeroExit,
	"BOOT_FAIL":     schema.ClassSchedNodeFail,
	"DEADLINE":      schema.ClassResourceWalltimeExceeded,
}

func (c *Collector) Collect(ctx context.Context, env collectors.Env, req collectors.Request) collectors.Result {
	res := collectors.Result{Source: schema.SourceSlurm}

	args := []string{"--parsable2", "--noheader", "--allocations=no",
		"--format=" + strings.Join(sacctFields, ",")}
	if req.Job != nil {
		args = append(args, "-j", req.Job.Raw)
	}

	out, err := env.Run(ctx, "sacct", args...)
	cap := collectors.Capability{
		Name:    "sacct",
		Level:   collectors.LevelUnprivileged,
		Reveals: "job state, exit code, resource limits and usage — the scheduler's own record of what happened",
	}
	if err != nil {
		if errors.Is(err, collectors.ErrNotFound) {
			cap.Detail = "sacct not available; is this a Slurm site, and is slurmdbd reachable?"
		} else {
			cap.Detail = err.Error()
		}
		res.Capabilities = append(res.Capabilities, cap)
		return res
	}
	cap.Available = true
	res.Capabilities = append(res.Capabilities, cap)

	res.Events, res.Warnings = parseSacct(out, req.Cluster, env.Location())

	// The job's stderr, located via scontrol. Done after sacct because the job's
	// end time is the only clock a stderr capture has.
	stderrEvents, caps, warns := c.collectStdErr(ctx, env, req, jobEnd(res.Events, req.Job))
	res.Events = append(res.Events, stderrEvents...)
	res.Capabilities = append(res.Capabilities, caps...)
	res.Warnings = append(res.Warnings, warns...)

	schema.SortEvents(res.Events)
	return res
}

// jobEnd returns the latest timestamp among events belonging to the job.
func jobEnd(evs []schema.Event, job *schema.JobID) time.Time {
	var out time.Time
	for _, e := range evs {
		if job != nil && !e.JobID.SameJob(job) {
			continue
		}
		if e.TS.After(out) {
			out = e.TS
		}
	}
	return out
}

// parseSacct handles both --parsable2 output and the aligned table sacct prints
// by default.
//
// Both, because the corpus contains captures of what an admin actually ran, and
// an admin at a terminal runs bare `sacct -j 12345`. A collector that only
// understood its own preferred flags could not be evaluated against real
// captures, which would defeat the point of the corpus.
func parseSacct(out []byte, cluster schema.ClusterName, loc *time.Location) ([]schema.Event, []string) {
	lines := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
	var events []schema.Event
	var warnings []string

	var cols []string
	var spans []span

	for i, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		// The dashed rule under an aligned header defines the column widths.
		if isRule(line) {
			spans = ruleSpans(line)
			continue
		}
		if strings.Contains(line, "|") {
			cols = nil
			spans = nil
			fields := strings.Split(line, "|")
			if isHeader(fields) {
				cols = fields
				continue
			}
			if cols == nil {
				cols = sacctFields
			}
			e, warn := rowToEvent(zip(cols, fields), cluster, loc)
			if warn != "" {
				warnings = append(warnings, fmt.Sprintf("sacct line %d: %s", i+1, warn))
			}
			if e != nil {
				events = append(events, *e)
			}
			continue
		}

		fields := strings.Fields(line)
		if isHeader(fields) {
			cols = fields
			continue
		}
		if spans == nil || cols == nil {
			warnings = append(warnings, fmt.Sprintf("sacct line %d: no header seen; cannot map columns", i+1))
			continue
		}
		e, warn := rowToEvent(zip(cols, splitFixed(line, spans)), cluster, loc)
		if warn != "" {
			warnings = append(warnings, fmt.Sprintf("sacct line %d: %s", i+1, warn))
		}
		if e != nil {
			events = append(events, *e)
		}
	}
	return events, warnings
}

func isRule(line string) bool {
	t := strings.TrimSpace(line)
	if t == "" {
		return false
	}
	return strings.Trim(t, "- ") == ""
}

// span is a half-open column range taken from the dashed rule.
type span struct{ start, end int }

// ruleSpans locates each run of dashes in the rule line.
//
// Positions, not widths. Accumulating widths and a separator per column drifts
// by one for every place the real output does not match the assumed layout, and
// the drift is silent until some column late in the row is sliced one character
// short — which turns a timestamp into an unparseable string and drops the
// event. Reading absolute positions cannot drift.
func ruleSpans(line string) []span {
	var out []span
	i := 0
	for i < len(line) {
		if line[i] != '-' {
			i++
			continue
		}
		start := i
		for i < len(line) && line[i] == '-' {
			i++
		}
		out = append(out, span{start, i})
	}
	return out
}

func isHeader(fields []string) bool {
	for _, f := range fields {
		if strings.EqualFold(strings.TrimSpace(f), "JobID") {
			return true
		}
	}
	return false
}

// splitFixed slices a row at the column positions taken from the dashed rule.
//
// Splitting on whitespace would be wrong: sacct pads with spaces and leaves
// cells empty, so a row with a blank MaxRSS would shift every subsequent value
// one column to the left — producing events that parse cleanly and mean
// something else entirely.
//
// The final column is extended to end of line, because a value wider than its
// header (a long NodeList, say) overflows to the right rather than being
// truncated.
func splitFixed(line string, spans []span) []string {
	out := make([]string, 0, len(spans))
	for i, sp := range spans {
		start, end := sp.start, sp.end
		if i == len(spans)-1 {
			end = len(line)
		}
		if start >= len(line) {
			out = append(out, "")
			continue
		}
		if end > len(line) {
			end = len(line)
		}
		out = append(out, strings.TrimSpace(line[start:end]))
	}
	return out
}

func zip(cols, fields []string) map[string]string {
	m := make(map[string]string, len(cols))
	for i, c := range cols {
		key := strings.TrimSpace(c)
		if i < len(fields) {
			m[key] = strings.TrimSpace(fields[i])
		} else {
			m[key] = ""
		}
	}
	return m
}

func rowToEvent(row map[string]string, cluster schema.ClusterName, loc *time.Location) (*schema.Event, string) {
	raw := row["JobID"]
	if raw == "" {
		return nil, ""
	}
	job, err := schema.ParseJobID(raw)
	if err != nil {
		return nil, fmt.Sprintf("unparseable job id %q: %v", raw, err)
	}

	state := normalizeState(row["State"])
	if state == "" {
		return nil, fmt.Sprintf("job %s has no state", raw)
	}
	// COMPLETED and RUNNING are outcomes, not observations worth an event. A
	// bundle full of "this step finished normally" buries the two lines that
	// matter.
	if state == "COMPLETED" || state == "RUNNING" || state == "PENDING" {
		return nil, ""
	}

	class, known := stateClass[state]
	if !known {
		// §2.6: an unrecognized state is recorded, not dropped. The signature
		// carries the state so the miss log names it precisely.
		class = schema.ClassUnknown
	}

	end, err := parseSlurmTime(row["End"], loc)
	if err != nil {
		return nil, fmt.Sprintf("job %s has an unusable End %q: %v", raw, row["End"], err)
	}

	attrs := map[string]string{"state": state}
	set := func(k, v string) {
		if v != "" && v != "Unknown" {
			attrs[k] = v
		}
	}
	set("exit_code", row["ExitCode"])
	set("account", row["Account"])
	set("partition", row["Partition"])
	set("user", row["User"])

	switch class {
	case schema.ClassResourceOOM:
		set("limit_bytes", bytesOf(row["ReqMem"]))
		set("usage_bytes", bytesOf(row["MaxRSS"]))
	case schema.ClassResourceWalltimeExceeded:
		set("limit_seconds", secondsOf(row["Timelimit"]))
		set("elapsed_seconds", secondsOf(row["Elapsed"]))
	case schema.ClassSchedCancelled, schema.ClassSchedPreempted,
		schema.ClassSchedNodeFail, schema.ClassSchedRequeued:
		set("reason", row["Reason"])
	}

	// Drop attrs the schema does not register for this class rather than
	// emitting an event the encoder will reject. Losing one field is a warning;
	// losing the whole event is a hole in the bundle.
	for k := range attrs {
		if !schema.AttrAllowed(class, k) {
			delete(attrs, k)
		}
	}

	node := firstNode(row["NodeList"])
	return &schema.Event{
		TS:      end,
		Cluster: cluster,
		Node:    schema.Hostname(node),
		JobID:   job,
		Source:  schema.SourceSlurm,
		Class:   class,
		Detail: schema.Detail{
			Signature: "slurm.sacct.state." + state,
			Attrs:     attrs,
		},
	}, ""
}

// normalizeState strips the annotations sacct adds and undoes its truncation.
//
// Aligned output truncates with a trailing "+", so OUT_OF_MEMORY arrives as
// "OUT_OF_ME+". A truncated state that is not resolved becomes ClassUnknown, and
// an entire class of failure would go unclassified for a reason no one would
// think to look for.
func normalizeState(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	// "CANCELLED by 12345" -> "CANCELLED"
	if i := strings.Index(s, " by "); i > 0 {
		s = s[:i]
	}
	s = strings.TrimSpace(s)
	if !strings.HasSuffix(s, "+") {
		return s
	}
	stem := strings.TrimSuffix(s, "+")
	var matches []string
	for full := range stateClass {
		if strings.HasPrefix(full, stem) {
			matches = append(matches, full)
		}
	}
	for _, full := range []string{"COMPLETED", "RUNNING", "PENDING"} {
		if strings.HasPrefix(full, stem) {
			matches = append(matches, full)
		}
	}
	if len(matches) == 1 {
		return matches[0]
	}
	// Ambiguous or unrecognized: keep the truncated form so the miss log shows
	// exactly what was seen, rather than a guess.
	sort.Strings(matches)
	return s
}

// firstNode reduces a NodeList to a single hostname.
//
// A step normally runs on one node. Ranges such as node-[0044-0047] are not
// expanded here: an event carries one node, and inventing four events from one
// accounting row would misreport how many things were observed. The join
// attaches node-scoped events by other means.
func firstNode(list string) string {
	list = strings.TrimSpace(list)
	if list == "" || list == "None assigned" {
		return ""
	}
	if strings.ContainsAny(list, "[,") {
		return ""
	}
	return list
}

// bytesOf converts a Slurm size such as "8G", "8190M", or "1204K" to bytes.
func bytesOf(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	// Slurm suffixes memory with c (per-cpu) or n (per-node); both are stripped,
	// since the per-unit distinction belongs to the request, not the observation.
	s = strings.TrimRight(s, "cn")
	if s == "" {
		return ""
	}
	mult := uint64(1)
	switch s[len(s)-1] {
	case 'K', 'k':
		mult, s = 1<<10, s[:len(s)-1]
	case 'M', 'm':
		mult, s = 1<<20, s[:len(s)-1]
	case 'G', 'g':
		mult, s = 1<<30, s[:len(s)-1]
	case 'T', 't':
		mult, s = 1<<40, s[:len(s)-1]
	}
	// Slurm prints fractional values such as "1.50G".
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return ""
	}
	return strconv.FormatUint(uint64(f*float64(mult)), 10)
}

// secondsOf converts a Slurm duration to seconds.
// Accepted: SS, MM:SS, HH:MM:SS, D-HH:MM:SS, D-HH:MM.
func secondsOf(s string) string {
	s = strings.TrimSpace(s)
	if s == "" || s == "UNLIMITED" || s == "Partition_Limit" || s == "INVALID" {
		return ""
	}
	days := 0
	if i := strings.Index(s, "-"); i > 0 {
		d, err := strconv.Atoi(s[:i])
		if err != nil {
			return ""
		}
		days, s = d, s[i+1:]
	}
	parts := strings.Split(s, ":")
	if len(parts) > 3 {
		return ""
	}
	total := 0
	for _, p := range parts {
		// Sub-second precision appears in Elapsed on some versions; the schema
		// records whole seconds, and a fraction of a second never changes a
		// walltime conclusion.
		if i := strings.Index(p, "."); i >= 0 {
			p = p[:i]
		}
		v, err := strconv.Atoi(p)
		if err != nil {
			return ""
		}
		total = total*60 + v
	}
	return strconv.Itoa(days*86400 + total)
}
