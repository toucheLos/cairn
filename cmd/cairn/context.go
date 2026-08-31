package main

import (
	"crypto/rand"
	"encoding/hex"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/touchelos/cairn/collectors"
	"github.com/touchelos/cairn/join"
	"github.com/touchelos/cairn/redact"
	"github.com/touchelos/cairn/schema"
)

func runContext(args []string) error {
	fs := flag.NewFlagSet("context", flag.ExitOnError)
	var common commonFlags
	common.register(fs)
	var (
		jobRaw   = fs.String("job", "", "job id to investigate (required)")
		format   = fs.String("format", "text", "text | json")
		budgetTk = fs.Int("budget", 4000, "token ceiling for text output; 0 for no limit")
		doRedact = fs.Bool("redact", false, "pseudonymize hosts, users and accounts before output")
		before   = fs.Duration("before", join.DefaultBefore, "how far before the job's first event to look for causes")
		after    = fs.Duration("after", join.DefaultAfter, "how far after the job's last event to look for consequences")
		incClus  = fs.Bool("include-cluster", false, "also include events with neither node nor job id")
		verbose  = fs.Bool("v", false, "show log lines that matched no signature")
	)
	fs.Usage = func() {
		fmt.Fprint(fs.Output(), `usage: cairn context --job <id> [flags]

Assemble every event bearing on one job, from every producer cairn can read,
in a deterministic order. Output is meant to be pasted into a model — or read
directly, which is the point of it working with inference switched off.

Events carrying the job's id are marked *. Everything else was joined by node
and time window: that is the join proposing a connection, not asserting one.
Most of what explains a failure carries no job id at all.

flags:
`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *jobRaw == "" {
		fs.Usage()
		return fmt.Errorf("--job is required")
	}
	job, err := schema.ParseJobID(*jobRaw)
	if err != nil {
		return err
	}
	if *format != "text" && *format != "json" {
		return fmt.Errorf("--format must be text or json, got %q", *format)
	}

	env, err := common.env()
	if err != nil {
		return err
	}
	cluster := common.clusterName()

	req := collectors.Request{Cluster: cluster, Job: job}
	events, results := collectForJob(env, req, *before, *after)

	res := join.ForJob(events, job, join.Options{
		Before:         *before,
		After:          *after,
		IncludeCluster: *incClus,
	})

	// Redaction happens here, at the boundary, on the whole bundle — never per
	// field at a call site (CLAUDE.md §10).
	bundle := schema.Bundle{
		Cluster:   cluster,
		Window:    res.Window,
		Clocks:    assumedClocks(results),
		Redaction: schema.Redaction{Mode: "none"},
		// In the join's relation order, not canonical order, so that the
		// relation labels can be re-attached positionally after redaction.
		// Bundle.Encode sorts a copy canonically, so the JSON on the wire is
		// unaffected by the order they are held in here (§2.7).
		Events: relatedEvents(res.Events),
	}

	mode := redact.ModeNone
	var salt []byte
	if *doRedact {
		mode = redact.ModePseudonymize
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

	if *format == "json" {
		out, err := bundle.Encode()
		if err != nil {
			return err
		}
		os.Stdout.Write(out)
		return nil
	}

	// Re-attach relations to the redacted events. Redaction rewrites values but
	// preserves order, and the bundle was built in the join's order above, so the
	// two streams line up positionally.
	redacted := res
	redacted.Events = make([]join.Related, len(res.Events))
	byOrder := bundle.Events
	for i := range res.Events {
		redacted.Events[i] = join.Related{Event: byOrder[i], Relation: res.Events[i].Relation}
	}
	redacted.Nodes = uniqueNodes(bundle.Events)

	fmt.Print(renderText(redacted, results, bundle.Cluster, bundle.Redaction, renderOpts{
		Budget:  *budgetTk,
		Verbose: *verbose,
	}))
	return nil
}

// assumedClocks records a zero offset for every producer that reported events.
//
// cairn does not measure skew yet, and "assumed_zero" says so on the record
// rather than leaving a reader to assume it was checked. An unstated assumption
// here silently misorders the join (schema/bundle.go).
func assumedClocks(results []collectors.Result) []schema.ClockOffset {
	var out []schema.ClockOffset
	for _, r := range results {
		if len(r.Events) == 0 {
			continue
		}
		out = append(out, schema.ClockOffset{Source: r.Source, Method: "assumed_zero"})
	}
	return out
}

func relatedEvents(rel []join.Related) []schema.Event {
	out := make([]schema.Event, len(rel))
	for i, r := range rel {
		out[i] = r.Event
	}
	return out
}

func uniqueNodes(evs []schema.Event) []schema.Hostname {
	seen := map[schema.Hostname]bool{}
	var out []schema.Hostname
	for _, e := range evs {
		if e.Node != "" && !seen[e.Node] {
			seen[e.Node] = true
			out = append(out, e.Node)
		}
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// saltPath is where the pseudonymization salt lives.
func saltPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "cairn", "salt"), nil
}

// loadSalt reads the site salt, creating one on first use.
//
// This is the one place cairn writes anything, and it is worth being explicit
// about why that does not violate invariant §2.4. That invariant is about the
// cluster: cairn does not drain nodes, edit config, or touch the scheduler. A
// salt is local operator state, and it has to persist — pseudonyms derived from
// a fresh salt every run would make two bundles from the same site disagree
// about which host is which, which is the whole point of having one.
//
// It is announced loudly on creation rather than written quietly, because this
// file is the key that maps pseudonyms back to real hosts. Someone who does not
// know it exists cannot protect it.
func loadSalt() ([]byte, error) {
	if v := os.Getenv("CAIRN_SALT"); v != "" {
		if len(v) < 16 {
			return nil, fmt.Errorf("CAIRN_SALT is %d bytes; at least 16 are needed, "+
				"because a short salt is a reversible hash of the hostname", len(v))
		}
		return []byte(v), nil
	}

	path, err := saltPath()
	if err != nil {
		return nil, fmt.Errorf("locating the salt: %w (set CAIRN_SALT instead)", err)
	}
	if data, err := os.ReadFile(path); err == nil {
		if len(data) < 16 {
			return nil, fmt.Errorf("%s holds only %d bytes; delete it and re-run to "+
				"generate a new one", path, len(data))
		}
		return data, nil
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}

	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return nil, fmt.Errorf("generating a salt: %w", err)
	}
	salt := []byte(hex.EncodeToString(buf))

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("creating %s: %w", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, salt, 0o600); err != nil {
		return nil, fmt.Errorf("writing %s: %w", path, err)
	}

	fmt.Fprintf(os.Stderr, `
cairn: created a new pseudonymization salt at
  %s

This file is the key that maps pseudonyms back to real hostnames. Keep it, back
it up, and never attach it to a bundle you send anywhere — shipping the two
together un-redacts the bundle. Bundles produced with different salts cannot be
compared to each other.

`, path)
	return salt, nil
}
