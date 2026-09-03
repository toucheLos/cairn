package policy

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/touchelos/cairn/yamlsub"
)

// ConfigVersion is the version of the policy.yaml format.
const ConfigVersion = 1

// DefaultPath is where cairn looks for a policy when not told otherwise.
const DefaultPath = "policy.yaml"

// PathEnv overrides DefaultPath.
const PathEnv = "CAIRN_POLICY"

// Policy is what an operator has authorized.
//
// The zero value denies everything, and that is load-bearing rather than
// incidental: a policy that failed to load, a policy file that does not exist,
// and a policy that explicitly allows nothing must all behave identically. Any
// arrangement where an error produces a *more* permissive engine than success is
// the arrangement that eventually drains a production node.
//
// It lives in its own file rather than in site.yaml. site.yaml is discovery
// output that `cairn init --force` regenerates, and authorization must not sit
// in a file a probe can overwrite; the two also deserve different review.
type Policy struct {
	Version int
	Cluster string

	// Allow lists the action kinds permitted. Empty means none.
	Allow []Kind

	// Nodes and Jobs bound what a permitted action may target. Empty means no
	// target is in scope — not "any target".
	Nodes []string
	Jobs  []string

	// DryRun decides everything and executes nothing. It defaults to true, and
	// a policy file that omits it stays true: turning off dry-run has to be a
	// sentence somebody wrote and somebody else reviewed.
	DryRun bool

	// Path is where this policy was read from, for operator output. Empty when
	// the policy is a default rather than a file.
	Path string
}

// Deny returns the policy that permits nothing.
//
// Named, and used wherever loading fails, so that "we could not read the policy"
// and "the policy forbids this" converge on the same behavior.
func Deny() Policy {
	return Policy{Version: ConfigVersion, DryRun: true}
}

// Allows reports whether the policy permits a kind.
func (p Policy) Allows(k Kind) bool {
	for _, a := range p.Allow {
		if a == k {
			return true
		}
	}
	return false
}

// InScope reports whether a target is within the policy's declared bounds.
//
// A pattern of "*" means every node or job. It has to be written explicitly:
// an operator who wants fleet-wide scope should have to say so, and an empty
// list must never mean "everything".
func (p Policy) InScope(t Target) bool {
	if t.Empty() {
		return false
	}
	if t.Node != "" && !matchAny(p.Nodes, t.Node) {
		return false
	}
	if t.Job != "" && !matchAny(p.Jobs, t.Job) {
		return false
	}
	return true
}

func matchAny(patterns []string, v string) bool {
	for _, p := range patterns {
		if p == "*" || p == v {
			return true
		}
		// A single trailing wildcard, so a site can scope to a rack or a
		// partition prefix. Deliberately not a full glob: this expression
		// decides what may be touched, and a richer language is more ways to
		// write one by accident.
		if strings.HasSuffix(p, "*") && strings.HasPrefix(v, strings.TrimSuffix(p, "*")) {
			return true
		}
	}
	return false
}

// Encode renders a policy as the file an operator reviews.
func (p Policy) Encode() ([]byte, error) {
	root := yamlsub.Map()
	root.SetRaw("version", "Format version of this file.", strconv.Itoa(orDefault(p.Version, ConfigVersion)))
	root.Set("cluster", "The cluster this policy applies to.", p.Cluster)

	allow := make([]string, len(p.Allow))
	for i, k := range p.Allow {
		allow[i] = string(k)
	}
	if len(allow) == 0 {
		// Written out rather than omitted. An absent key and an empty list mean
		// the same thing to the parser, but only one of them tells a reader that
		// the emptiness was intended.
		root.SetRaw("allow",
			"Action kinds this site permits. Default-deny: anything not listed\n"+
				"here cannot run, and an empty list permits nothing.\n"+
				"\n"+
				"This build ships no actions at all, so there is currently nothing\n"+
				"that could be listed. See `cairn policy`.", "[]")
	} else {
		root.SetList("allow",
			"Action kinds this site permits. Default-deny: anything not listed\n"+
				"here cannot run.", allow)
	}

	// Scope keys are always written, empty or not. This file exists to be edited,
	// and a key an operator cannot see is one they will not think to set — while
	// an explicit `nodes: []` says both that the key exists and that it currently
	// permits nothing.
	setListOrEmpty(root, "nodes",
		"Nodes a permitted action may target. \"*\" means all, a trailing \"*\"\n"+
			"matches a prefix, and an empty list means none. Fleet-wide scope has to\n"+
			"be written out; it is never inferred from an omission.", p.Nodes)
	setListOrEmpty(root, "jobs",
		"Jobs a permitted action may target. Same rules as nodes.", p.Jobs)

	root.SetRaw("dry_run",
		"Decide everything, execute nothing.\n"+
			"\n"+
			"True by default, and a file that omits it stays true. Turning this off\n"+
			"is the moment cairn stops being read-only, so it should be a sentence\n"+
			"somebody wrote and somebody else reviewed.", strconv.FormatBool(p.DryRun))

	var b strings.Builder
	b.WriteString(header)
	b.WriteString("\n")
	b.WriteString(root.Render())
	return []byte(b.String()), nil
}

const header = `# cairn policy.
#
# What cairn is permitted to do to this cluster. It is default-deny: an action
# that is not listed here cannot run, and neither can one this build does not
# implement.
#
# cairn is read-only by default in every code path (CLAUDE.md §2.4). This file is
# the only thing that could ever change that, which is why it is separate from
# site.yaml — that file is regenerated by a probe, and this one must not be.
#
# Unknown keys are an error rather than being ignored, so a typo here fails
# loudly instead of silently reverting to whatever the default was.
`

// setListOrEmpty writes a list, or an explicit empty list when there is nothing
// to write, so the key is always visible in the generated file.
func setListOrEmpty(n *yamlsub.Node, key, comment string, values []string) {
	if len(values) == 0 {
		n.SetRaw(key, comment, "[]")
		return
	}
	n.SetList(key, comment, values)
}

func orDefault(v, def int) int {
	if v == 0 {
		return def
	}
	return v
}

// Decode parses a policy.yaml.
//
// On any error the caller gets an error and no policy — never a partially
// understood one. A half-read authorization file is the most dangerous object in
// this package.
func Decode(data []byte) (Policy, error) {
	root, err := yamlsub.Parse(data)
	if err != nil {
		return Deny(), fmt.Errorf("policy.yaml: %w", err)
	}
	if !root.IsMap() {
		return Deny(), fmt.Errorf("policy.yaml: expected a mapping at the top level")
	}

	r := yamlsub.NewReader(root, "")
	p := Policy{
		Version: r.IntOr("version", ConfigVersion),
		Cluster: r.Str("cluster"),
		Nodes:   r.List("nodes"),
		Jobs:    r.List("jobs"),
	}
	for _, a := range r.List("allow") {
		p.Allow = append(p.Allow, Kind(a))
	}

	// dry_run defaults to true when absent. yamlsub.Bool cannot distinguish
	// "absent" from "false", so the presence check is explicit — getting this
	// backwards would make a file that forgot the key execute for real.
	if r.Str("dry_run") == "" {
		p.DryRun = true
	} else {
		p.DryRun = r.Bool("dry_run")
	}

	if errs := yamlsub.CollectErrors(r); len(errs) > 0 {
		return Deny(), fmt.Errorf("policy.yaml:\n  %s", strings.Join(errs, "\n  "))
	}
	if p.Version != ConfigVersion {
		return Deny(), fmt.Errorf(
			"policy.yaml: version %d, but this cairn understands version %d",
			p.Version, ConfigVersion)
	}
	return p, nil
}

// Load reads a policy from disk.
//
// A missing file is not an error: it means nothing is authorized, which is the
// correct and complete state for every site that has not opted in. It returns
// Deny() so that the absent case and the forbidden case are the same case.
func Load(path string) (Policy, error) {
	if path == "" {
		path = os.Getenv(PathEnv)
	}
	if path == "" {
		path = DefaultPath
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return Deny(), nil
		}
		return Deny(), err
	}
	p, err := Decode(data)
	if err != nil {
		return Deny(), fmt.Errorf("%s: %w", path, err)
	}
	p.Path = filepath.Clean(path)
	return p, nil
}
