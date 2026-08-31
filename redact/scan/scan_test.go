package scan

import (
	"strings"
	"testing"
)

// TestCatchesUnredacted plants material of each kind the scanner claims to
// catch. Every string here is invented for this test; none of it comes from a
// real site.
//
// A guard that has never been observed to fire is a guard nobody has verified,
// and this one is the last thing standing between a hand-redaction slip and a
// permanent public commit.
func TestCatchesUnredacted(t *testing.T) {
	cases := map[string]struct {
		content string
		rule    string
	}{
		"routable ipv4":     {"NodeAddr=10.1.2.3", "ipv4"},
		"public ipv4":       {"connecting to 140.221.9.14 failed", "ipv4"},
		"ipv6":              {"fe80:0000:0000:0000:0202:b3ff:fe1e:8329", "ipv6"},
		"ib guid":           {"Port GUID: 0x0002c903004b1234", "ib-guid"},
		"ib guid low bits":  {"Node GUID: 0xb8599f0300fa1234", "ib-guid"},
		"mac":               {"link/ether 00:1b:21:3c:4d:5e brd ff:ff:ff:ff:ff:ff", "mac"},
		"email":             {"submitted by ada@physics.example-university.edu", "email"},
		"fqdn edu":          {"login2.cluster.someuniversity.edu", "fqdn"},
		"fqdn gov":          {"cori01.nersc-like.gov", "fqdn"},
		"home path":         {"/home/asmith/run.sh", "home-path"},
		"users path":        {"WorkDir=/users/jdoe/project", "home-path"},
		"ssh key":           {"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIabcdefghijklmnop user", "ssh-key"},
		"private key":       {"-----BEGIN OPENSSH PRIVATE KEY-----", "private-key"},
		"long hex":          {"munge key digest 4f3c2b1a09876543210fedcba9876543210abcdef", "long-hex"},
		"uid in user range": {"process running as uid=54321", "uid"},
		"xsede account":     {"Account=TG-CHE200098", "account-code"},
		"project code":      {"charged to PROJ12345", "account-code"},
		"slurm ClusterName": {"ClusterName=frontera", "slurm-conf-host"},
		"slurm NodeName":    {"NodeName=c123-456 CPUs=56", "slurm-conf-host"},
		"control machine":   {"SlurmctldHost=mgmt01", "slurm-conf-host"},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got := Scan("fixtures/test/input.txt", []byte(tc.content))
			if len(got) == 0 {
				t.Fatalf("scanner missed %q; expected rule %q to fire", tc.content, tc.rule)
			}
			var rules []string
			for _, f := range got {
				rules = append(rules, f.Rule)
				if f.Rule == tc.rule {
					return
				}
			}
			t.Errorf("scanner fired %v on %q, but not the expected rule %q",
				rules, tc.content, tc.rule)
		})
	}
}

// TestAcceptsRedacted confirms that correctly redacted fixtures pass. A scanner
// that flags its own pseudonym convention would be turned off within a week,
// which is the actual failure mode to avoid.
func TestAcceptsRedacted(t *testing.T) {
	clean := []string{
		`NodeName=node-0042 CPUs=56`,
		`ClusterName=cluster-a`,
		`WorkDir=/home/user-01/project`,
		`JobID=918273.batch State=OUT_OF_MEMORY ExitCode=0:125`,
		`User=user-01 Account=acct-01 Partition=gpu`,
		`address 192.0.2.10`,
		`address 198.51.100.7`,
		`address 203.0.113.44`,
		`listening on 127.0.0.1`,
		`daemon running as uid=0`,
		`nobody uid=65534`,
		`pci 0000:c1:00.0`,
		`NVRM: Xid (PCI:0000:c1:00): 79, GPU has fallen off the bus.`,
		`Driver Version: 535.104.05  CUDA Version: 12.4`,
		`mlx5_0 port 1 ==> Down (Polling)`,
		`see https://www.example.com/docs`,
		`slurmstepd: error: Detected 1 oom_kill event in StepId=918273.batch`,
		`TimeLimit=01:00:00 Elapsed=01:00:12`,
		`[8-20%4] pending array tasks`,
		`Port GUID: 0x0000000000000012`,
		`Node GUID: 0x0000000000000011`,
	}
	for _, line := range clean {
		if got := Scan("fixtures/test/input.txt", []byte(line)); len(got) > 0 {
			t.Errorf("false positive on redacted content %q:\n  %v", line, got[0])
		}
	}
}

func TestAnnotationSuppresses(t *testing.T) {
	line := "ControlMachine=realhost.someuniversity.edu"
	if got := Scan("f", []byte(line)); len(got) == 0 {
		t.Fatal("baseline did not fire; the annotation test proves nothing")
	}
	annotated := line + "   # redaction-ok: invented for the scanner's own test"
	if got := Scan("f", []byte(annotated)); len(got) != 0 {
		t.Errorf("annotation did not suppress findings: %v", got)
	}
}

// TestFindingsAreDeterministic: a scanner whose output reorders between runs
// cannot be used in CI, for the same reason non-deterministic bundles cannot be
// diffed.
func TestFindingsAreDeterministic(t *testing.T) {
	content := strings.Join([]string{
		"ClusterName=somecluster",
		"NodeAddr=10.9.8.7",
		"/home/bsmith/x",
		"uid=4242 gid=4242",
		"0x0002c903004b9999",
	}, "\n")

	first := Scan("f", []byte(content))
	if len(first) == 0 {
		t.Fatal("expected findings")
	}
	for i := 0; i < 50; i++ {
		got := Scan("f", []byte(content))
		if len(got) != len(first) {
			t.Fatalf("finding count varies between runs: %d vs %d", len(first), len(got))
		}
		for j := range got {
			if got[j].String() != first[j].String() {
				t.Fatalf("finding %d differs between runs:\n%s\nvs\n%s", j, first[j], got[j])
			}
		}
	}
}

func TestLineAndColumnAreReported(t *testing.T) {
	content := "clean line\nsecond clean\nNodeAddr=10.1.2.3\n"
	got := Scan("fixtures/x/input/slurm.conf", []byte(content))
	if len(got) == 0 {
		t.Fatal("expected a finding")
	}
	if got[0].Line != 3 {
		t.Errorf("line = %d, want 3", got[0].Line)
	}
	if got[0].Col < 1 {
		t.Errorf("col = %d, want >= 1", got[0].Col)
	}
	if !strings.Contains(got[0].String(), "fixtures/x/input/slurm.conf:3:") {
		t.Errorf("finding does not render as an editor-navigable location: %s", got[0])
	}
}

func TestIsPseudonym(t *testing.T) {
	for _, s := range []string{
		"node-0042", "user-01", "acct-01", "cluster-a", // hand-redaction ordinals
		"node-41938274", "user-00291837", "acct-71620045", "cluster-00483712", // machine-derived
	} {
		if !IsPseudonym(s) {
			t.Errorf("%q should be recognized as a pseudonym", s)
		}
	}
	for _, s := range []string{"frontera", "c123-456", "asmith", "node", "node-"} {
		if IsPseudonym(s) {
			t.Errorf("%q should not be recognized as a pseudonym", s)
		}
	}
}

// TestRuleSetIsPinned guards against a rule being deleted to make a stubborn
// fixture pass. Removing one requires editing this list deliberately.
func TestRuleSetIsPinned(t *testing.T) {
	want := []string{
		"account-code", "email", "fqdn", "home-path", "ib-guid", "ipv4", "ipv6",
		"long-hex", "mac", "private-key", "slurm-conf-host", "ssh-key", "uid",
	}
	got := RuleNames()
	if len(got) != len(want) {
		t.Fatalf("rule set changed: got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("rule set changed: got %v, want %v", got, want)
		}
	}
}

// TestIsFixtureData pins what the scanner considers part of the corpus. Getting
// this wrong in the permissive direction floods CI with findings from import
// paths and worked examples in documentation, which trains people to ignore the
// scanner — the only way a tool like this really fails.
func TestIsFixtureData(t *testing.T) {
	for _, p := range []string{
		"fixtures/001-oom-cgroup/input/sacct.txt",
		"fixtures/001-oom-cgroup/input/journal.txt",
		"fixtures/001-oom-cgroup/meta.yaml",
		"fixtures/001-oom-cgroup/expected/events.json",
		"fixtures/005-ib-link-flap/input/ibstat.txt",
		"fixtures/004-node-not-responding/input/slurmctld.log",
	} {
		if !IsFixtureData(p) {
			t.Errorf("%q should be scanned as fixture data", p)
		}
	}
	for _, p := range []string{
		"fixtures/fixtures.go",
		"fixtures/fixtures_test.go",
		"fixtures/README.md",
		"fixtures/005-ib-link-flap/input/.redaction-ok",
	} {
		if IsFixtureData(p) {
			t.Errorf("%q should not be scanned as fixture data", p)
		}
	}
}
