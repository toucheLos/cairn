// Package gpu reads nvidia-smi and DCGM.
//
// The header nvidia-smi prints carries a timestamp in the host's local time with
// no offset, which is why collectors.Env exposes Location: a collector that
// assumed UTC here would place every GPU event hours away from the journal
// events describing the same second.
package gpu

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

type Collector struct {
	// MinDriverForCUDA maps a CUDA major.minor to the lowest driver that
	// supports it. Used to recognize a driver/runtime mismatch.
	//
	// A table rather than an inference: the mapping is published by the vendor
	// and cannot be derived from anything nvidia-smi prints. Phase 3 replaces the
	// site-independent half of this with fleet-relative comparison (§7), which is
	// strictly better because it needs no table to go stale.
	MinDriverForCUDA map[string]string
}

func New() *Collector {
	return &Collector{MinDriverForCUDA: defaultMinDriver}
}

// defaultMinDriver covers the CUDA generations currently in the field.
// Incomplete by construction: an unlisted runtime yields no mismatch claim
// rather than a guess.
var defaultMinDriver = map[string]string{
	"11.8": "520.61.05",
	"12.0": "525.60.13",
	"12.1": "530.30.02",
	"12.2": "535.54.03",
	"12.3": "545.23.06",
	"12.4": "550.54.14",
	"12.5": "555.42.02",
	"12.6": "560.28.03",
}

func (c *Collector) Source() schema.Source { return schema.SourceGPU }

var (
	smiHeader = regexp.MustCompile(
		`^(?P<dow>\w{3})\s+(?P<mon>\w{3})\s+(?P<day>\d{1,2})\s+(?P<time>\d{2}:\d{2}:\d{2})\s+(?P<year>\d{4})`)
	smiVersions = regexp.MustCompile(
		`Driver Version:\s*(?P<driver>[\d.]+)\s+CUDA Version:\s*(?P<cuda>[\d.]+)`)
	smiDevice = regexp.MustCompile(
		`^\|\s+(?P<index>\d+)\s+(?P<name>.*?)\s{2,}(?:On|Off)\s+\|\s+(?P<pci>[0-9A-Fa-f]{8}:[0-9A-Fa-f]{2}:[0-9A-Fa-f]{2}\.[0-9A-Fa-f])`)
	smiEcc = regexp.MustCompile(`\|\s+(?P<used>\d+)MiB\s*/\s*(?P<total>\d+)MiB\s+\|\s+(?P<util>\d+)%`)
)

func (c *Collector) Collect(ctx context.Context, env collectors.Env, req collectors.Request) collectors.Result {
	res := collectors.Result{Source: schema.SourceGPU}

	out, err := env.Run(ctx, "nvidia-smi")
	cap := collectors.Capability{
		Name:    "nvidia-smi",
		Level:   collectors.LevelUnprivileged,
		Reveals: "driver and CUDA versions, device inventory, memory occupancy and utilization",
	}
	if err != nil {
		if errors.Is(err, collectors.ErrNotFound) {
			// The correct and complete answer on a CPU-only node. Invariant
			// §2.6: this is a capability report, not a failure.
			cap.Detail = "nvidia-smi not present; this node has no NVIDIA GPUs, or the driver is not loaded"
		} else {
			cap.Detail = err.Error()
		}
		res.Capabilities = append(res.Capabilities, cap)
		return res
	}
	cap.Available = true
	res.Capabilities = append(res.Capabilities, cap)

	evs, warns := c.parseSMI(out, req, env.Hostname(), env.Location())
	res.Events = append(res.Events, evs...)
	res.Warnings = append(res.Warnings, warns...)
	return res
}

func (c *Collector) parseSMI(out []byte, req collectors.Request, host string, loc *time.Location) ([]schema.Event, []string) {
	lines := strings.Split(strings.TrimRight(string(out), "\n"), "\n")

	var ts time.Time
	var driver, cuda string
	type device struct {
		index, pci string
		usedMiB    string
		totalMiB   string
		util       string
	}
	var devices []device
	var warnings []string

	for _, line := range lines {
		if ts.IsZero() {
			if m := matchNamed(smiHeader, line); m != nil {
				t, err := parseSMITime(m, loc)
				if err != nil {
					warnings = append(warnings, "unparseable nvidia-smi header timestamp")
				} else {
					ts = t
				}
				continue
			}
		}
		if driver == "" {
			if m := matchNamed(smiVersions, line); m != nil {
				driver, cuda = m["driver"], m["cuda"]
				continue
			}
		}
		if m := matchNamed(smiDevice, line); m != nil {
			devices = append(devices, device{index: m["index"], pci: strings.ToLower(m["pci"])})
			continue
		}
		if m := matchNamed(smiEcc, line); m != nil && len(devices) > 0 {
			d := &devices[len(devices)-1]
			d.usedMiB, d.totalMiB, d.util = m["used"], m["total"], m["util"]
			continue
		}
	}

	if ts.IsZero() {
		// Without a timestamp there is nothing to place on a timeline, and
		// inventing one from the wall clock would break §2.7.
		warnings = append(warnings, "nvidia-smi output carried no timestamp; no events emitted")
		return nil, warnings
	}
	if req.Cluster == "" {
		return nil, warnings
	}

	node := schema.Hostname(host)
	if len(req.Nodes) == 1 {
		node = req.Nodes[0]
	}

	var events []schema.Event

	// Driver / runtime mismatch. The claim is only made when the table knows the
	// runtime; an unlisted CUDA version produces nothing rather than a guess.
	if driver != "" && cuda != "" {
		if min, ok := c.MinDriverForCUDA[cuda]; ok && compareVersions(driver, min) < 0 {
			idx := "0"
			if len(devices) > 0 {
				idx = devices[0].index
			}
			events = append(events, schema.Event{
				TS: ts, Cluster: req.Cluster, Node: node,
				Source: schema.SourceGPU, Class: schema.ClassGPUDriverMismatch,
				Detail: schema.Detail{
					Signature: "nvidia.driver.runtime_version_mismatch",
					Attrs: map[string]string{
						"gpu_index":               idx,
						"driver_version":          driver,
						"cuda_version":            cuda,
						"expected_driver_version": min,
					},
				},
			})
		}
	}

	// Memory held with no work being done. This is the clearest single signature
	// of a collective hang, and cairn has no class for it — see fixture
	// 007-nccl-hang and the deferred question in schema/CHANGELOG.md. Recording
	// it as ClassUnknown keeps the evidence and keeps the gap visible; §2.6 is
	// what makes that an acceptable answer rather than a dropped observation.
	for _, d := range devices {
		if d.usedMiB == "" || d.util == "" {
			continue
		}
		used, err1 := strconv.Atoi(d.usedMiB)
		total, err2 := strconv.Atoi(d.totalMiB)
		util, err3 := strconv.Atoi(d.util)
		if err1 != nil || err2 != nil || err3 != nil || total == 0 {
			continue
		}
		if util == 0 && used*100/total >= 50 {
			events = append(events, schema.Event{
				TS: ts, Cluster: req.Cluster, Node: node,
				Source: schema.SourceGPU, Class: schema.ClassUnknown,
				Detail: schema.Detail{
					Signature: "nvidia.smi.allocated_but_idle",
					Attrs:     map[string]string{"severity": "warn"},
				},
			})
		}
	}

	schema.SortEvents(events)
	return events, warnings
}

// parseSMITime reads the "Wed Mar  4 09:31:14 2026" header.
func parseSMITime(m map[string]string, loc *time.Location) (time.Time, error) {
	if loc == nil {
		loc = time.UTC
	}
	s := fmt.Sprintf("%s %s %s %s", m["mon"], m["day"], m["time"], m["year"])
	t, err := time.ParseInLocation("Jan 2 15:04:05 2006", s, loc)
	if err != nil {
		return time.Time{}, err
	}
	return t.UTC(), nil
}

// compareVersions compares dotted numeric versions. Returns -1, 0, or 1.
func compareVersions(a, b string) int {
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(as) || i < len(bs); i++ {
		var av, bv int
		if i < len(as) {
			av, _ = strconv.Atoi(as[i])
		}
		if i < len(bs) {
			bv, _ = strconv.Atoi(bs[i])
		}
		if av != bv {
			if av < bv {
				return -1
			}
			return 1
		}
	}
	return 0
}

func matchNamed(re *regexp.Regexp, s string) map[string]string {
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
