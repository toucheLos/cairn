package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/touchelos/cairn/schema"
)

// buildCairn compiles the binary once and returns its path. The CLI is what
// actually ships (§2.5, one static binary), so these tests exercise it as a
// process rather than calling the run* functions directly — that is the only way
// flag parsing, exit codes, and stdout/stderr separation get covered.
func buildCairn(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "cairn")
	out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput()
	if err != nil {
		t.Fatalf("building cairn: %v\n%s", err, out)
	}
	return bin
}

type invocation struct {
	stdout, stderr string
	code           int
}

func run(t *testing.T, bin string, env []string, args ...string) invocation {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Env = append(os.Environ(), env...)
	var so, se strings.Builder
	cmd.Stdout, cmd.Stderr = &so, &se
	err := cmd.Run()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("running %v: %v", args, err)
	}
	return invocation{so.String(), se.String(), code}
}

// corpus lists the fixtures with the job and node each one is about.
func corpus() []struct{ dir, job, node string } {
	return []struct{ dir, job, node string }{
		{"001-oom-cgroup", "918273", "node-0042"},
		{"002-walltime-exceeded", "918301", "node-0043"},
		{"003-gpu-driver-mismatch", "918412", "node-0048"},
		{"004-node-not-responding", "918520", "node-0044"},
		{"005-ib-link-flap", "918633", "node-0045"},
		{"006-munge-auth-failure", "918714", "node-0046"},
		{"007-nccl-hang", "918820", "node-0047"},
	}
}

func fixtureEnv(node string) []string {
	return []string{"CAIRN_CLUSTER=cluster-a", "CAIRN_NODE=" + node}
}

// TestContextOverCorpus runs the shipped command against every fixture. This is
// the Phase 2 acceptance test: one command, output you can paste into a model.
func TestContextOverCorpus(t *testing.T) {
	bin := buildCairn(t)
	for _, f := range corpus() {
		t.Run(f.dir, func(t *testing.T) {
			got := run(t, bin, fixtureEnv(f.node),
				"context", "--job", f.job, "--fixture", "../../fixtures/"+f.dir, "--tz", "UTC")
			if got.code != 0 {
				t.Fatalf("exit %d\nstderr: %s", got.code, got.stderr)
			}
			// Every fixture is one real incident, so every one must produce the
			// event that carries its job id.
			if !strings.Contains(got.stdout, "* ") {
				t.Errorf("no event marked as carrying job %s:\n%s", f.job, got.stdout)
			}
			for _, want := range []string{"cairn context — job " + f.job, "TIMELINE", "WHAT CAIRN COULD NOT SEE"} {
				if !strings.Contains(got.stdout, want) {
					t.Errorf("output is missing %q:\n%s", want, got.stdout)
				}
			}
			if strings.Contains(got.stdout, "No events") {
				t.Errorf("fixture produced no events:\n%s", got.stdout)
			}
		})
	}
}

// TestContextIsDeterministic — invariant §2.7 through the whole pipeline:
// collectors, join, redaction, rendering, in a fresh process each time.
func TestContextIsDeterministic(t *testing.T) {
	bin := buildCairn(t)
	for _, f := range corpus() {
		args := []string{"context", "--job", f.job, "--fixture", "../../fixtures/" + f.dir, "--tz", "UTC"}
		first := run(t, bin, fixtureEnv(f.node), args...).stdout
		for i := 0; i < 5; i++ {
			if got := run(t, bin, fixtureEnv(f.node), args...).stdout; got != first {
				t.Fatalf("%s: run %d differs\n--- first ---\n%s\n--- got ---\n%s", f.dir, i, first, got)
			}
		}
	}
}

// TestJSONOutputIsTheCanonicalBundle: --format json must be exactly what the
// schema produces, so a bundle attached to a ticket replays byte-identically.
func TestJSONOutputIsTheCanonicalBundle(t *testing.T) {
	bin := buildCairn(t)
	f := corpus()[0]
	got := run(t, bin, fixtureEnv(f.node),
		"context", "--job", f.job, "--fixture", "../../fixtures/"+f.dir, "--tz", "UTC", "--format", "json")
	if got.code != 0 {
		t.Fatalf("exit %d: %s", got.code, got.stderr)
	}
	// Derived from schema.Version rather than written out, so a deliberate
	// version bump does not present as an unrelated CLI test failure. The bump
	// itself is guarded by schema/testdata/bundle.golden, which is where a
	// reviewer should be made to read the diff.
	if !strings.HasPrefix(got.stdout, fmt.Sprintf(`{"schema_version":%d,`, schema.Version)) {
		t.Errorf("json output does not start with the bundle header:\n%s", got.stdout[:min(200, len(got.stdout))])
	}
	// A wall-clock stamp would make two bundles of the same window differ.
	for _, forbidden := range []string{"generated_at", "collected_at"} {
		if strings.Contains(got.stdout, forbidden) {
			t.Errorf("bundle contains a wall-clock field %q", forbidden)
		}
	}
}

// TestRedactionLeavesNothingIdentifying is the check that matters before a
// bundle leaves a site.
func TestRedactionLeavesNothingIdentifying(t *testing.T) {
	bin := buildCairn(t)
	env := append(fixtureEnv("node-0046"), "CAIRN_SALT=a-test-salt-of-sufficient-length")
	got := run(t, bin, env,
		"context", "--job", "918714", "--fixture", "../../fixtures/006-munge-auth-failure",
		"--tz", "UTC", "--redact")
	if got.code != 0 {
		t.Fatalf("exit %d: %s", got.code, got.stderr)
	}
	for _, secret := range []string{"node-0046", "acct-01", "user-01", "cluster-a"} {
		if strings.Contains(got.stdout, secret) {
			t.Errorf("%q survived --redact:\n%s", secret, got.stdout)
		}
	}
	// And the evidence must survive.
	for _, keep := range []string{"config.clock_skew", "skew_sec=312", "auth.munge"} {
		if !strings.Contains(got.stdout, keep) {
			t.Errorf("--redact destroyed evidence: %q is missing", keep)
		}
	}
	if !strings.Contains(got.stdout, "pseudonymized, salt sha256:") {
		t.Error("redacted output does not report its salt id")
	}
}

// TestShortSaltIsRefused: an unsalted or barely-salted pseudonym is a reversible
// hash of the hostname.
func TestShortSaltIsRefused(t *testing.T) {
	bin := buildCairn(t)
	got := run(t, bin, append(fixtureEnv("node-0046"), "CAIRN_SALT=tooshort"),
		"context", "--job", "918714", "--fixture", "../../fixtures/006-munge-auth-failure", "--redact")
	if got.code == 0 {
		t.Error("a short salt was accepted")
	}
	if !strings.Contains(got.stderr, "16") {
		t.Errorf("the error does not say what the minimum is: %s", got.stderr)
	}
}

func TestDoctorReportsMissingProducers(t *testing.T) {
	bin := buildCairn(t)
	got := run(t, bin, fixtureEnv("node-0042"), "doctor", "--fixture", "../../fixtures/001-oom-cgroup")
	if got.code != 0 {
		t.Fatalf("exit %d: %s", got.code, got.stderr)
	}
	// Producers with no collector must be named, or a clean report reads as
	// "cairn checked your fabric and it is fine".
	for _, s := range []string{"bmc", "fabric", "storage", "not yet implemented"} {
		if !strings.Contains(got.stdout, s) {
			t.Errorf("doctor does not disclose %q:\n%s", s, got.stdout)
		}
	}
}

func TestUsageAndErrors(t *testing.T) {
	bin := buildCairn(t)
	if got := run(t, bin, nil); got.code != 2 || !strings.Contains(got.stderr, "usage: cairn") {
		t.Errorf("bare invocation: exit %d, stderr %q", got.code, got.stderr)
	}
	if got := run(t, bin, nil, "context"); got.code == 0 || !strings.Contains(got.stderr, "--job is required") {
		t.Errorf("context without --job: exit %d, stderr %q", got.code, got.stderr)
	}
	if got := run(t, bin, nil, "context", "--job", "not-a-job-id"); got.code == 0 {
		t.Error("an unparseable job id was accepted")
	}
	if got := run(t, bin, nil, "nonsense"); got.code != 2 {
		t.Errorf("unknown command: exit %d", got.code)
	}
	wantVersion := fmt.Sprintf("schema version %d", schema.Version)
	if got := run(t, bin, nil, "version"); got.code != 0 || !strings.Contains(got.stdout, wantVersion) {
		t.Errorf("version: exit %d, stdout %q", got.code, got.stdout)
	}
}

// TestMissLog round-trips the log that CLAUDE.md §6 makes the input to Phase 3.
func TestMissLog(t *testing.T) {
	bin := buildCairn(t)
	logPath := filepath.Join(t.TempDir(), "misses.jsonl")
	env := []string{"CAIRN_MISS_LOG=" + logPath, "CAIRN_CLUSTER=cluster-a"}

	if got := run(t, bin, env, "miss", "--list"); got.code != 0 ||
		!strings.Contains(got.stdout, "No misses recorded") {
		t.Errorf("empty log: exit %d, stdout %q", got.code, got.stdout)
	}

	got := run(t, bin, env, "miss", "--when", "2026-03-04T09:14:02Z", "--kind", "misclassed",
		"--job", "918273", "--expected", "resource.oom",
		"--note", "reported app.nonzero_exit; the cgroup OOM was the cause")
	if got.code != 0 {
		t.Fatalf("recording a miss: exit %d, %s", got.code, got.stderr)
	}

	got = run(t, bin, env, "miss", "--list")
	for _, want := range []string{"misclassed", "918273", "resource.oom", "1 miss(es)"} {
		if !strings.Contains(got.stdout, want) {
			t.Errorf("--list is missing %q:\n%s", want, got.stdout)
		}
	}

	// Required fields and validated enums, so the log stays analysable.
	for _, args := range [][]string{
		{"miss", "--kind", "missed", "--when", "2026-03-04T09:14:02Z"},
		{"miss", "--note", "x", "--kind", "missed"},
		{"miss", "--note", "x", "--when", "2026-03-04T09:14:02Z"},
		{"miss", "--note", "x", "--when", "2026-03-04T09:14:02Z", "--kind", "not-a-kind"},
		{"miss", "--note", "x", "--when", "2026-03-04T09:14:02Z", "--kind", "missed", "--expected", "not.a.class"},
		{"miss", "--note", "x", "--when", "yesterday", "--kind", "missed"},
	} {
		if got := run(t, bin, env, args...); got.code == 0 {
			t.Errorf("accepted an invalid miss: %v", args)
		}
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// ---------- Phase 3: init, profile, diff ----------

// TestInitWritesAndRefusesToClobber covers the workflow CLAUDE.md §6 specifies:
// admins correct a generated file. The correction surviving a re-run is the
// whole feature, so the refusal is tested as carefully as the write.
func TestInitWritesAndRefusesToClobber(t *testing.T) {
	bin := buildCairn(t)
	out := filepath.Join(t.TempDir(), "site.yaml")

	got := run(t, bin, nil, "init", "--fixture", "../../site/testdata/slurm-lmod-gpu", "-o", out)
	if got.code != 0 {
		t.Fatalf("init: exit %d: %s", got.code, got.stderr)
	}
	first, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(first), "cluster: cluster-a") {
		t.Errorf("the cluster name should come from slurm.conf:\n%s", first)
	}

	// Re-running with no change must be a no-op, not a diff.
	if got := run(t, bin, nil, "init", "--fixture", "../../site/testdata/slurm-lmod-gpu", "-o", out); got.code != 0 {
		t.Errorf("re-running init on an identical file: exit %d: %s", got.code, got.stderr)
	}

	// An admin's correction must not be silently overwritten.
	edited := strings.Replace(string(first), "kind: slurm", "kind: pbs", 1)
	if err := os.WriteFile(out, []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}
	got = run(t, bin, nil, "init", "--fixture", "../../site/testdata/slurm-lmod-gpu", "-o", out)
	if got.code == 0 {
		t.Error("init overwrote a corrected file without --force")
	}
	if !strings.Contains(got.stderr, "scheduler.kind") {
		t.Errorf("the refusal must show what would change, got: %s", got.stderr)
	}
	after, _ := os.ReadFile(out)
	if string(after) != edited {
		t.Error("the corrected file was modified despite the refusal")
	}

	// --force is the escape hatch, and it must actually work.
	if got := run(t, bin, nil, "init", "--fixture", "../../site/testdata/slurm-lmod-gpu",
		"-o", out, "--force"); got.code != 0 {
		t.Errorf("--force: exit %d: %s", got.code, got.stderr)
	}
	forced, _ := os.ReadFile(out)
	if !bytes.Equal(forced, first) {
		t.Error("--force did not restore the probed profile")
	}
}

// TestInitOnUnknownStack is invariant §2.6 at the CLI boundary: an unrecognized
// scheduler must still produce a file, because the admin correcting it is
// exactly the person who knows what cairn failed to recognize.
func TestInitOnUnknownStack(t *testing.T) {
	bin := buildCairn(t)
	out := filepath.Join(t.TempDir(), "site.yaml")
	got := run(t, bin, nil, "init", "--fixture", "../../site/testdata/unknown-scheduler", "-o", out)
	if got.code != 0 {
		t.Fatalf("an unknown stack must not fail: exit %d: %s", got.code, got.stderr)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "kind: pbs") {
		t.Errorf("expected pbs to be detected:\n%s", data)
	}
}

// TestContextCarriesTheSiteHeader: §6 calls this the thing that stops a model
// answering a Slurm question in PBS syntax, so its presence is load-bearing.
func TestContextCarriesTheSiteHeader(t *testing.T) {
	bin := buildCairn(t)
	f := corpus()[5] // 006-munge-auth-failure, on cluster-a
	got := run(t, bin, fixtureEnv(f.node), "context", "--job", f.job,
		"--fixture", "../../fixtures/"+f.dir, "--tz", "UTC",
		"--site", "../../site/testdata/slurm-lmod-gpu.golden.yaml")
	if got.code != 0 {
		t.Fatalf("exit %d: %s", got.code, got.stderr)
	}
	for _, want := range []string{"SITE", "scheduler  slurm 23.02.7", "modules    lmod"} {
		if !strings.Contains(got.stdout, want) {
			t.Errorf("context output lacks %q:\n%s", want, got.stdout)
		}
	}
	// The cluster name comes from the profile rather than defaulting to "local".
	if !strings.Contains(got.stdout, "on cluster-a") {
		t.Errorf("cluster name did not come from the site profile:\n%s", got.stdout)
	}
}

// TestContextWithoutSiteSaysSo: silence would invite a reader to assume a stack,
// which is the failure the profile exists to prevent.
func TestContextWithoutSiteSaysSo(t *testing.T) {
	bin := buildCairn(t)
	f := corpus()[5]
	got := run(t, bin, append(fixtureEnv(f.node), "CAIRN_SITE="), "context", "--job", f.job,
		"--fixture", "../../fixtures/"+f.dir, "--tz", "UTC")
	if got.code != 0 {
		t.Fatalf("exit %d: %s", got.code, got.stderr)
	}
	if !strings.Contains(got.stdout, "no profile") || !strings.Contains(got.stdout, "cairn init") {
		t.Errorf("a missing profile must be stated and name the fix:\n%s", got.stdout)
	}
}

// TestDiffFlagsAfterTheNode guards a footgun that already bit once: Go's flag
// package stops parsing at the first non-flag argument, so `diff <node> --flag`
// silently ignored every flag after the node.
func TestDiffFlagsAfterTheNode(t *testing.T) {
	bin := buildCairn(t)
	got := run(t, bin, nil, "diff", "node-0046", "--profiles", "../../site/testdata/profiles")
	if got.code != 0 {
		t.Fatalf("exit %d: %s", got.code, got.stderr)
	}
	if !strings.Contains(got.stdout, "nvidia.driver_version") {
		t.Errorf("--profiles after the node was ignored:\n%s%s", got.stdout, got.stderr)
	}
	// The same invocation with the flag first must produce identical output.
	other := run(t, bin, nil, "diff", "--profiles", "../../site/testdata/profiles", "node-0046")
	if other.stdout != got.stdout {
		t.Errorf("flag order changed the output:\n%s\nvs\n%s", got.stdout, other.stdout)
	}
}

// TestDiffRefusesBelowMinPeers: a confident-looking claim derived from one peer
// is worse than no claim, and the refusal has to reach the operator.
func TestDiffRefusesBelowMinPeers(t *testing.T) {
	bin := buildCairn(t)
	got := run(t, bin, nil, "diff", "node-0046", "--profiles", "../../site/testdata/profiles-few")
	if got.code != 0 {
		t.Fatalf("a refusal is a result, not an error: exit %d: %s", got.code, got.stderr)
	}
	if !strings.Contains(got.stdout, "Not compared") {
		t.Errorf("expected a refusal:\n%s", got.stdout)
	}
	if strings.Contains(got.stdout, "DIVERGENCE") {
		t.Errorf("a refused comparison must report no drift:\n%s", got.stdout)
	}
}

// TestDiffJSONRedacts is the check that cannot be undone once a bundle is sent.
func TestDiffJSONRedacts(t *testing.T) {
	bin := buildCairn(t)
	env := []string{"CAIRN_SALT=0123456789abcdef0123456789abcdef"}
	got := run(t, bin, env, "diff", "node-0049",
		"--profiles", "../../site/testdata/profiles", "--redact", "--format", "json")
	if got.code != 0 {
		t.Fatalf("exit %d: %s", got.code, got.stderr)
	}
	for _, forbidden := range []string{"node-0049", "cluster-a", "192.0.2.10"} {
		if strings.Contains(got.stdout, forbidden) {
			t.Errorf("%q survived redaction:\n%s", forbidden, got.stdout)
		}
	}
	// The drift is still legible: pseudonymized, not deleted.
	if !strings.Contains(got.stdout, "config.drift") || !strings.Contains(got.stdout, "lustre") {
		t.Errorf("redaction destroyed the evidence:\n%s", got.stdout)
	}
}

// TestProfileIsDeterministic: two captures of one fixture at a fixed time must
// be byte-identical, or a fleet comparison is comparing noise (§2.7).
func TestProfileIsDeterministic(t *testing.T) {
	bin := buildCairn(t)
	args := []string{"profile", "--fixture", "../../site/testdata/slurm-lmod-gpu",
		"--node", "node-0046", "--cluster", "cluster-a", "--at", "2026-03-04T09:00:00Z"}
	first := run(t, bin, nil, args...)
	if first.code != 0 {
		t.Fatalf("exit %d: %s", first.code, first.stderr)
	}
	for i := 0; i < 5; i++ {
		if got := run(t, bin, nil, args...); got.stdout != first.stdout {
			t.Fatalf("capture %d differs:\n%s\nvs\n%s", i, first.stdout, got.stdout)
		}
	}
	// The munge key mtime is stat'ed, and its contents must never be read.
	if !strings.Contains(first.stdout, "munge.key_mtime") {
		t.Errorf("expected a munge key mtime drift key:\n%s", first.stdout)
	}
}
