// Package journal reads journald and the slurmd/slurmctld logs.
//
// This is the collector where invariant §2.3 — no log storage — is actually
// enforced. It reads a great deal of text and emits none of it. A line that
// matches a signature becomes an event carrying the signature name and whatever
// structured values were extracted; the line itself is discarded.
//
// That constraint is deliberate and it costs something: a line nobody has
// written a signature for produces nothing but a warning. The alternative — keep
// the line "just in case" — is how a diagnostic tool turns into a log shipper.
package journal

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/touchelos/cairn/collectors"
	"github.com/touchelos/cairn/schema"
)

// MaxLinesUnbounded caps a journal read when the caller supplied no time window.
const MaxLinesUnbounded = 10000

type Collector struct{}

func New() *Collector { return &Collector{} }

func (c *Collector) Source() schema.Source { return schema.SourceJournal }

// signature matches one kind of log line.
type signature struct {
	name  string
	class schema.Class
	re    *regexp.Regexp
	// attrs maps a named capture group to a detail key. A group with no entry
	// here is used by the pattern but not emitted.
	attrs map[string]string
	// derive computes extra attrs from the match. Used where a value needs
	// converting rather than copying.
	derive func(m map[string]string) map[string]string
}

// signatures is the recognized vocabulary, tried in order.
//
// Order matters where patterns overlap: the more specific line must come first.
// Each entry is a claim about what a producer prints, so each is a place the
// corpus can prove us wrong.
var signatures = []signature{
	// ---- kernel: memory ----
	{
		name:  "kernel.memcg.oom_kill",
		class: schema.ClassResourceOOM,
		re: regexp.MustCompile(`Memory cgroup out of memory: Killed process (?P<pid>\d+) \((?P<comm>[^)]+)\)` +
			`(?:.*?anon-rss:(?P<anonrss>\d+)kB)?`),
		attrs: map[string]string{"pid": "killed_pid", "comm": "killed_comm"},
	},
	{
		name:  "kernel.oom.invoked",
		class: schema.ClassResourceOOM,
		re:    regexp.MustCompile(`(?P<comm>\S+) invoked oom-killer`),
		attrs: map[string]string{"comm": "killed_comm"},
	},
	{
		name:  "kernel.memcg.limit_reached",
		class: schema.ClassResourceOOM,
		re:    regexp.MustCompile(`^memory: usage (?P<usage>\d+)kB, limit (?P<limit>\d+)kB`),
		derive: func(m map[string]string) map[string]string {
			return map[string]string{
				"usage_bytes": kbToBytes(m["usage"]),
				"limit_bytes": kbToBytes(m["limit"]),
			}
		},
	},

	// ---- slurmstepd ----
	{
		name:  "slurm.slurmstepd.oom_kill_detected",
		class: schema.ClassResourceOOM,
		// The step id ends at the dot that closes the sentence, not the dot
		// inside "918273.batch" — a non-greedy match to the first dot silently
		// truncates every step id to its base job.
		re:    regexp.MustCompile(`Detected (?P<count>\d+) oom_kill event in StepId=(?P<job>\S+?)\.\s`),
		attrs: map[string]string{},
	},
	{
		name:  "slurm.slurmstepd.time_limit",
		class: schema.ClassResourceWalltimeExceeded,
		re:    regexp.MustCompile(`\*\*\* JOB (?P<job>\d+) ON \S+ CANCELLED AT \S+ DUE TO TIME LIMIT \*\*\*`),
		attrs: map[string]string{},
	},
	{
		name:  "slurm.slurmstepd.step_time_limit",
		class: schema.ClassResourceWalltimeExceeded,
		re:    regexp.MustCompile(`\*\*\* (?:STEP|JOB) (?P<job>\S+) ON \S+ CANCELLED AT \S+ DUE TO STEP TIME LIMIT \*\*\*`),
		attrs: map[string]string{},
	},

	// ---- munge / auth ----
	{
		name:  "munge.expired_credential",
		class: schema.ClassAuthMunge,
		re:    regexp.MustCompile(`^Expired credential`),
		derive: func(map[string]string) map[string]string {
			return map[string]string{"reason": "Expired credential", "daemon": "munged"}
		},
	},
	{
		name:  "slurm.slurmd.munge_decode_failed",
		class: schema.ClassAuthMunge,
		re:    regexp.MustCompile(`Munge decode failed: (?P<reason>.+)$`),
		attrs: map[string]string{"reason": "reason"},
		derive: func(map[string]string) map[string]string {
			return map[string]string{"daemon": "slurmd"}
		},
	},
	{
		name:  "slurm.slurmd.register_auth_error",
		class: schema.ClassAuthMunge,
		re:    regexp.MustCompile(`Unable to register: (?P<reason>.*authentication error.*)$`),
		attrs: map[string]string{"reason": "reason"},
		derive: func(map[string]string) map[string]string {
			return map[string]string{"daemon": "slurmd"}
		},
	},
	{
		name:  "slurm.protocol_auth_error",
		class: schema.ClassAuthMunge,
		re:    regexp.MustCompile(`has authentication error: (?P<reason>.+)$`),
		attrs: map[string]string{"reason": "reason"},
	},

	// ---- clock ----
	{
		name:  "chrony.system_clock_wrong",
		class: schema.ClassConfigClockSkew,
		re:    regexp.MustCompile(`System clock wrong by (?P<skew>-?[\d.]+) seconds`),
		derive: func(m map[string]string) map[string]string {
			return map[string]string{
				"skew_sec":  truncateSeconds(m["skew"]),
				"reference": "chrony",
				"daemon":    "chronyd",
			}
		},
	},
	{
		name:  "ntp.step_threshold_exceeded",
		class: schema.ClassConfigClockSkew,
		re:    regexp.MustCompile(`time (?:reset|step) (?P<skew>-?[\d.]+) s`),
		derive: func(m map[string]string) map[string]string {
			return map[string]string{"skew_sec": truncateSeconds(m["skew"]), "reference": "ntp", "daemon": "ntpd"}
		},
	},

	// ---- slurmctld: node state ----
	{
		name:  "slurm.slurmctld.node_set_down",
		class: schema.ClassSchedNodeFail,
		re:    regexp.MustCompile(`Nodes (?P<node>\S+) not responding, setting DOWN`),
		derive: func(map[string]string) map[string]string {
			return map[string]string{"reason": "Not responding", "state": "DOWN*"}
		},
	},
	{
		name:  "slurm.slurmctld.node_not_responding",
		class: schema.ClassSchedNodeFail,
		re:    regexp.MustCompile(`Nodes (?P<node>\S+) not responding$`),
		derive: func(map[string]string) map[string]string {
			return map[string]string{"reason": "Not responding"}
		},
	},

	// ---- InfiniBand ----
	{
		name:   "mlx5.port_module_event.cable_unplugged",
		class:  schema.ClassFabricLinkFlap,
		re:     regexp.MustCompile(`mlx5_port_module_event.*Cable unplugged`),
		derive: func(map[string]string) map[string]string { return nil },
	},
	{
		name:  "mlx5.ib_event.link_down",
		class: schema.ClassFabricLinkFlap,
		re:    regexp.MustCompile(`infiniband (?P<device>\S+):.*Port (?P<port>\d+) link down`),
		attrs: map[string]string{"device": "device", "port": "port"},
		derive: func(map[string]string) map[string]string {
			return map[string]string{"link_state": "Down"}
		},
	},
	{
		name:  "mlx5.ib_event.link_up",
		class: schema.ClassFabricLinkFlap,
		re:    regexp.MustCompile(`infiniband (?P<device>\S+):.*Port (?P<port>\d+) link up`),
		attrs: map[string]string{"device": "device", "port": "port"},
		derive: func(map[string]string) map[string]string {
			return map[string]string{"link_state": "Active"}
		},
	},

	// ---- GPU ----
	{
		name:  "nvidia.xid",
		class: schema.ClassGPUXid,
		re:    regexp.MustCompile(`NVRM: Xid \(PCI:(?P<pci>[0-9a-fA-F:.]+)\): (?P<xid>\d+)`),
		attrs: map[string]string{"pci": "pci_addr", "xid": "xid"},
	},
	{
		name:  "nvidia.fallen_off_bus",
		class: schema.ClassGPUFallenOffBus,
		re:    regexp.MustCompile(`GPU has fallen off the bus`),
	},

	// ---- storage ----
	{
		name:  "lustre.estale",
		class: schema.ClassStorageStaleHandle,
		re:    regexp.MustCompile(`Lustre:.*Stale file handle`),
		derive: func(map[string]string) map[string]string {
			return map[string]string{"fs_type": "lustre", "errno": "ESTALE"}
		},
	},
	{
		name:  "nfs.server_not_responding",
		class: schema.ClassStorageIOError,
		re:    regexp.MustCompile(`nfs: server (?P<server>\S+) not responding`),
		derive: func(map[string]string) map[string]string {
			return map[string]string{"fs_type": "nfs", "errno": "ETIMEDOUT"}
		},
	},
}

func (c *Collector) Collect(ctx context.Context, env collectors.Env, req collectors.Request) collectors.Result {
	res := collectors.Result{Source: schema.SourceJournal}

	// journald first; the slurm daemon logs are read separately because on most
	// sites they are files rather than journal units.
	//
	// The read is always bounded. A login node's journal is routinely millions of
	// lines covering a year, and slurping it to answer a question about one job
	// is slow enough that nobody would run the tool twice.
	args := []string{"-o", "short-iso", "--no-pager"}
	bounded := ""
	if !req.Window.Start.IsZero() {
		args = append(args, "--since", req.Window.Start.UTC().Format("2006-01-02 15:04:05"), "--utc")
		if !req.Window.End.IsZero() {
			args = append(args, "--until", req.Window.End.UTC().Format("2006-01-02 15:04:05"))
		}
	} else {
		// No window: fall back to a line cap rather than a time cap, so the bound
		// needs no clock and stays reproducible.
		args = append(args, "--lines", strconv.Itoa(MaxLinesUnbounded))
		bounded = fmt.Sprintf("no window given, so only the most recent %d lines were read",
			MaxLinesUnbounded)
	}

	out, err := env.Run(ctx, "journalctl", args...)
	cap := collectors.Capability{
		Name:  "journalctl",
		Level: collectors.LevelUnprivileged,
		Reveals: "kernel OOM kills, GPU Xid errors, fabric link transitions, and munge failures — " +
			"most of what explains a failure the scheduler only records as FAILED",
	}
	if err != nil {
		if errors.Is(err, collectors.ErrNotFound) {
			cap.Detail = "journalctl not available"
		} else {
			cap.Detail = err.Error()
		}
	} else {
		cap.Available = true
		cap.Detail = bounded
		evs, warns := parseLines(out, req.Cluster, env.Hostname(), env.Location())
		res.Events = append(res.Events, evs...)
		res.Warnings = append(res.Warnings, warns...)
	}
	res.Capabilities = append(res.Capabilities, cap)

	// An unprivileged user usually sees their own units and the kernel ring, but
	// not other users' — so a partial journal is the normal case, not an error.
	// It is reported so `doctor` can say so rather than implying completeness.
	for _, log := range []string{"slurmd.log", "slurmctld.log"} {
		data, err := env.ReadFile("/var/log/slurm/" + log)
		lcap := collectors.Capability{
			Name:    "log:" + log,
			Level:   collectors.LevelPrivileged,
			Reveals: "the daemon's own view: node state transitions, requeue decisions, authentication failures",
		}
		if err != nil {
			lcap.Detail = "not readable"
			res.Capabilities = append(res.Capabilities, lcap)
			continue
		}
		lcap.Available = true
		res.Capabilities = append(res.Capabilities, lcap)
		evs, warns := parseLines(data, req.Cluster, env.Hostname(), env.Location())
		res.Events = append(res.Events, evs...)
		res.Warnings = append(res.Warnings, warns...)
	}

	schema.SortEvents(res.Events)
	return res
}

// lineHead splits a log line into timestamp, host, unit, and message.
//
// Two shapes are handled: journalctl -o short-iso, and the bracketed timestamps
// the Slurm daemons write to their own logs.
var (
	// The offset may be written -0400 or -04:00. systemd emits the colon form,
	// and accepting only the other one made every real journal line unparseable
	// while the fixtures — which happened to use +0000 — passed. Captured
	// output is not a substitute for running against a live host.
	journalLine = regexp.MustCompile(
		`^(?P<ts>\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:[+-]\d{2}:?\d{2}|Z)?)\s+` +
			`(?P<host>\S+)\s+(?P<unit>[^:\[]+)(?:\[(?P<pid>\d+)\])?:\s*(?P<msg>.*)$`)
	slurmLogLine = regexp.MustCompile(
		`^\[(?P<ts>\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?)\]\s*(?P<msg>.*)$`)
)

func parseLines(data []byte, cluster schema.ClusterName, host string, loc *time.Location) ([]schema.Event, []string) {
	var events []schema.Event

	// Warnings are aggregated by kind rather than emitted per line. A journal on
	// a login node is millions of lines and most of them will never match a
	// signature; one warning each is not a miss log, it is a memory leak that
	// buries the handful of findings that matter.
	var unparsedLines, unparsedFirst int
	var badJobIDs, badJobFirst int
	var badJobExample string

	for i, line := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		ts, node, unit, msg, ok := splitLine(line, host, loc)
		if !ok {
			if unparsedLines == 0 {
				unparsedFirst = i + 1
			}
			unparsedLines++
			continue
		}
		if cluster == "" {
			continue
		}

		matched := false
		for _, sig := range signatures {
			m := match(sig.re, msg)
			if m == nil {
				continue
			}
			matched = true

			attrs := map[string]string{}
			for group, key := range sig.attrs {
				if v := m[group]; v != "" {
					attrs[key] = v
				}
			}
			if sig.derive != nil {
				for k, v := range sig.derive(m) {
					if v != "" {
						attrs[k] = v
					}
				}
			}
			// A "severity: error" marker on the line is worth keeping; the text
			// after it is not.
			if strings.HasPrefix(msg, "error:") || strings.Contains(unit, "error") {
				attrs["severity"] = "error"
			}

			// A line may name the job it concerns. That is how a journal event
			// acquires a jobid at all — most of them never do, and the join has
			// to attach those by node and time (§6).
			var job *schema.JobID
			if raw := m["job"]; raw != "" {
				if j, err := schema.ParseJobID(raw); err == nil {
					job = j
				} else {
					if badJobIDs == 0 {
						badJobFirst, badJobExample = i+1, raw
					}
					badJobIDs++
				}
			}
			evNode := node
			if v := m["node"]; v != "" {
				evNode = v
			}

			for k := range attrs {
				if !schema.AttrAllowed(sig.class, k) {
					delete(attrs, k)
				}
			}
			if len(attrs) == 0 {
				attrs = nil
			}

			events = append(events, schema.Event{
				TS:      ts,
				Cluster: cluster,
				Node:    schema.Hostname(evNode),
				JobID:   job,
				Source:  schema.SourceJournal,
				Class:   sig.class,
				Detail:  schema.Detail{Signature: sig.name, Attrs: attrs},
			})
			break
		}
		_ = matched
	}

	var warnings []string
	if unparsedLines > 0 {
		warnings = append(warnings, fmt.Sprintf(
			"%d line(s) did not parse as a log line at all (first at line %d). "+
				"Most journal traffic is unrelated to cairn, so a large number here is "+
				"normal; a number close to the total line count means the log format "+
				"is one cairn does not recognize.", unparsedLines, unparsedFirst))
	}
	if badJobIDs > 0 {
		warnings = append(warnings, fmt.Sprintf(
			"%d unparseable job id(s), first %q at line %d", badJobIDs, badJobExample, badJobFirst))
	}
	return events, warnings
}

func splitLine(line, host string, loc *time.Location) (ts time.Time, node, unit, msg string, ok bool) {
	if m := match(journalLine, line); m != nil {
		t, err := parseJournalTime(m["ts"], loc)
		if err != nil {
			return time.Time{}, "", "", "", false
		}
		return t, m["host"], strings.TrimSpace(m["unit"]), strings.TrimSpace(m["msg"]), true
	}
	if m := match(slurmLogLine, line); m != nil {
		t, err := parseJournalTime(m["ts"], loc)
		if err != nil {
			return time.Time{}, "", "", "", false
		}
		return t, host, "", strings.TrimSpace(m["msg"]), true
	}
	return time.Time{}, "", "", "", false
}

// parseJournalTime interprets a log timestamp.
//
// A timestamp carrying an explicit offset is authoritative and is used as given.
// One without an offset is interpreted in loc — never assumed to be UTC. The
// Slurm daemons write local time with no zone, so assuming UTC would shift every
// slurmctld event by the site's offset and misorder it against journald, which
// does carry an offset. That failure is invisible: nothing errors, the join is
// simply wrong.
func parseJournalTime(s string, loc *time.Location) (time.Time, error) {
	if loc == nil {
		loc = time.UTC
	}
	for _, layout := range []string{
		"2006-01-02T15:04:05Z07:00",        // -04:00
		"2006-01-02T15:04:05.999999Z07:00", // -04:00 with fraction
		"2006-01-02T15:04:05-0700",         // -0400
		"2006-01-02T15:04:05.999999-0700",
	} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC(), nil
		}
	}
	for _, layout := range []string{
		"2006-01-02T15:04:05.999999",
		"2006-01-02T15:04:05",
	} {
		if t, err := time.ParseInLocation(layout, s, loc); err == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("unrecognized timestamp %q", s)
}

func match(re *regexp.Regexp, s string) map[string]string {
	m := re.FindStringSubmatch(s)
	if m == nil {
		return nil
	}
	out := make(map[string]string, len(m))
	for i, name := range re.SubexpNames() {
		if name != "" && i < len(m) {
			out[name] = m[i]
		}
	}
	return out
}

func kbToBytes(s string) string {
	n, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return ""
	}
	return strconv.FormatUint(n*1024, 10)
}

// truncateSeconds keeps whole seconds. Sub-second precision on a clock-skew
// reading never changes what an admin does about it.
func truncateSeconds(s string) string {
	if i := strings.Index(s, "."); i >= 0 {
		s = s[:i]
	}
	if s == "" || s == "-" {
		return ""
	}
	return s
}
