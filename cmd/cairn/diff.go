package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/touchelos/cairn/redact"
	"github.com/touchelos/cairn/schema"
	"github.com/touchelos/cairn/site"
)

func runDiff(args []string) error {
	fs := flag.NewFlagSet("diff", flag.ExitOnError)
	var common commonFlags
	common.register(fs)
	var (
		dir      = fs.String("profiles", "profiles", "directory of node profiles written by `cairn profile`")
		format   = fs.String("format", "text", "text | json")
		doRedact = fs.Bool("redact", false, "pseudonymize hosts, users and accounts before output")
		all      = fs.Bool("A", false, "compare every node in the directory, not just one")
	)
	fs.Usage = func() {
		fmt.Fprint(fs.Output(), `usage: cairn diff [node] [flags]
       cairn diff -A [flags]

Compare a node's captured profile against its fleet siblings, and report every
key on which it diverges. With no node named, the local host is compared.

This replaces a threshold someone guessed in 2014 with a comparison against the
other 47 machines: a node is interesting when it differs from its peers, not
when it crosses a number.

What this does NOT say is which side is right. A node diverging from 47 siblings
may be the only correctly configured machine in the room — the one that got the
patch. cairn reports the divergence and leaves the verdict to you.

Below `+fmt.Sprint(site.MinPeers)+` siblings it refuses to compare at all, because a majority of two
is not a fleet norm.

Capture profiles first:

  srun -w node[001-048] --ntasks-per-node=1 \
      sh -c 'cairn profile > profiles/$(hostname -s).json'

flags:
`)
		fs.PrintDefaults()
	}
	// The node is an operand, and operands read naturally before flags:
	// `cairn diff node-0046 --profiles ...` is what anyone types, and it is what
	// this command's own usage line advertises. Go's flag package stops parsing
	// at the first non-flag argument, so without lifting the operand out first
	// every flag after it is silently ignored — the flag keeps its default and
	// nothing reports that it was dropped.
	operand, rest := splitLeadingOperand(args)
	if err := fs.Parse(rest); err != nil {
		return err
	}
	if operand != "" && fs.NArg() > 0 {
		return fmt.Errorf("compare one node, or -A for every node")
	}

	profiles, err := loadNodeProfiles(*dir)
	if err != nil {
		return err
	}
	if len(profiles) == 0 {
		return fmt.Errorf("no node profiles in %s; capture some with `cairn profile` first", *dir)
	}
	if *format != "text" && *format != "json" {
		return fmt.Errorf("--format must be text or json, got %q", *format)
	}

	var targets []schema.Hostname
	switch {
	case *all:
		for _, p := range profiles {
			targets = append(targets, p.Node)
		}
	case operand != "":
		targets = []schema.Hostname{schema.Hostname(operand)}
	case fs.NArg() == 1:
		targets = []schema.Hostname{schema.Hostname(fs.Arg(0))}
	case fs.NArg() > 1:
		return fmt.Errorf("compare one node, or -A for every node")
	default:
		env, err := common.env()
		if err != nil {
			return err
		}
		h := env.Hostname()
		if h == "" {
			return fmt.Errorf("could not determine this host's name; name a node, or use -A")
		}
		targets = []schema.Hostname{schema.Hostname(h)}
	}
	sort.Slice(targets, func(i, j int) bool { return targets[i] < targets[j] })

	var results []site.DiffResult
	for _, t := range targets {
		target, ok := findProfile(profiles, t)
		if !ok {
			return fmt.Errorf("no profile for node %q in %s; found %s",
				t, *dir, strings.Join(nodeNames(profiles), ", "))
		}
		results = append(results, site.Compare(target, profiles))
	}

	if *format == "json" {
		return emitDiffJSON(results, *doRedact)
	}
	fmt.Print(renderDiff(results))
	return nil
}

// splitLeadingOperand lifts a leading non-flag argument out of the argument
// list, so flags after it are still parsed.
//
// Only a *leading* operand, and only one. Anything more would be reimplementing
// getopt, and this exists to fix one specific footgun rather than to become a
// second argument parser.
func splitLeadingOperand(args []string) (operand string, rest []string) {
	if len(args) > 0 && args[0] != "" && !strings.HasPrefix(args[0], "-") {
		return args[0], args[1:]
	}
	return "", args
}

// loadNodeProfiles reads every *.json in a directory.
//
// A file that does not parse is a hard error rather than a skip. Invariant §2.6
// is about unknown *stacks*, not corrupt input: silently dropping a sibling
// would shrink the peer set and shift the majority, and the operator would never
// know the comparison had been made against a different fleet than they asked
// for.
func loadNodeProfiles(dir string) ([]site.NodeProfile, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".json") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	var out []site.NodeProfile
	seen := map[schema.Hostname]string{}
	for _, name := range names {
		full := filepath.Join(dir, name)
		data, err := os.ReadFile(full)
		if err != nil {
			return nil, err
		}
		np, err := site.DecodeNodeJSON(data)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", full, err)
		}
		if prev, dup := seen[np.Node]; dup {
			return nil, fmt.Errorf(
				"node %s is profiled twice, in %s and %s; one of them would vote twice",
				np.Node, prev, full)
		}
		seen[np.Node] = full
		out = append(out, np)
	}
	return out, nil
}

func findProfile(ps []site.NodeProfile, node schema.Hostname) (site.NodeProfile, bool) {
	for _, p := range ps {
		if p.Node == node {
			return p, true
		}
	}
	return site.NodeProfile{}, false
}

func nodeNames(ps []site.NodeProfile) []string {
	out := make([]string, len(ps))
	for i, p := range ps {
		out[i] = string(p.Node)
	}
	sort.Strings(out)
	return out
}

// staleAfter is the capture spread past which a comparison is called into
// question in the output.
//
// A day is generous. It is not a cutoff — cairn still reports the drift, because
// refusing would be worse — but a reader has to be told that "drift" measured
// against profiles a week old may just be a reboot they already know about.
const staleAfter = 24 * time.Hour

func renderDiff(results []site.DiffResult) string {
	var b strings.Builder
	for i, r := range results {
		if i > 0 {
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "cairn diff — %s on cluster %s\n", r.Node, r.Cluster)

		if r.Refused != "" {
			fmt.Fprintf(&b, "\nNot compared: %s\n", r.Refused)
			b.WriteString("\nCapture more of the fleet and re-run. A comparison against one or two\n" +
				"machines produces confident-looking output from a coin flip, which is\n" +
				"worse than no output.\n")
			continue
		}

		fmt.Fprintf(&b, "%d sibling(s): %s\n", len(r.Peers), joinHosts(r.Peers))
		if s := r.CaptureSpread(); s > staleAfter {
			fmt.Fprintf(&b, "captures span %s — siblings profiled this far apart may differ\n"+
				"because of when they were read, not how they are configured\n", roundSpread(s))
		}

		if len(r.Drifts) == 0 {
			b.WriteString("\nNo divergence from the sibling majority on any captured key.\n")
		} else {
			b.WriteString("\nDIVERGENCE FROM SIBLING MAJORITY\n")
			wKey := 0
			for _, d := range r.Drifts {
				if len(d.Key) > wKey {
					wKey = len(d.Key)
				}
			}
			for _, d := range r.Drifts {
				fmt.Fprintf(&b, "  %-*s  this node: %s\n", wKey, d.Key, d.Observed)
				fmt.Fprintf(&b, "  %-*s  %d of %d siblings: %s\n", wKey, "", d.PeerMajority, d.PeerCount, d.Expected)
			}
			b.WriteString("\nWhich side is correct is not something cairn can tell you. A node that\n" +
				"differs from its siblings may be the only one that got the patch.\n")
		}

		if len(r.Undecided) > 0 {
			fmt.Fprintf(&b, "\nNO FLEET MAJORITY (not compared)\n  %s\n", strings.Join(r.Undecided, "\n  "))
			b.WriteString("The siblings do not agree with each other on these, so there is no norm\n" +
				"for this node to diverge from. That is usually worth looking at on its own.\n")
		}
	}
	return b.String()
}

func joinHosts(hs []schema.Hostname) string {
	out := make([]string, len(hs))
	for i, h := range hs {
		out[i] = string(h)
	}
	return strings.Join(out, " ")
}

func roundSpread(d time.Duration) string {
	if d >= 24*time.Hour {
		return fmt.Sprintf("%.0fd", d.Hours()/24)
	}
	return d.Round(time.Minute).String()
}

// emitDiffJSON renders the drifts as a canonical bundle.
//
// A bundle rather than a bespoke format, so a drift report archives, redacts and
// replays exactly like the evidence from any other producer — and so a Track A
// site can attach one to a ticket with the same tooling (CLAUDE.md §7).
func emitDiffJSON(results []site.DiffResult, doRedact bool) error {
	var events []schema.Event
	var cluster schema.ClusterName
	var lo, hi time.Time
	for _, r := range results {
		evs, err := r.Events()
		if err != nil {
			return err
		}
		events = append(events, evs...)
		if cluster == "" {
			cluster = r.Cluster
		}
		if !r.Oldest.IsZero() && (lo.IsZero() || r.Oldest.Before(lo)) {
			lo = r.Oldest
		}
		if r.Newest.After(hi) {
			hi = r.Newest
		}
	}
	if cluster == "" {
		return fmt.Errorf("node profiles carry no cluster name")
	}

	bundle := schema.Bundle{
		Cluster: cluster,
		Window:  schema.Window{Start: lo, End: hi},
		// Every profile was captured by cairn on its own node, so there is no
		// second clock to reconcile — but "assumed_zero" is stated rather than
		// implied, exactly as the collectors do it (schema/bundle.go).
		Clocks:    []schema.ClockOffset{{Source: schema.SourceSite, Method: "assumed_zero"}},
		Redaction: schema.Redaction{Mode: "none"},
		Events:    events,
	}

	mode := redact.ModeNone
	var salt []byte
	if doRedact {
		mode = redact.ModePseudonymize
		var err error
		if salt, err = loadSalt(); err != nil {
			return err
		}
	}
	r, err := redact.New(mode, salt)
	if err != nil {
		return err
	}
	bundle, err = r.Bundle(bundle)
	if err != nil {
		return err
	}
	out, err := bundle.Encode()
	if err != nil {
		return err
	}
	os.Stdout.Write(out)
	return nil
}
