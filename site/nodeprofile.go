package site

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/touchelos/cairn/collectors"
	"github.com/touchelos/cairn/schema"
)

// CaptureNode records this node's drift keys.
//
// The key set is CLAUDE.md §7's list: driver generation, kernel cmdline, glibc,
// module tree, mount set, munge key mtime. These are the things Ansible
// enforces and nothing measures — the class of failure that stays invisible
// until jobs die on one node and not its 47 siblings.
//
// Like Discover, it never returns an error and reads only through Env, so it
// replays against a fixture. Unlike Discover it is node-scoped: nothing here
// describes the cluster, because the whole point is to compare one node's
// answer to another's.
func CaptureNode(ctx context.Context, env collectors.Env, cluster schema.ClusterName,
	node schema.Hostname, now time.Time) NodeProfile {

	np := NodeProfile{
		Version:    ProfileVersion,
		Cluster:    cluster,
		Node:       node,
		CapturedAt: now.UTC(),
		Keys:       map[string]string{},
	}

	// The probes are shared with Discover rather than reimplemented. One parser
	// per producer means a fix to nvidia-smi parsing lands in both paths, and
	// that a node profile and a site profile can never disagree about the same
	// fact on the same host.
	var scratch Profile
	os := probeOS(ctx, env, &scratch)
	gpu := probeGPU(ctx, env, &scratch)
	mounts := probeMounts(env, &scratch)
	mods := probeModules(env, &scratch)
	_, ports := probeFabric(ctx, env, &scratch)
	np.Probes = scratch.Probes

	set := func(k, v string) {
		if v != "" {
			np.Keys[k] = v
		}
	}

	set(KeyOSID, os.ID)
	set(KeyOSVersion, os.VersionID)
	set(KeyKernelRelease, os.KernelRelease)
	set(KeyGlibcVersion, os.GlibcVersion)

	if gpu.Vendor == "nvidia" {
		set(KeyDriverVersion, gpu.DriverVersion)
		set(KeyCUDAVersion, gpu.CUDAVersion)
	}

	set(KeyModuleRoots, strings.Join(mods.Roots, ":"))

	if data, err := env.ReadFile("/proc/cmdline"); err == nil {
		set(KeyKernelCmdline, normalizeCmdline(string(data)))
	}

	// The munge key's mtime, at day granularity.
	//
	// The signal is a node that missed a key rotation — its key is days or
	// months older than the fleet's. Recording the exact second would instead
	// report drift every time config management wrote the file a few seconds
	// apart across a fleet, which is normal and means nothing. Day granularity
	// keeps the failure visible and drops the noise.
	//
	// stat, not read: /etc/munge/munge.key is 0400 munge:munge, so this works
	// unprivileged, and reading key material is something cairn must never do
	// (fixtures/README.md: key material is deleted, never redacted).
	if mtime, err := env.Stat("/etc/munge/munge.key"); err == nil {
		set(KeyMungeKeyMtime, mtime.UTC().Format("2006-01-02"))
	} else {
		np.Probes = append(np.Probes, Probe{
			Name:  "munge_key",
			Level: LevelUnprivileged,
			Detail: "/etc/munge/munge.key not present; this node does not run munge, " +
				"or the path is non-standard",
			Reveals: "whether this node missed a munge key rotation, which presents as authentication failures that look like a network problem",
		})
	}

	for _, m := range mounts {
		set(KeyMountPrefix+m.Mountpoint, m.FSType+" "+m.Source)
	}

	// Port state, per port. Recorded individually rather than as one digest so
	// that diff names the port that diverged: "mlx5_1 is Down while four
	// siblings have it Active" is actionable, "the fabric differs" is not.
	for _, port := range ports {
		set(KeyFabricPortPrefix+port.ID(), port.Summary())
	}

	sort.SliceStable(np.Probes, func(i, j int) bool { return np.Probes[i].Name < np.Probes[j].Name })
	return np
}

// normalizeCmdline canonicalizes /proc/cmdline for comparison.
//
// Sorted, because the kernel preserves the order the bootloader supplied and
// two nodes with identical parameters in a different order are not drifted.
//
// Per-node parameters are dropped rather than compared: root=, resume= and the
// rd.* device references legitimately differ on every machine, so keeping them
// would report drift on every node against every other node and bury the real
// findings. What survives is the part a site intends to be uniform — hugepages,
// isolcpus, iommu, mitigations, and the kernel image itself.
func normalizeCmdline(s string) string {
	var out []string
	for _, tok := range strings.Fields(s) {
		key, _, _ := strings.Cut(tok, "=")
		switch {
		case key == "root", key == "resume", key == "rootflags", key == "ro", key == "rw":
			continue
		case strings.HasPrefix(key, "rd."):
			continue
		}
		out = append(out, tok)
	}
	sort.Strings(out)
	return strings.Join(out, " ")
}
