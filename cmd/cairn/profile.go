package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/touchelos/cairn/schema"
	"github.com/touchelos/cairn/site"
)

func runProfile(args []string) error {
	fs := flag.NewFlagSet("profile", flag.ExitOnError)
	var common commonFlags
	common.register(fs)
	var (
		nodeName = fs.String("node", "", "node name to record (default: this host's name)")
		at       = fs.String("at", "", "capture time, RFC3339 (default: now; set it to make a run reproducible)")
	)
	fs.Usage = func() {
		fmt.Fprint(fs.Output(), `usage: cairn profile [flags] > profiles/<node>.json

Capture this node's configuration drift keys — driver generation, kernel release
and cmdline, glibc, module roots, mount set, munge key mtime — as canonical JSON
on stdout. Feed a directory of these to "cairn diff".

cairn does not fan out. Use whatever your site already runs:

  srun -w node[001-048] --ntasks-per-node=1 \
      sh -c 'cairn profile > profiles/$(hostname -s).json'
  pdsh -w node[001-048] cairn profile
  clush -w @compute cairn profile

Shipping our own remote execution would mean an ssh dependency and a second
read-only boundary to get right, for something every site already has.

Reads nothing privileged. The munge key is stat'ed for its mtime, never opened.

The output carries this node's real hostname, because that is what "cairn diff"
joins on. Redaction happens on diff's output, not here — so treat a profiles
directory as site-internal, and use "cairn diff --redact" for anything you send
outside.

flags:
`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}

	env, err := common.env()
	if err != nil {
		return err
	}

	// A site profile is consulted only for the cluster name. Everything else in
	// a node profile has to come from this node, or the comparison is between
	// what a node is and what a file says it should be — which is drift from
	// intent, a different and less reliable question than drift from siblings.
	set, _, err := common.sites()
	if err != nil {
		return err
	}
	sp, err := common.profileFor(set)
	if err != nil {
		return err
	}
	cluster := common.clusterNameWith(sp)

	node := schema.Hostname(*nodeName)
	if node == "" {
		node = schema.Hostname(env.Hostname())
	}
	if node == "" {
		return fmt.Errorf("could not determine this node's name; pass --node")
	}

	now := time.Now()
	if *at != "" {
		t, err := schema.ParseTime(normalizeTS(*at))
		if err != nil {
			return fmt.Errorf("--at %q is not a usable timestamp: %w", *at, err)
		}
		now = t
	}

	np := site.CaptureNode(context.Background(), env, cluster, node, now)
	data, err := np.EncodeJSON()
	if err != nil {
		return err
	}
	os.Stdout.Write(data)

	// Gaps go to stderr so that redirecting stdout into a profile directory
	// still shows the operator what this node could not report — and does not
	// corrupt the JSON with them.
	for _, pr := range np.Probes {
		if pr.Available {
			continue
		}
		fmt.Fprintf(os.Stderr, "%s: %s (%s) — %s\n", node, pr.Name, pr.Level, pr.Detail)
	}
	return nil
}
