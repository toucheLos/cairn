// Package scan looks for unredacted material in fixture content.
//
// CLAUDE.md §3 requires that no real hostname, username, account code, or raw
// accounting row lands in this repository, and that fixtures are redacted by
// hand before they are committed. This package is the check on that claim. It is
// a backstop, not a substitute: hand redaction is still the process, and a clean
// scan is not evidence that a fixture was reviewed.
//
// The rules are tuned to over-flag. A false positive costs one `redaction-ok`
// annotation; a false negative costs an IP incident that cannot be undone once
// pushed. Where the two trade off, this package chooses noise.
package scan

import (
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Annotation suppresses findings on the line it appears on. It is deliberately
// per-line rather than per-file: a file-level opt-out would eventually be
// applied to a file that later grew a real hostname.
const Annotation = "redaction-ok"

// Finding is one suspected piece of unredacted material.
type Finding struct {
	Path  string
	Line  int
	Col   int
	Rule  string
	Match string
	Why   string
}

func (f Finding) String() string {
	return fmt.Sprintf("%s:%d:%d: [%s] %q — %s", f.Path, f.Line, f.Col, f.Rule, f.Match, f.Why)
}

type rule struct {
	name string
	re   *regexp.Regexp
	why  string
	// allow reports whether a specific match is acceptable despite matching the
	// pattern, e.g. a documentation-range IP address.
	allow func(match string) bool
}

// Pseudonym conventions used by redacted fixtures. Values matching these are the
// *output* of redaction and must not be flagged, or every correctly redacted
// fixture would fail the scan.
var (
	pseudoNode    = regexp.MustCompile(`^node-[0-9]{2,6}$`)
	pseudoUser    = regexp.MustCompile(`^user-[0-9]{2,4}$`)
	pseudoAccount = regexp.MustCompile(`^acct-[0-9]{2,4}$`)
	pseudoCluster = regexp.MustCompile(`^cluster-[a-z0-9]{1,8}$`)
)

// IsPseudonym reports whether s is already a redaction placeholder.
func IsPseudonym(s string) bool {
	return pseudoNode.MatchString(s) || pseudoUser.MatchString(s) ||
		pseudoAccount.MatchString(s) || pseudoCluster.MatchString(s)
}

var rules = []rule{
	{
		name: "ipv4",
		re:   regexp.MustCompile(`\b(?:\d{1,3}\.){3}\d{1,3}\b`),
		why:  "looks like an IP address; use an RFC 5737 documentation range (192.0.2.x, 198.51.100.x, 203.0.113.x)",
		allow: func(m string) bool {
			parts := strings.Split(m, ".")
			octets := make([]int, 0, 4)
			for _, p := range parts {
				n, err := strconv.Atoi(p)
				if err != nil || n < 0 || n > 255 {
					return true // not actually an IPv4 address; a version string, most likely
				}
				octets = append(octets, n)
			}
			switch {
			case octets[0] == 127, // loopback
				octets[0] == 0,
				octets[0] == 192 && octets[1] == 0 && octets[2] == 2,    // RFC 5737 TEST-NET-1
				octets[0] == 198 && octets[1] == 51 && octets[2] == 100, // TEST-NET-2
				octets[0] == 203 && octets[1] == 0 && octets[2] == 113:  // TEST-NET-3
				return true
			}
			return false
		},
	},
	{
		name: "ipv6",
		// Conservative: at least four groups, to avoid matching PCI addresses
		// (0000:c1:00.0) and timestamps.
		re:  regexp.MustCompile(`\b(?:[0-9a-fA-F]{1,4}:){4,}[0-9a-fA-F]{1,4}\b`),
		why: "looks like an IPv6 address or an InfiniBand GUID; replace with a placeholder",
	},
	{
		name: "ib-guid",
		re:   regexp.MustCompile(`\b0x[0-9a-fA-F]{16}\b`),
		why:  "looks like an InfiniBand GUID; these identify specific hardware at a specific site",
		allow: func(m string) bool {
			// Redaction convention: a GUID is replaced by zeroes plus a small
			// ordinal, e.g. 0x0000000000000001. This keeps ibstat output
			// structurally intact — the same GUID still appears in every place
			// the original did, so the fabric topology survives — while carrying
			// no information about the hardware or the site.
			return strings.HasPrefix(strings.ToLower(m), "0x000000000000")
		},
	},
	{
		name: "mac",
		re:   regexp.MustCompile(`\b(?:[0-9a-fA-F]{2}[:-]){5}[0-9a-fA-F]{2}\b`),
		why:  "looks like a MAC address",
	},
	{
		name: "email",
		re:   regexp.MustCompile(`\b[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}\b`),
		why:  "looks like an email address",
	},
	{
		name: "fqdn",
		re: regexp.MustCompile(`\b[a-zA-Z0-9][a-zA-Z0-9-]{0,62}(?:\.[a-zA-Z0-9][a-zA-Z0-9-]{0,62})*\.` +
			`(?:edu|gov|mil|org|com|net|int|ac\.[a-z]{2}|edu\.[a-z]{2}|gov\.[a-z]{2}|` +
			`de|fr|ch|it|es|nl|se|no|dk|fi|pl|cz|jp|cn|kr|in|au|ca|uk|br|za|sg|il|ru)\b`),
		why: "looks like a fully qualified domain name; site domains identify the institution",
		allow: func(m string) bool {
			// example.com / example.org / *.invalid are reserved for documentation.
			l := strings.ToLower(m)
			return strings.HasSuffix(l, "example.com") ||
				strings.HasSuffix(l, "example.org") ||
				strings.HasSuffix(l, "example.net") ||
				strings.HasSuffix(l, ".invalid")
		},
	},
	{
		name: "home-path",
		re:   regexp.MustCompile(`/(?:home|users|u|nfs/home|gpfs/home)/[A-Za-z0-9._-]+`),
		why:  "home directory paths contain account names",
		allow: func(m string) bool {
			i := strings.LastIndex(m, "/")
			return i >= 0 && IsPseudonym(m[i+1:])
		},
	},
	{
		name: "ssh-key",
		re:   regexp.MustCompile(`\b(?:ssh-(?:rsa|dss|ed25519)|ecdsa-sha2-nistp\d+)\s+[A-Za-z0-9+/]{20,}`),
		why:  "looks like an SSH public key",
	},
	{
		name: "private-key",
		re:   regexp.MustCompile(`-----BEGIN (?:[A-Z ]+ )?PRIVATE KEY-----`),
		why:  "private key material must never be committed under any circumstances",
	},
	{
		name: "long-hex",
		// Munge keys, password hashes, tokens. PCI addresses and Xid numbers are
		// far shorter than this.
		re:  regexp.MustCompile(`\b[0-9a-fA-F]{32,}\b`),
		why: "long hex string; could be a munge key, a hash, or a token",
	},
	{
		name: "uid",
		re:   regexp.MustCompile(`(?i)\b(?:uid|gid)\s*[=:]\s*(\d+)\b`),
		why:  "numeric uid/gid in the human range maps back to a specific account at a specific site",
		allow: func(m string) bool {
			digits := regexp.MustCompile(`\d+`).FindString(m)
			n, err := strconv.Atoi(digits)
			if err != nil {
				return true
			}
			// System accounts are not identifying; ordinary user ranges are.
			// Redaction maps real uids into the 90000+ band, which is above any
			// realistic site allocation and so is unambiguously a placeholder —
			// the value stays a plausible integer, so output that parses before
			// redaction still parses after it.
			return n < 1000 || n > 60000
		},
	},
	{
		name: "account-code",
		// Allocation codes: ACCESS/XSEDE (TG-CHE200098), NERSC (m1234),
		// and the general shape of an uppercase project code with a number.
		re:  regexp.MustCompile(`\b(?:TG-[A-Z]{3}\d{6}|[A-Z]{2,}[-_]?\d{4,})\b`),
		why: "looks like an allocation or project code",
	},
	{
		name: "slurm-conf-host",
		// ClusterName=, ControlMachine=, SlurmctldHost=, NodeName=, AccountingStorageHost=
		re: regexp.MustCompile(`(?i)\b(?:ClusterName|ControlMachine|SlurmctldHost|BackupController|` +
			`AccountingStorageHost|DbdHost|NodeName|NodeHostname|NodeAddr)\s*=\s*([A-Za-z0-9._\[\]{}-]+)`),
		why: "slurm.conf identity fields name the real cluster and its nodes",
		allow: func(m string) bool {
			i := strings.Index(m, "=")
			if i < 0 {
				return false
			}
			v := strings.TrimSpace(m[i+1:])
			// Accept placeholders and bracketed ranges over placeholders,
			// e.g. node-[0001-0128].
			base := regexp.MustCompile(`\[[^\]]*\]`).ReplaceAllString(v, "0000")
			return IsPseudonym(base)
		},
	},
}

// Suppressions are reviewed exceptions, keyed by "rule\tmatched-text".
//
// They exist because the inline Annotation cannot be used everywhere. A fixture's
// input files are captured producer output that Phase 1 collectors must parse;
// appending a comment to an ibstat dump or a sacct table would make the fixture
// unparseable and therefore useless as a test. Suppressions live in a sidecar
// file instead, where a reviewer reads them as a short, explicit list rather
// than hunting for comments scattered through the data.
type Suppressions map[string]string // key -> reason

func suppressionKey(rule, match string) string { return rule + "\t" + match }

// Add registers a reviewed exception.
func (s Suppressions) Add(rule, match, reason string) {
	s[suppressionKey(rule, match)] = reason
}

// ParseSuppressions reads a .redaction-ok sidecar file.
//
// Format, one per line: RULE<space>MATCHED-TEXT<space>#<space>REASON
// Blank lines and lines beginning with # are ignored. A reason is required —
// an unexplained suppression is indistinguishable from a mistake.
func ParseSuppressions(path string, content []byte) (Suppressions, error) {
	out := Suppressions{}
	for i, line := range strings.Split(string(content), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		hash := strings.Index(trimmed, "#")
		if hash < 0 {
			return nil, fmt.Errorf("%s:%d: suppression has no reason; append \"# why this is safe\"", path, i+1)
		}
		reason := strings.TrimSpace(trimmed[hash+1:])
		if reason == "" {
			return nil, fmt.Errorf("%s:%d: suppression has an empty reason", path, i+1)
		}
		body := strings.TrimSpace(trimmed[:hash])
		rule, match, ok := strings.Cut(body, " ")
		if !ok || strings.TrimSpace(match) == "" {
			return nil, fmt.Errorf("%s:%d: expected \"RULE MATCHED-TEXT # reason\", got %q", path, i+1, trimmed)
		}
		known := false
		for _, n := range RuleNames() {
			if n == rule {
				known = true
				break
			}
		}
		if !known {
			return nil, fmt.Errorf("%s:%d: unknown rule %q", path, i+1, rule)
		}
		out.Add(rule, strings.TrimSpace(match), reason)
	}
	return out, nil
}

// Scan examines one file's content and returns findings in a stable order.
//
// Findings are sorted by (line, column, rule) so that two runs over the same
// input produce identical output. Invariant §2.7 is about bundles, but a scanner
// whose output reorders between runs is unusable in CI for the same reason.
func Scan(path string, content []byte) []Finding {
	return ScanWith(path, content, nil)
}

// ScanWith is Scan with a set of reviewed exceptions applied.
func ScanWith(path string, content []byte, sup Suppressions) []Finding {
	var out []Finding
	lines := strings.Split(string(content), "\n")
	for i, line := range lines {
		if strings.Contains(line, Annotation) {
			continue
		}
		for _, r := range rules {
			for _, loc := range r.re.FindAllStringIndex(line, -1) {
				m := line[loc[0]:loc[1]]
				if r.allow != nil && r.allow(m) {
					continue
				}
				if _, ok := sup[suppressionKey(r.name, m)]; ok {
					continue
				}
				out = append(out, Finding{
					Path:  path,
					Line:  i + 1,
					Col:   loc[0] + 1,
					Rule:  r.name,
					Match: m,
					Why:   r.why,
				})
			}
		}
	}
	sort.SliceStable(out, func(a, b int) bool {
		if out[a].Line != out[b].Line {
			return out[a].Line < out[b].Line
		}
		if out[a].Col != out[b].Col {
			return out[a].Col < out[b].Col
		}
		return out[a].Rule < out[b].Rule
	})
	return out
}

// IsFixtureData reports whether a path holds captured producer output that must
// be scanned, as opposed to source or prose that must not.
//
// Go source and Markdown are excluded. Both are reviewed as what they are —
// code and documentation — and both legitimately contain strings that look like
// findings: import paths, and the redaction conventions documented with worked
// examples. Scanning them produces noise that trains people to ignore the
// scanner, which is the only way a tool like this actually fails.
//
// It lives here rather than in the command so that the CLI, the pre-commit hook,
// and the corpus test cannot drift apart about what "the corpus" means.
func IsFixtureData(path string) bool {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".go", ".md":
		return false
	}
	return filepath.Base(path) != SuppressionFile
}

// SuppressionFile is the sidecar listing reviewed exceptions for a directory.
const SuppressionFile = ".redaction-ok"

// RuleNames returns every rule name, sorted. Used by the golden test that pins
// the rule set so a rule cannot be silently dropped.
func RuleNames() []string {
	out := make([]string, 0, len(rules))
	for _, r := range rules {
		out = append(out, r.name)
	}
	sort.Strings(out)
	return out
}
