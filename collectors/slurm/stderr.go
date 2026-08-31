package slurm

import (
	"context"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/touchelos/cairn/collectors"
	"github.com/touchelos/cairn/schema"
)

// A job's stderr is where two of the most expensive failure modes announce
// themselves and nowhere else: a CUDA driver/runtime mismatch, and a NCCL
// collective watchdog timeout. Neither appears in sacct, and neither reaches
// journald. A collector that reads only the scheduler's own record cannot
// diagnose either, which on a rented GPU fleet is the difference between a
// two-hour abort and a forty-hour one (CLAUDE.md §8).
//
// Only the job's own stderr is read, and only signature matches are emitted.
// The file is never stored, quoted, or excerpted (§2.3).

var stdErrPath = regexp.MustCompile(`(?m)^\s*StdErr=(?P<path>\S+)\s*$`)

// nccl records what a NCCL communicator announced at init, so that a later
// timeout line can be reported with the communicator's size.
//
// Stateful on purpose: rank and world size are printed once at init, and the
// timeout is printed somewhere else entirely, often thousands of lines later.
// Matching each line in isolation would emit a timeout that cannot say how large
// the collective was — which is most of what distinguishes a straggler rank from
// a fabric fault.
type nccl struct {
	worldSize string
	commID    string
}

var (
	ncclInit = regexp.MustCompile(
		`NCCL INFO comm \S+ rank (?P<rank>\d+) nranks (?P<nranks>\d+).*commId (?P<comm>\S+)`)
	ncclTimeout = regexp.MustCompile(
		`(?:\[rank(?P<rank>\d+)\][:\s].*)?Watchdog caught collective operation timeout: ` +
			`WorkNCCL\(.*?OpType=(?P<op>\w+).*?Timeout\(ms\)=(?P<timeout>\d+)`)
	cudaInsufficient = regexp.MustCompile(
		`CUDA driver version is insufficient for CUDA runtime version`)
	cudaModule = regexp.MustCompile(`(?i)\bcuda[/-](?P<ver>\d+\.\d+)`)
)

// collectStdErr reads the job's stderr, if it can find it, and matches
// signatures against it.
//
// at is the timestamp events are stamped with. Job stderr carries no timestamps
// of its own, so the job's end time from sacct is the only clock available. That
// places these events at the end of the job rather than at the moment the
// condition arose, which is imprecise and recorded as such — a NCCL watchdog
// that fired thirty minutes before the job aborted is reported at the abort.
// Inventing a more precise time from arithmetic over the timeout duration would
// look better and be no more true.
func (c *Collector) collectStdErr(
	ctx context.Context, env collectors.Env, req collectors.Request, at time.Time,
) ([]schema.Event, []collectors.Capability, []string) {

	cap := collectors.Capability{
		Name:  "job-stderr",
		Level: collectors.LevelUnprivileged,
		Reveals: "CUDA driver/runtime mismatches and NCCL collective timeouts — " +
			"failures that appear nowhere in sacct or journald",
	}
	if req.Job == nil || at.IsZero() {
		cap.Detail = "no job specified, or the job has no end time yet"
		return nil, []collectors.Capability{cap}, nil
	}

	out, err := env.Run(ctx, "scontrol", "show", "job", req.Job.Raw)
	if err != nil {
		// scontrol forgets a job minutes after it ends, so this is the normal
		// outcome for anything but a very recent failure — reported, not an error.
		cap.Detail = "scontrol could not locate the job; StdErr path unknown " +
			"(scontrol retains jobs only briefly after they end)"
		return nil, []collectors.Capability{cap}, nil
	}
	m := stdErrPath.FindStringSubmatch(string(out))
	if m == nil {
		cap.Detail = "scontrol reported no StdErr path for this job"
		return nil, []collectors.Capability{cap}, nil
	}

	data, err := env.ReadFile(m[1])
	if err != nil {
		cap.Detail = "stderr file not readable; it may live on a filesystem this node cannot see"
		return nil, []collectors.Capability{cap}, nil
	}
	cap.Available = true

	events, warnings := parseStdErr(data, req, at)
	return events, []collectors.Capability{cap}, warnings
}

func parseStdErr(data []byte, req collectors.Request, at time.Time) ([]schema.Event, []string) {
	var events []schema.Event
	var warnings []string

	comms := map[string]nccl{}
	var cudaVer string
	node := schema.Hostname("")
	if len(req.Nodes) == 1 {
		node = req.Nodes[0]
	}

	emit := func(class schema.Class, sig string, attrs map[string]string) {
		for k := range attrs {
			if attrs[k] == "" || !schema.AttrAllowed(class, k) {
				delete(attrs, k)
			}
		}
		if len(attrs) == 0 {
			attrs = nil
		}
		events = append(events, schema.Event{
			TS: at, Cluster: req.Cluster, Node: node, JobID: req.Job,
			Source: schema.SourceSlurm, Class: class,
			Detail: schema.Detail{Signature: sig, Attrs: attrs},
		})
	}

	for _, line := range strings.Split(string(data), "\n") {
		if m := matchMap(ncclInit, line); m != nil {
			comms[m["rank"]] = nccl{worldSize: m["nranks"], commID: m["comm"]}
			continue
		}
		if m := matchMap(cudaModule, line); m != nil && cudaVer == "" {
			cudaVer = m["ver"]
		}
		if m := matchMap(ncclTimeout, line); m != nil {
			info := comms[m["rank"]]
			emit(schema.ClassFabricNCCLTimeout, "nccl.watchdog.collective_timeout", map[string]string{
				"rank":            m["rank"],
				"op":              m["op"],
				"timeout_seconds": msToSeconds(m["timeout"]),
				"world_size":      info.worldSize,
				"comm":            info.commID,
			})
			if info.worldSize == "" {
				warnings = append(warnings,
					"NCCL timeout reported for a rank whose communicator init was not in the captured stderr; "+
						"world size and communicator id are unknown")
			}
			continue
		}
		if cudaInsufficient.MatchString(line) {
			if cudaVer == "" {
				warnings = append(warnings,
					"CUDA driver/runtime mismatch reported but the requested runtime version was not "+
						"visible in stderr; the module load line may have gone to stdout")
			}
			emit(schema.ClassGPUDriverMismatch, "cuda.runtime.driver_insufficient", map[string]string{
				"cuda_version": cudaVer,
			})
		}
	}

	schema.SortEvents(events)
	return events, warnings
}

func msToSeconds(s string) string {
	n, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return ""
	}
	return strconv.FormatUint(n/1000, 10)
}

func matchMap(re *regexp.Regexp, s string) map[string]string {
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
