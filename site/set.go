package site

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/touchelos/cairn/schema"
)

// Set is the profiles for every cluster an operator runs.
//
// CLAUDE.md §6 asks for "multi-cluster config: one invocation, N clusters", and
// it is worth being precise about which half of that cairn can honestly deliver.
// Reaching N schedulers *live* from one host needs either a daemon or an ssh
// dependency, and invariant §2.5 forecloses both. What a set gives is the
// config: every cluster's profile is loadable at once, so the commands that read
// stored artifacts — doctor, diff, the context header — work across all of them
// in one invocation, and the one command that needs a live scheduler says so
// rather than appearing to have checked clusters it never touched.
type Set struct {
	Profiles []Profile
	// Paths maps a cluster name to the file it was read from, for error
	// messages that name the file an admin has to go and edit.
	Paths map[schema.ClusterName]string
}

// Names returns every cluster in the set, sorted.
func (s Set) Names() []schema.ClusterName {
	out := make([]schema.ClusterName, 0, len(s.Profiles))
	for _, p := range s.Profiles {
		out = append(out, p.Cluster)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// Find returns the profile for one cluster.
func (s Set) Find(name schema.ClusterName) (Profile, bool) {
	for _, p := range s.Profiles {
		if p.Cluster == name {
			return p, true
		}
	}
	return Profile{}, false
}

// Only returns the single profile in a one-cluster set.
//
// The common case by a wide margin: most sites run one cluster and have one
// site.yaml, and should never have to name it.
func (s Set) Only() (Profile, bool) {
	if len(s.Profiles) == 1 {
		return s.Profiles[0], true
	}
	return Profile{}, false
}

// Load reads a site profile or a directory of them.
//
// A path to a file loads that file. A path to a directory loads every *.yaml in
// it, which is how an operator keeps one committed profile per cluster.
func Load(path string) (Set, error) {
	info, err := os.Stat(path)
	if err != nil {
		return Set{}, err
	}
	if !info.IsDir() {
		p, err := loadFile(path)
		if err != nil {
			return Set{}, err
		}
		return Set{Profiles: []Profile{p}, Paths: map[schema.ClusterName]string{p.Cluster: path}}, nil
	}

	entries, err := os.ReadDir(path)
	if err != nil {
		return Set{}, err
	}
	set := Set{Paths: map[schema.ClusterName]string{}}
	var names []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names) // deterministic load order, so errors report in a stable sequence
	for _, name := range names {
		full := filepath.Join(path, name)
		p, err := loadFile(full)
		if err != nil {
			return Set{}, err
		}
		if prev, dup := set.Paths[p.Cluster]; dup {
			// Two profiles claiming one cluster name would make cairn silently
			// pick whichever sorted first, and two sites' evidence would merge.
			return Set{}, fmt.Errorf(
				"cluster %q is defined twice, in %s and %s; cluster names must be unique",
				p.Cluster, prev, full)
		}
		set.Profiles = append(set.Profiles, p)
		set.Paths[p.Cluster] = full
	}
	if len(set.Profiles) == 0 {
		return Set{}, fmt.Errorf("no *.yaml site profiles in %s", path)
	}
	return set, nil
}

func loadFile(path string) (Profile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Profile{}, err
	}
	p, err := DecodeYAML(data)
	if err != nil {
		// Name the file: a set may hold a dozen, and "unknown key" is useless
		// without knowing which one to open.
		return Profile{}, fmt.Errorf("%s: %w", path, err)
	}
	return p, nil
}

// DefaultPaths are searched, in order, when no profile is named explicitly.
//
// Repo-local first: a site.yaml committed next to the work is the one an admin
// just edited, and it should win over a stale copy in their home directory.
func DefaultPaths(configDir string) []string {
	paths := []string{"sites", "site.yaml"}
	if configDir != "" {
		paths = append(paths,
			filepath.Join(configDir, "cairn", "sites"),
			filepath.Join(configDir, "cairn", "site.yaml"))
	}
	return paths
}

// Discovered locates a profile without being told where to look. The second
// return is the path it came from, empty when nothing was found.
//
// Finding nothing is not an error. cairn works with no site profile at all —
// it just cannot tell a model which scheduler this is, and says so.
func Discovered(configDir string) (Set, string, error) {
	for _, p := range DefaultPaths(configDir) {
		if _, err := os.Stat(p); err != nil {
			continue
		}
		set, err := Load(p)
		if err != nil {
			return Set{}, p, err
		}
		return set, p, nil
	}
	return Set{}, "", nil
}
