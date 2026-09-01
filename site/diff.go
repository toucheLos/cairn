package site

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/touchelos/cairn/schema"
)

// MinPeers is the fewest siblings a fleet comparison will accept.
//
// Below this, "the majority" is not a fleet norm — it is one other machine.
// Reporting drift against a single peer would produce a confident-looking claim
// from a coin flip, and the admin has no way to tell that from a real finding.
// cairn refuses and says why (CLAUDE.md §7: divergence from 47 peers is the
// signal, not a threshold someone guessed).
const MinPeers = 3

// Absent is the recorded value for a key a node does not have.
//
// A missing key is drift, and often the most important kind: the node that
// lost /scratch is exactly the node whose jobs are failing. Treating absence as
// "no data" rather than a value would silently exclude it.
const Absent = "(absent)"

// Drift is one key on which a node diverges from its siblings.
type Drift struct {
	Key          string
	Observed     string
	Expected     string
	PeerCount    int
	PeerMajority int
}

// DiffResult is the outcome of comparing a node to its fleet.
type DiffResult struct {
	Node    schema.Hostname
	Cluster schema.ClusterName
	Peers   []schema.Hostname
	Drifts  []Drift

	// Refused is set when the comparison was not attempted, and says why.
	Refused string

	// Undecided lists keys where the fleet itself has no majority.
	//
	// Reported rather than dropped: a fleet split evenly across two driver
	// versions is a real and interesting state, and it is not the target node's
	// problem. Calling the larger group "expected" would invent a norm.
	Undecided []string

	// Oldest and Newest bound the capture times of the profiles compared.
	Oldest, Newest time.Time
}

// CaptureSpread reports the interval between the oldest and newest profile.
//
// Surfaced because a fleet comparison across profiles captured a week apart is
// not a fleet comparison: a node "drifted" from siblings profiled before the
// last reboot has told you nothing. The number goes in the output so the reader
// can judge it, rather than being silently folded into the verdict.
func (d DiffResult) CaptureSpread() time.Duration {
	if d.Oldest.IsZero() || d.Newest.IsZero() {
		return 0
	}
	return d.Newest.Sub(d.Oldest)
}

// Compare diffs one node against its siblings.
//
// It reports divergence, never a verdict. The majority is not necessarily
// correct — a freshly patched node diverging from 47 unpatched ones is the only
// correct machine in the room, and cairn has no way to know which case it is
// looking at. Every consumer of this result has to preserve that distinction;
// the moment it renders as "node-0046 is unhealthy" the tool is asserting
// something it cannot support.
func Compare(target NodeProfile, peers []NodeProfile) DiffResult {
	res := DiffResult{Node: target.Node, Cluster: target.Cluster}

	// A profile of the target itself in the peer set would vote for its own
	// value, dragging the majority toward the node under test.
	var siblings []NodeProfile
	for _, p := range peers {
		if p.Node == target.Node {
			continue
		}
		siblings = append(siblings, p)
	}
	sort.Slice(siblings, func(i, j int) bool { return siblings[i].Node < siblings[j].Node })
	for _, s := range siblings {
		res.Peers = append(res.Peers, s.Node)
	}

	res.Oldest, res.Newest = captureBounds(append(siblings, target))

	if len(siblings) < MinPeers {
		res.Refused = fmt.Sprintf(
			"%d sibling profile(s); at least %d are needed before a majority means anything",
			len(siblings), MinPeers)
		return res
	}

	// Every key seen anywhere, so a key the target lacks is still compared.
	keys := map[string]bool{}
	for k := range target.Keys {
		keys[k] = true
	}
	for _, s := range siblings {
		for k := range s.Keys {
			keys[k] = true
		}
	}
	ordered := make([]string, 0, len(keys))
	for k := range keys {
		ordered = append(ordered, k)
	}
	sort.Strings(ordered)

	for _, key := range ordered {
		tally := map[string]int{}
		for _, s := range siblings {
			tally[valueOf(s, key)]++
		}
		expected, count, tied := majority(tally, len(siblings))
		if tied {
			res.Undecided = append(res.Undecided, key)
			continue
		}
		observed := valueOf(target, key)
		if observed == expected {
			continue
		}
		res.Drifts = append(res.Drifts, Drift{
			Key:          key,
			Observed:     observed,
			Expected:     expected,
			PeerCount:    len(siblings),
			PeerMajority: count,
		})
	}
	return res
}

func valueOf(n NodeProfile, key string) string {
	if v, ok := n.Keys[key]; ok && v != "" {
		return v
	}
	return Absent
}

// majority returns the value held by more than half the peers.
//
// A strict majority, not a plurality. With three values split 40/35/25 there is
// no fleet norm to diverge from, and naming the 40% "expected" would
// manufacture one — so the key is reported as undecided instead.
func majority(tally map[string]int, peers int) (value string, count int, tied bool) {
	// Sorted so a tie resolves identically on every run (§2.7).
	vals := make([]string, 0, len(tally))
	for v := range tally {
		vals = append(vals, v)
	}
	sort.Strings(vals)

	for _, v := range vals {
		if tally[v] > count {
			value, count = v, tally[v]
		}
	}
	if count*2 <= peers {
		return "", 0, true
	}
	return value, count, false
}

func captureBounds(ps []NodeProfile) (oldest, newest time.Time) {
	for _, p := range ps {
		if p.CapturedAt.IsZero() {
			continue
		}
		if oldest.IsZero() || p.CapturedAt.Before(oldest) {
			oldest = p.CapturedAt
		}
		if p.CapturedAt.After(newest) {
			newest = p.CapturedAt
		}
	}
	return oldest, newest
}

// Events renders the drifts as schema events.
//
// The timestamp is the target's capture time, not the moment diff ran: the
// observation is "this is what the node looked like when profiled", and using
// the wall clock would make two runs over the same profiles produce different
// bundles, which invariant §2.7 forbids.
func (d DiffResult) Events() ([]schema.Event, error) {
	var out []schema.Event
	for _, dr := range d.Drifts {
		e := schema.Event{
			TS:      d.Newest,
			Cluster: d.Cluster,
			Node:    d.Node,
			Source:  schema.SourceSite,
			Class:   schema.ClassConfigDrift,
			Detail: schema.Detail{
				Signature: "site.drift." + dr.Key,
				Attrs: map[string]string{
					"key":           dr.Key,
					"observed":      dr.Observed,
					"expected":      dr.Expected,
					"peer_count":    strconv.Itoa(dr.PeerCount),
					"peer_majority": strconv.Itoa(dr.PeerMajority),
				},
			},
		}
		if err := e.Validate(); err != nil {
			return nil, fmt.Errorf("drift on %s: %w", dr.Key, err)
		}
		out = append(out, e)
	}
	schema.SortEvents(out)
	return out, nil
}

// ProfileDiff is one field on which a recorded profile and a fresh probe
// disagree.
type ProfileDiff struct {
	Key      string
	Recorded string
	Probed   string
}

// CompareProfiles reports where a committed site.yaml and a fresh probe differ.
//
// This is what `cairn init` shows instead of overwriting. It compares the
// discovered facts only — not the probe records, which are a report about one
// run rather than a claim about the site, and would otherwise fill the diff with
// noise every time a transient tool was missing.
func CompareProfiles(recorded, probed Profile) []ProfileDiff {
	var out []ProfileDiff
	cmp := func(key, a, b string) {
		if a != b {
			out = append(out, ProfileDiff{Key: key, Recorded: a, Probed: b})
		}
	}
	joinList := func(v []string) string { return strings.Join(v, ",") }

	cmp("cluster", string(recorded.Cluster), string(probed.Cluster))
	cmp("scheduler.kind", recorded.Scheduler.Kind, probed.Scheduler.Kind)
	cmp("scheduler.version", recorded.Scheduler.Version, probed.Scheduler.Version)
	cmp("scheduler.config_path", recorded.Scheduler.ConfigPath, probed.Scheduler.ConfigPath)
	cmp("scheduler.partitions", joinList(recorded.Scheduler.Partitions), joinList(probed.Scheduler.Partitions))
	cmp("scheduler.qos", joinList(recorded.Scheduler.QOS), joinList(probed.Scheduler.QOS))
	cmp("modules.kind", recorded.Modules.Kind, probed.Modules.Kind)
	cmp("modules.roots", joinList(recorded.Modules.Roots), joinList(probed.Modules.Roots))
	cmp("os.id", recorded.OS.ID, probed.OS.ID)
	cmp("os.version_id", recorded.OS.VersionID, probed.OS.VersionID)
	cmp("os.kernel_release", recorded.OS.KernelRelease, probed.OS.KernelRelease)
	cmp("os.glibc_version", recorded.OS.GlibcVersion, probed.OS.GlibcVersion)
	cmp("fabric.kind", recorded.Fabric.Kind, probed.Fabric.Kind)
	cmp("fabric.hcas", joinList(recorded.Fabric.HCAs), joinList(probed.Fabric.HCAs))
	cmp("gpu.vendor", recorded.GPU.Vendor, probed.GPU.Vendor)
	cmp("gpu.driver_version", recorded.GPU.DriverVersion, probed.GPU.DriverVersion)
	cmp("gpu.cuda_version", recorded.GPU.CUDAVersion, probed.GPU.CUDAVersion)
	cmp("gpu.models", joinList(recorded.GPU.Models), joinList(probed.GPU.Models))
	cmp("bmc.kind", recorded.BMC.Kind, probed.BMC.Kind)

	var rb, pb []string
	for _, b := range recorded.Builders {
		rb = append(rb, b.Kind+"="+b.Root)
	}
	for _, b := range probed.Builders {
		pb = append(pb, b.Kind+"="+b.Root)
	}
	cmp("builders", joinList(rb), joinList(pb))

	var rm, pm []string
	for _, m := range recorded.Mounts {
		rm = append(rm, m.Mountpoint+"("+m.FSType+")")
	}
	for _, m := range probed.Mounts {
		pm = append(pm, m.Mountpoint+"("+m.FSType+")")
	}
	cmp("mounts", joinList(rm), joinList(pm))

	return out
}
