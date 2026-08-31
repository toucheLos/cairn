package main

import (
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/touchelos/cairn/collectors"
	"github.com/touchelos/cairn/collectors/gpu"
	"github.com/touchelos/cairn/collectors/journal"
	"github.com/touchelos/cairn/collectors/slurm"
	"github.com/touchelos/cairn/schema"
)

// registry is the set of collectors this build knows how to run.
//
// Phase 1 scope (CLAUDE.md §6): slurm, journal, gpu. fabric, storage, and bmc
// have no collector yet, and `doctor` says so rather than leaving their absence
// to be inferred from silence.
func registry() collectors.Registry {
	return collectors.Registry{slurm.New(), journal.New(), gpu.New()}
}

// commonFlags are shared by every command that collects.
type commonFlags struct {
	cluster string
	fixture string
	tz      string
}

func (c *commonFlags) register(fs *flag.FlagSet) {
	fs.StringVar(&c.cluster, "cluster", "",
		"cluster name stamped on every event (default $CAIRN_CLUSTER, else \"local\")")
	fs.StringVar(&c.fixture, "fixture", "",
		"replay a fixture directory instead of reading the live system")
	fs.StringVar(&c.tz, "tz", "",
		"IANA zone of the host that produced the output, e.g. America/New_York\n"+
			"(default: this host's local zone; several producers print local time with no offset)")
}

// clusterName resolves the cluster stamped on events.
//
// Phase 3 replaces this with site.yaml, which discovers the real name. Until
// then it is explicit or "local" — never guessed from the hostname, because a
// wrong cluster name silently makes two sites' bundles look like one site's.
func (c *commonFlags) clusterName() schema.ClusterName {
	if c.cluster != "" {
		return schema.ClusterName(c.cluster)
	}
	if v := os.Getenv("CAIRN_CLUSTER"); v != "" {
		return schema.ClusterName(v)
	}
	return "local"
}

func (c *commonFlags) location() (*time.Location, error) {
	if c.tz == "" {
		return time.Local, nil
	}
	loc, err := time.LoadLocation(c.tz)
	if err != nil {
		return nil, fmt.Errorf("unknown time zone %q: %w", c.tz, err)
	}
	return loc, nil
}

// env builds the environment collectors read through.
//
// The fixture path is not a test-only convenience. It is how an admin replays a
// bundle someone else sent them, and how a miss gets reproduced offline after
// the cluster has moved on (CLAUDE.md §7, shareable incident bundles).
func (c *commonFlags) env() (collectors.Env, error) {
	loc, err := c.location()
	if err != nil {
		return nil, err
	}
	if c.fixture != "" {
		if _, err := os.Stat(c.fixture); err != nil {
			return nil, fmt.Errorf("fixture directory: %w", err)
		}
		fe := collectors.NewFixtureEnv(c.fixture)
		fe.Loc = loc
		fe.Host = os.Getenv("CAIRN_NODE")
		return fe, nil
	}
	return collectors.OSEnv{Loc: loc}, nil
}
