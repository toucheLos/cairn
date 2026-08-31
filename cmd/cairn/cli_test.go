package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
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
	if !strings.HasPrefix(got.stdout, `{"schema_version":1,`) {
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
	if got := run(t, bin, nil, "version"); got.code != 0 || !strings.Contains(got.stdout, "schema version 1") {
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
