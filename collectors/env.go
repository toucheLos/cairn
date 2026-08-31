package collectors

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Env is everything a collector is allowed to touch outside its own process.
//
// It exists so that every collector can be run against the fixture corpus
// without a cluster, on a laptop, in CI. That is not a testing convenience: the
// corpus is the eval set (CLAUDE.md §0.3), and a collector that can only be
// exercised on real hardware cannot be evaluated at all.
//
// It is also the enforcement point for invariant §2.4, read-only in every code
// path. There is no write, no create, no remove here, and adding one would be
// visible in review as a change to this interface rather than buried in a
// collector.
type Env interface {
	// Run executes a read-only command and returns its stdout.
	//
	// A nonzero exit is returned as an error together with whatever was written
	// to stdout: sacct and nvidia-smi both emit useful output alongside a
	// failure, and discarding it loses evidence.
	Run(ctx context.Context, name string, args ...string) ([]byte, error)

	// ReadFile reads a file.
	ReadFile(path string) ([]byte, error)

	// LookPath reports whether a binary is present and executable.
	LookPath(name string) (string, error)

	// Location is the time zone of the host the output came from.
	//
	// Several producers print local time with no offset — nvidia-smi's header is
	// the obvious one. A collector that assumes UTC silently shifts every one of
	// those events by the host's offset, which corrupts the join without
	// producing a single error. Making the zone an explicit input means a
	// collector cannot avoid deciding what to do about it.
	Location() *time.Location

	// Hostname is the node the collector is running on, already redacted if
	// redaction is active. Empty when it cannot be determined.
	Hostname() string
}

// ErrNotFound is returned by LookPath when a binary is absent. It is an ordinary
// answer, not a failure: "nvidia-smi absent" is the correct and complete result
// on a CPU-only node (invariant §2.6).
var ErrNotFound = errors.New("not found")

// OSEnv is the real environment.
type OSEnv struct {
	// Loc overrides the host time zone. Nil means time.Local.
	Loc *time.Location
	// Timeout bounds every command. Zero means 30s.
	Timeout time.Duration
}

func (e OSEnv) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	timeout := e.Timeout
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, name, args...)
	out, err := cmd.Output()
	if err != nil {
		return out, fmt.Errorf("running %s: %w", name, err)
	}
	return out, nil
}

func (e OSEnv) ReadFile(path string) ([]byte, error) { return os.ReadFile(path) }

func (e OSEnv) LookPath(name string) (string, error) {
	p, err := exec.LookPath(name)
	if err != nil {
		return "", fmt.Errorf("%s: %w", name, ErrNotFound)
	}
	return p, nil
}

func (e OSEnv) Location() *time.Location {
	if e.Loc != nil {
		return e.Loc
	}
	return time.Local
}

func (e OSEnv) Hostname() string {
	h, err := os.Hostname()
	if err != nil {
		return ""
	}
	return h
}

// FixtureEnv replays a fixture's input/ directory.
//
// Commands are resolved to files by name: `sacct ...` reads input/sacct.txt,
// `scontrol show node ...` reads input/scontrol-show-node.txt. That convention
// is what lets a fixture be captured by running the real commands and saving
// their output under the obvious name.
//
// A command with no corresponding file returns ErrNotFound, which a collector
// must treat as an absent capability rather than an error — the same code path a
// CPU-only node takes for nvidia-smi. So the corpus exercises the missing-tool
// path on every fixture that omits a producer, without anything extra being
// written to test it.
type FixtureEnv struct {
	Dir  string // the fixture directory, containing input/
	Loc  *time.Location
	Host string

	// Seen records which inputs were actually consumed, so a test can report
	// fixture files that no collector reads. An unread input is either a missing
	// collector or a fixture nobody finished.
	Seen map[string]bool
}

// NewFixtureEnv returns an Env replaying dir/input.
//
// The zone defaults to UTC. Fixtures whose producers print local time must say
// so explicitly rather than inheriting the developer's laptop, or the corpus
// would produce different events in Berlin than in California.
func NewFixtureEnv(dir string) *FixtureEnv {
	return &FixtureEnv{Dir: dir, Loc: time.UTC, Seen: map[string]bool{}}
}

// candidates returns the input filenames a command could resolve to, most
// specific first: `scontrol show node` prefers scontrol-show-node.txt over
// scontrol.txt.
func candidates(name string, args []string) []string {
	base := filepath.Base(name)
	var words []string
	for _, a := range args {
		if strings.HasPrefix(a, "-") {
			continue
		}
		// Only bare subcommand words participate; values such as a job id or a
		// format string do not name a file.
		if strings.ContainsAny(a, "=,/:") {
			continue
		}
		words = append(words, a)
	}

	var out []string
	for n := len(words); n >= 0; n-- {
		stem := strings.Join(append([]string{base}, words[:n]...), "-")
		for _, ext := range []string{".txt", ".log", ".json", ".xml"} {
			out = append(out, stem+ext)
		}
	}
	return out
}

func (e *FixtureEnv) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	for _, c := range candidates(name, args) {
		path := filepath.Join(e.Dir, "input", c)
		data, err := os.ReadFile(path)
		if err == nil {
			if e.Seen != nil {
				e.Seen[c] = true
			}
			return data, nil
		}
	}
	return nil, fmt.Errorf("%s: %w", name, ErrNotFound)
}

func (e *FixtureEnv) ReadFile(path string) ([]byte, error) {
	// Absolute paths a collector reads on a live node (a log file, say) are
	// resolved to the fixture by base name.
	full := filepath.Join(e.Dir, "input", filepath.Base(path))
	data, err := os.ReadFile(full)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, ErrNotFound)
	}
	if e.Seen != nil {
		e.Seen[filepath.Base(path)] = true
	}
	return data, nil
}

func (e *FixtureEnv) LookPath(name string) (string, error) {
	for _, c := range candidates(name, nil) {
		if _, err := os.Stat(filepath.Join(e.Dir, "input", c)); err == nil {
			return name, nil
		}
	}
	// A binary may still be "present" for a collector that reads log files
	// rather than running the tool; those collectors do not call LookPath.
	return "", fmt.Errorf("%s: %w", name, ErrNotFound)
}

func (e *FixtureEnv) Location() *time.Location {
	if e.Loc != nil {
		return e.Loc
	}
	return time.UTC
}

func (e *FixtureEnv) Hostname() string { return e.Host }

// Unread returns input files no collector consumed, sorted.
func (e *FixtureEnv) Unread() []string {
	entries, err := os.ReadDir(filepath.Join(e.Dir, "input"))
	if err != nil {
		return nil
	}
	var out []string
	for _, ent := range entries {
		if ent.IsDir() || strings.HasPrefix(ent.Name(), ".") {
			continue
		}
		if !e.Seen[ent.Name()] {
			out = append(out, ent.Name())
		}
	}
	sort.Strings(out)
	return out
}
