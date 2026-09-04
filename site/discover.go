package site

import (
	"context"
	"regexp"
	"sort"
	"strings"

	"github.com/touchelos/cairn/collectors"
	"github.com/touchelos/cairn/collectors/fabric"
	"github.com/touchelos/cairn/schema"
)

// nvidia-smi header patterns.
//
// Duplicated from collectors/gpu rather than exported from it, and the
// duplication is the lesser evil: the gpu collector's copies are tuned to emit
// events and are covered by the fixture corpus, so exporting them would couple
// a discovery change to the classification path. These two read only the
// inventory line, which is a smaller claim.
var (
	smiVersions = regexp.MustCompile(`Driver Version:\s*([\d.]+)\s+CUDA Version:\s*([\d.]+)`)
	smiModel    = regexp.MustCompile(`(?m)^\|\s+\d+\s+(.*?)\s{2,}(?:On|Off)\s+\|`)
)

// Discover probes the stack and returns a profile.
//
// It never returns an error. Invariant §2.6: an unrecognized scheduler,
// filesystem or fabric is logged and skipped, never fatal — and `cairn init` on
// an unknown stack must still produce a usable file, because the admin
// correcting it is exactly the person who knows what cairn failed to recognize.
//
// Everything reads through collectors.Env, so the whole of discovery replays
// against a fixture directory with no cluster present. That is what lets the
// probes be tested at all.
func Discover(ctx context.Context, env collectors.Env, fallbackCluster schema.ClusterName) Profile {
	p := Profile{Version: ProfileVersion}

	p.Scheduler, p.Cluster = probeScheduler(ctx, env, &p)
	if p.Cluster == "" {
		p.Cluster = fallbackCluster
	}
	p.Modules = probeModules(env, &p)
	p.Builders = probeBuilders(env, &p)
	p.OS = probeOS(ctx, env, &p)
	p.Fabric, _ = probeFabric(ctx, env, &p)
	p.GPU = probeGPU(ctx, env, &p)
	p.Mounts = probeMounts(env, &p)
	p.BMC = probeBMC(env, &p)
	p.Metrics = probeMetrics(env, &p)

	sort.SliceStable(p.Probes, func(i, j int) bool { return p.Probes[i].Name < p.Probes[j].Name })
	return p
}

func add(p *Profile, pr Probe) { p.Probes = append(p.Probes, pr) }

// ---------- scheduler ----------

func probeScheduler(ctx context.Context, env collectors.Env, p *Profile) (Scheduler, schema.ClusterName) {
	pr := Probe{
		Name:  "scheduler",
		Level: LevelUnprivileged,
		Reveals: "the batch system and its version — without it cairn cannot name " +
			"partitions or QOS, and a model has nothing to stop it answering in the wrong dialect",
	}

	// Slurm first: it is what the Phase 1 collectors read, so a site running it
	// gets the fullest profile.
	if _, err := env.LookPath("scontrol"); err == nil {
		s := Scheduler{Kind: "slurm"}
		if out, err := env.Run(ctx, "scontrol", "--version"); err == nil {
			// "slurm 23.02.7"
			if f := strings.Fields(strings.TrimSpace(string(out))); len(f) >= 2 {
				s.Version = f[1]
			}
		}
		cluster := schema.ClusterName("")
		for _, path := range []string{"/etc/slurm/slurm.conf", "/etc/slurm-llnl/slurm.conf"} {
			data, err := env.ReadFile(path)
			if err != nil {
				continue
			}
			s.ConfigPath = path
			cluster = schema.ClusterName(slurmConfValue(data, "ClusterName"))
			break
		}
		s.Partitions = slurmPartitions(ctx, env)
		s.QOS = slurmQOS(ctx, env)

		pr.Available = true
		pr.Detail = "slurm"
		if s.Version != "" {
			pr.Detail = "slurm " + s.Version
		}
		if s.ConfigPath == "" {
			pr.Detail += "; slurm.conf not readable, so the cluster name is not from the scheduler"
		}
		add(p, pr)
		return s, cluster
	}

	for _, probe := range []struct{ bin, kind string }{
		{"qstat", "pbs"},
		{"bjobs", "lsf"},
	} {
		if _, err := env.LookPath(probe.bin); err != nil {
			continue
		}
		pr.Available = true
		pr.Detail = probe.kind + " detected via " + probe.bin +
			"; cairn has no collector for it yet, so no events will come from the scheduler"
		add(p, pr)
		return Scheduler{Kind: probe.kind}, ""
	}

	// §2.6: not an error. A login node with no scheduler client is a real
	// configuration, and so is a scheduler cairn has never heard of.
	pr.Detail = "no scheduler client found on PATH (looked for scontrol, qstat, bjobs)"
	add(p, pr)
	return Scheduler{Kind: "none"}, ""
}

// slurmConfValue reads a `Key=Value` from slurm.conf, ignoring case and comments.
func slurmConfValue(data []byte, key string) string {
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok || !strings.EqualFold(strings.TrimSpace(k), key) {
			continue
		}
		if i := strings.Index(v, "#"); i >= 0 {
			v = v[:i]
		}
		return strings.TrimSpace(v)
	}
	return ""
}

func slurmPartitions(ctx context.Context, env collectors.Env) []string {
	out, err := env.Run(ctx, "sinfo", "-h", "-o", "%R")
	if err != nil {
		return nil
	}
	return uniqueSorted(strings.Split(string(out), "\n"))
}

func slurmQOS(ctx context.Context, env collectors.Env) []string {
	out, err := env.Run(ctx, "sacctmgr", "-n", "-P", "show", "qos", "format=Name")
	if err != nil {
		return nil
	}
	return uniqueSorted(strings.Split(string(out), "\n"))
}

// ---------- modules ----------

func probeModules(env collectors.Env, p *Profile) Modules {
	pr := Probe{
		Name:    "modules",
		Level:   LevelUnprivileged,
		Reveals: "which modules this site publishes, and therefore whether a failing `module load` is a typo or a missing build",
	}

	m := Modules{Kind: "none"}
	switch {
	case env.Getenv("LMOD_CMD") != "" || env.Getenv("LMOD_PKG") != "":
		m.Kind = "lmod"
	case env.Getenv("MODULESHOME") != "":
		// Tcl Environment Modules sets MODULESHOME too, so this is only reached
		// when the Lmod-specific variables are absent.
		m.Kind = "tcl"
	}

	if mp := env.Getenv("MODULEPATH"); mp != "" {
		m.Roots = uniqueSorted(strings.Split(mp, ":"))
	}

	if m.Kind == "none" {
		pr.Detail = "no module system active in this shell ($LMOD_CMD, $MODULESHOME unset)"
		add(p, pr)
		return m
	}
	pr.Available = true
	pr.Detail = m.Kind
	if len(m.Roots) == 0 {
		pr.Detail += "; $MODULEPATH is unset, so no module roots were recorded"
	}
	add(p, pr)
	return m
}

func probeBuilders(env collectors.Env, p *Profile) []Builder {
	pr := Probe{
		Name:    "builders",
		Level:   LevelUnprivileged,
		Reveals: "where this site's software is built from, which is most of the answer to \"why is this library the wrong version\"",
	}

	var out []Builder
	if root := env.Getenv("SPACK_ROOT"); root != "" {
		out = append(out, Builder{Kind: "spack", Root: root})
	}
	if root := env.Getenv("EASYBUILD_INSTALLPATH"); root != "" {
		out = append(out, Builder{Kind: "easybuild", Root: root})
	}

	if len(out) == 0 {
		pr.Detail = "no build system active in this shell ($SPACK_ROOT, $EASYBUILD_INSTALLPATH unset)"
		add(p, pr)
		return nil
	}
	pr.Available = true
	var kinds []string
	for _, b := range out {
		kinds = append(kinds, b.Kind)
	}
	pr.Detail = strings.Join(kinds, ", ")
	add(p, pr)
	return out
}

// ---------- base system ----------

func probeOS(ctx context.Context, env collectors.Env, p *Profile) OSFacts {
	pr := Probe{
		Name:    "os",
		Level:   LevelUnprivileged,
		Reveals: "distro, kernel and glibc — the drift keys that stay invisible until a job dies on one node and not its siblings",
	}

	var o OSFacts
	if data, err := env.ReadFile("/etc/os-release"); err == nil {
		o.ID = osReleaseValue(data, "ID")
		o.VersionID = osReleaseValue(data, "VERSION_ID")
	}
	if data, err := env.ReadFile("/proc/sys/kernel/osrelease"); err == nil {
		o.KernelRelease = strings.TrimSpace(string(data))
	}
	if out, err := env.Run(ctx, "ldd", "--version"); err == nil {
		// "ldd (GNU libc) 2.39"
		first, _, _ := strings.Cut(string(out), "\n")
		if f := strings.Fields(strings.TrimSpace(first)); len(f) > 0 {
			o.GlibcVersion = f[len(f)-1]
		}
	}

	var missing []string
	if o.ID == "" {
		missing = append(missing, "/etc/os-release")
	}
	if o.KernelRelease == "" {
		missing = append(missing, "kernel release")
	}
	if o.GlibcVersion == "" {
		missing = append(missing, "glibc version")
	}
	if len(missing) == 0 {
		pr.Available = true
		pr.Detail = strings.TrimSpace(o.ID + " " + o.VersionID + ", kernel " + o.KernelRelease)
	} else {
		pr.Detail = "could not read " + strings.Join(missing, ", ")
	}
	add(p, pr)
	return o
}

func osReleaseValue(data []byte, key string) string {
	for _, line := range strings.Split(string(data), "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok || k != key {
			continue
		}
		return strings.Trim(strings.TrimSpace(v), `"'`)
	}
	return ""
}

// ---------- fabric ----------

// probeFabric returns the cluster-level fabric summary and the per-port state.
//
// The ports are returned separately rather than stored on Fabric because they
// are a fact about one machine, not about the cluster. site.yaml describes the
// site; a port being Down belongs in the node profile, where `cairn diff`
// compares it against the same port on the node's siblings.
func probeFabric(ctx context.Context, env collectors.Env, p *Profile) (Fabric, []fabric.Port) {
	pr := Probe{
		Name:  "fabric",
		Level: LevelUnprivileged,
		Reveals: "the interconnect and its link rates — without it a collective hang " +
			"cannot be told apart from a slow one, which is the whole of Track B attribution",
	}

	if _, err := env.LookPath("ibstat"); err != nil {
		pr.Detail = "ibstat not present; this host has no InfiniBand tooling, or the fabric is Ethernet"
		add(p, pr)
		return Fabric{Kind: "none"}, nil
	}
	out, err := env.Run(ctx, "ibstat")
	if err != nil {
		pr.Detail = "ibstat present but did not run: " + err.Error()
		add(p, pr)
		return Fabric{Kind: "none"}, nil
	}

	// One ibstat parser in the repo, shared with the collector. site/ had its
	// own until collectors/fabric existed; two parsers of one tool is how the
	// nvidia-smi header patterns ended up duplicated, and that is not a
	// precedent worth extending.
	ports, warns := fabric.ParseIbstat(out)
	f := Fabric{Kind: "infiniband"}
	for _, port := range ports {
		f.HCAs = append(f.HCAs, port.Device)
		f.Rates = append(f.Rates, port.Rate)
		if strings.EqualFold(port.LinkLayer, "Ethernet") {
			// A Mellanox card in Ethernet mode is RoCE, not InfiniBand, and the
			// difference decides whether perfquery means anything here.
			f.Kind = "roce"
		}
	}
	f.HCAs = uniqueSorted(f.HCAs)
	f.Rates = uniqueSorted(f.Rates)
	if len(warns) > 0 {
		pr.Detail = strings.Join(warns, "; ")
	}

	pr.Available = true
	pr.Detail = f.Kind
	if len(f.HCAs) > 0 {
		pr.Detail += ", " + strings.Join(f.HCAs, " ")
	}
	add(p, pr)
	return f, ports
}

// ---------- gpu ----------

func probeGPU(ctx context.Context, env collectors.Env, p *Profile) GPUFacts {
	pr := Probe{
		Name:    "gpu",
		Level:   LevelUnprivileged,
		Reveals: "driver and CUDA generation, and the device inventory a job is actually scheduled onto",
	}

	if _, err := env.LookPath("nvidia-smi"); err != nil {
		pr.Detail = "nvidia-smi not present; this is a CPU-only host, or the driver is not loaded"
		add(p, pr)
		return GPUFacts{Vendor: "none"}
	}
	out, err := env.Run(ctx, "nvidia-smi")
	if err != nil {
		pr.Detail = "nvidia-smi present but did not run: " + err.Error()
		add(p, pr)
		return GPUFacts{Vendor: "none"}
	}

	g := GPUFacts{Vendor: "nvidia"}
	if m := smiVersions.FindStringSubmatch(string(out)); m != nil {
		g.DriverVersion, g.CUDAVersion = m[1], m[2]
	}
	for _, m := range smiModel.FindAllStringSubmatch(string(out), -1) {
		g.Models = append(g.Models, strings.TrimSpace(m[1]))
	}
	g.Models = uniqueSorted(g.Models)
	if _, err := env.LookPath("dcgmi"); err == nil {
		g.DCGM = true
	}

	pr.Available = true
	pr.Detail = "nvidia driver " + g.DriverVersion + ", CUDA " + g.CUDAVersion
	if !g.DCGM {
		pr.Detail += "; dcgmi absent, so per-job GPU occupancy is unavailable"
	}
	add(p, pr)
	return g
}

// ---------- mounts ----------

// sharedFS is the set of filesystem types worth recording.
//
// A deny-list of local filesystems would be wrong here: new local types appear
// (overlay, erofs, bpf) and would silently pollute every profile. Naming the
// shared types instead means an unrecognized filesystem is simply absent, which
// §2.6 says is the correct outcome, and an admin who wants it adds a line.
var sharedFS = map[string]bool{
	"lustre": true, "gpfs": true, "nfs": true, "nfs4": true,
	"beegfs": true, "panfs": true, "ceph": true, "cephfs": true, "fhgfs": true,
	"weka": true, "wekafs": true, "daos": true,
}

func probeMounts(env collectors.Env, p *Profile) []Mount {
	pr := Probe{
		Name:    "mounts",
		Level:   LevelUnprivileged,
		Reveals: "the shared filesystems a submission script can reference, and which of them is a plausible source of an I/O stall",
	}

	data, err := env.ReadFile("/proc/mounts")
	if err != nil {
		pr.Detail = "/proc/mounts not readable"
		add(p, pr)
		return nil
	}

	var out []Mount
	seen := map[string]bool{}
	for _, line := range strings.Split(string(data), "\n") {
		f := strings.Fields(line)
		if len(f) < 3 || !sharedFS[f[2]] {
			continue
		}
		mp := unescapeMount(f[1])
		if seen[mp] {
			continue
		}
		seen[mp] = true
		out = append(out, Mount{Mountpoint: mp, FSType: f[2], Source: unescapeMount(f[0])})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Mountpoint < out[j].Mountpoint })

	if len(out) == 0 {
		pr.Detail = "no shared filesystems mounted here (looked for lustre, gpfs, nfs, beegfs, panfs, ceph, weka, daos)"
		add(p, pr)
		return nil
	}
	pr.Available = true
	var names []string
	for _, m := range out {
		names = append(names, m.Mountpoint)
	}
	pr.Detail = strings.Join(names, " ")
	add(p, pr)
	return out
}

// unescapeMount decodes the octal escapes /proc/mounts uses for spaces and tabs.
func unescapeMount(s string) string {
	return strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`).Replace(s)
}

// ---------- bmc and metrics ----------

func probeBMC(env collectors.Env, p *Profile) BMCFacts {
	pr := Probe{
		Name:    "bmc",
		Level:   LevelPrivileged,
		Reveals: "chassis power events, thermal excursions and SEL entries — the evidence that a node died rather than its job",
	}

	if _, err := env.LookPath("ipmitool"); err != nil {
		pr.Detail = "ipmitool not present, and cairn does not probe Redfish over the network during init"
		add(p, pr)
		return BMCFacts{Kind: "none"}
	}
	// Deliberately not run. `ipmitool` against a local BMC needs /dev/ipmi0,
	// which is root-only on every distro, and reaching a remote BMC means
	// putting credentials in a config file. Both are decisions for an admin,
	// not for a discovery pass — so init records that the tool exists and
	// leaves the endpoint to be filled in by hand.
	pr.Detail = "ipmitool present but not run; set the endpoint here by hand if you want BMC evidence"
	add(p, pr)
	return BMCFacts{Kind: "ipmi"}
}

func probeMetrics(env collectors.Env, p *Profile) []MetricsSystem {
	pr := Probe{
		Name:    "metrics",
		Level:   LevelUnprivileged,
		Reveals: "telemetry already deployed here, which cairn reads from rather than replacing (CLAUDE.md §7)",
	}

	var out []MetricsSystem
	for _, c := range []struct{ kind, path string }{
		{"prometheus", "/etc/prometheus/prometheus.yml"},
		{"ganglia", "/etc/ganglia/gmond.conf"},
	} {
		if _, err := env.Stat(c.path); err == nil {
			out = append(out, MetricsSystem{Kind: c.kind})
		}
	}

	if len(out) == 0 {
		pr.Detail = "no Prometheus or Ganglia configuration on this host; if they run elsewhere, add the endpoint here by hand"
		add(p, pr)
		return nil
	}
	pr.Available = true
	var kinds []string
	for _, m := range out {
		kinds = append(kinds, m.Kind)
	}
	pr.Detail = strings.Join(kinds, ", ") + "; endpoints are not probed, fill them in by hand"
	add(p, pr)
	return out
}

// ---------- helpers ----------

// uniqueSorted trims, drops blanks, deduplicates and sorts. Every list in a
// profile goes through it, because a list whose order depends on what a tool
// happened to print is a list that breaks byte-comparison (§2.7).
func uniqueSorted(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}
