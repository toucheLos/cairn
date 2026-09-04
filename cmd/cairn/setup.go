package main

import (
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/touchelos/cairn/collectors"
	"github.com/touchelos/cairn/collectors/fabric"
	"github.com/touchelos/cairn/collectors/gpu"
	"github.com/touchelos/cairn/collectors/journal"
	"github.com/touchelos/cairn/collectors/slurm"
	"github.com/touchelos/cairn/schema"
	"github.com/touchelos/cairn/site"
)

// registry is the set of collectors this build knows how to run.
//
// slurm, journal and gpu emit events. fabric reports what it can see and emits
// none — ibstat carries no timestamp, so its snapshot becomes node-profile drift
// rather than a timeline entry (see collectors/fabric). It is registered anyway
// because `doctor` must be able to distinguish a fabric it cannot read from one
// cairn does not implement; before this they looked identical.
//
// storage and bmc still have no collector, and `doctor` says so rather than
// leaving their absence to be inferred from silence.
func registry() collectors.Registry {
	return collectors.Registry{slurm.New(), journal.New(), gpu.New(), fabric.New()}
}

// commonFlags are shared by every command that collects.
type commonFlags struct {
	cluster string
	fixture string
	tz      string
	site    string
}

func (c *commonFlags) register(fs *flag.FlagSet) {
	fs.StringVar(&c.cluster, "cluster", "",
		"cluster name stamped on every event (default $CAIRN_CLUSTER, else \"local\")")
	fs.StringVar(&c.fixture, "fixture", "",
		"replay a fixture directory instead of reading the live system")
	fs.StringVar(&c.tz, "tz", "",
		"IANA zone of the host that produced the output, e.g. America/New_York\n"+
			"(default: this host's local zone; several producers print local time with no offset)")
	fs.StringVar(&c.site, "site", "",
		"site profile, or a directory of them (default $CAIRN_SITE, else ./sites,\n"+
			"./site.yaml, then the user config dir). Written by `cairn init`.")
}

// sites loads the configured site profiles.
//
// Finding none is not an error and must not be treated as one: cairn works with
// no profile at all, it simply cannot tell a reader which scheduler this is. The
// returned path is empty in that case, and the commands say so rather than
// printing a header built on a guess.
func (c *commonFlags) sites() (site.Set, string, error) {
	explicit := c.site
	if explicit == "" {
		explicit = os.Getenv("CAIRN_SITE")
	}
	if explicit != "" {
		// Named explicitly, so a missing file is a real error — the operator
		// asked for that profile and did not get it.
		set, err := site.Load(explicit)
		if err != nil {
			return site.Set{}, explicit, err
		}
		return set, explicit, nil
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		dir = ""
	}
	return site.Discovered(dir)
}

// profileFor picks the profile a command should use.
//
// With one profile it is used without being named — most sites run one cluster
// and should never have to say which. With several, --cluster selects; an
// ambiguous set is an error rather than a guess, because silently picking one
// would stamp the wrong cluster name on a bundle.
func (c *commonFlags) profileFor(set site.Set) (site.Profile, error) {
	if len(set.Profiles) == 0 {
		return site.Profile{}, nil
	}
	name := c.cluster
	if name == "" {
		name = os.Getenv("CAIRN_CLUSTER")
	}
	if name != "" {
		p, ok := set.Find(schema.ClusterName(name))
		if !ok {
			return site.Profile{}, fmt.Errorf(
				"no profile for cluster %q; this set has %s", name, clusterList(set))
		}
		return p, nil
	}
	if p, ok := set.Only(); ok {
		return p, nil
	}
	return site.Profile{}, fmt.Errorf(
		"this set defines %d clusters (%s); name one with --cluster",
		len(set.Profiles), clusterList(set))
}

func clusterList(set site.Set) string {
	names := set.Names()
	parts := make([]string, len(names))
	for i, n := range names {
		parts[i] = string(n)
	}
	return strings.Join(parts, ", ")
}

// clusterName resolves the cluster stamped on events, without a site profile.
//
// Never guessed from the hostname: a wrong cluster name silently makes two
// sites' bundles look like one site's, and nothing downstream can detect it.
// "local" is deliberately a non-name — it reads as unconfigured, which it is.
func (c *commonFlags) clusterName() schema.ClusterName {
	if c.cluster != "" {
		return schema.ClusterName(c.cluster)
	}
	if v := os.Getenv("CAIRN_CLUSTER"); v != "" {
		return schema.ClusterName(v)
	}
	return "local"
}

// clusterNameWith resolves the cluster, preferring a site profile.
//
// This is the Phase 3 half of the TODO the comment above used to carry. An
// explicit --cluster still wins: an operator correcting cairn is more reliable
// than a probe, and site.yaml is itself a file they are expected to correct.
func (c *commonFlags) clusterNameWith(p site.Profile) schema.ClusterName {
	if c.cluster != "" {
		return schema.ClusterName(c.cluster)
	}
	if p.Cluster != "" {
		return p.Cluster
	}
	return c.clusterName()
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
