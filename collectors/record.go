package collectors

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// RecordingEnv wraps an Env and remembers everything a collector read.
//
// It exists so `cairn capture` can build a fixture without carrying its own list
// of commands. The alternative — a table of "the commands cairn runs" — is a
// second copy of knowledge that already lives in the collectors, and it would
// drift the first time a collector gained an argument. Here the captured inputs
// are by construction exactly the inputs the collectors asked for.
//
// It records; it does not write. Flush is a separate, explicit step, so nothing
// reaches the disk because a collector happened to run.
type RecordingEnv struct {
	Inner Env

	// calls is every successful read, in the order they happened. Failures are
	// not recorded: a command that did not run produced no evidence, and writing
	// an empty file for it would turn "this host has no InfiniBand" into "this
	// fabric reported nothing", which are opposite findings (§2.6).
	calls []call
}

type call struct {
	// name and args are the command, empty for a file read.
	name string
	args []string
	// path is the file read, empty for a command.
	path string
	out  []byte
}

// NewRecordingEnv returns a RecordingEnv over inner.
func NewRecordingEnv(inner Env) *RecordingEnv { return &RecordingEnv{Inner: inner} }

func (e *RecordingEnv) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	out, err := e.Inner.Run(ctx, name, args...)
	if err == nil {
		e.calls = append(e.calls, call{name: name, args: append([]string(nil), args...), out: out})
	}
	return out, err
}

func (e *RecordingEnv) ReadFile(path string) ([]byte, error) {
	out, err := e.Inner.ReadFile(path)
	if err == nil {
		e.calls = append(e.calls, call{path: path, out: out})
	}
	return out, err
}

func (e *RecordingEnv) Stat(path string) (time.Time, error)  { return e.Inner.Stat(path) }
func (e *RecordingEnv) LookPath(name string) (string, error) { return e.Inner.LookPath(name) }
func (e *RecordingEnv) Location() *time.Location             { return e.Inner.Location() }
func (e *RecordingEnv) Hostname() string                     { return e.Inner.Hostname() }
func (e *RecordingEnv) Getenv(name string) string            { return e.Inner.Getenv(name) }

// Captured reports how many reads were recorded.
func (e *RecordingEnv) Captured() int { return len(e.calls) }

// Flush writes the recorded reads into dir/input, returning the filenames.
//
// Names come from candidates(), the same resolver FixtureEnv uses to find them
// again. That is the whole trick, and it is worth being explicit about why the
// obvious approach fails: the real invocations are
//
//	sacct --parsable2 --noheader --format=… -j 918273
//	journalctl -o short-iso --no-pager --since … --utc
//
// so deriving a name from the argument list directly yields sacct-918273.txt and
// journalctl-short-iso.txt — a job id and an option value baked into filenames
// that the documented convention (fixtures/README.md) says should be sacct.txt
// and journalctl.txt.
//
// Instead each call claims the *least* specific candidate not already holding
// different content, and escalates only on collision. One scontrol call becomes
// scontrol.txt; a second becomes scontrol-show-node.txt, because the first
// already holds the bytes for `scontrol show job`. Replay resolves most-specific
// first and so finds each of them.
//
// Because that reasoning is subtle, Verify replays the result rather than
// trusting it.
func (e *RecordingEnv) Flush(dir string) ([]string, error) {
	inputDir := filepath.Join(dir, "input")
	if err := os.MkdirAll(inputDir, 0o700); err != nil {
		return nil, err
	}

	names, err := e.stems()
	if err != nil {
		return nil, err
	}

	claimed := map[string][]byte{}
	var written []string

	for i, c := range e.calls {
		var name string
		if c.path != "" {
			// A file read resolves by base name, so there is one possible name
			// and no room to escalate.
			name = filepath.Base(c.path)
			if prev, taken := claimed[name]; taken && string(prev) != string(c.out) {
				return nil, fmt.Errorf(
					"two different files both named %q were read; a fixture cannot hold both", name)
			}
		} else {
			name = names[i] + ".txt"
		}

		if _, already := claimed[name]; already {
			continue // identical content under a name already written
		}
		claimed[name] = c.out
		// 0600: this is a real site's producer output until someone redacts it.
		if err := os.WriteFile(filepath.Join(inputDir, name), c.out, 0o600); err != nil {
			return nil, err
		}
		written = append(written, name)
	}

	sort.Strings(written)
	return written, nil
}

// stems chooses a filename stem for each recorded command.
//
// The rule: for each base command, use the shortest prefix of its subcommand
// words that tells all of that command's distinct invocations apart. One sacct
// call gives "sacct"; two scontrol calls give "scontrol-show-job" and
// "scontrol-show-node", which is the convention fixtures/README.md documents.
//
// Escalating greedily per call — take the shortest free name, bump on collision
// — is the obvious approach and it is wrong. It hands one scontrol call
// "scontrol.txt" and the next "scontrol-show.txt", and since "scontrol-show" is
// a prefix of *both* invocations, replay of the first then resolves to the
// second's bytes. Verify caught exactly that, which is the argument for Verify
// existing at all.
//
// Every invocation of a base therefore gets the same depth: names at equal
// specificity cannot shadow one another.
func (e *RecordingEnv) stems() ([]string, error) {
	type parsed struct {
		base  string
		words []string
	}
	p := make([]parsed, len(e.calls))
	byBase := map[string][]int{}
	for i, c := range e.calls {
		if c.path != "" {
			continue
		}
		base, words := commandWords(c.name, c.args)
		p[i] = parsed{base, words}
		byBase[base] = append(byBase[base], i)
	}

	out := make([]string, len(e.calls))
	for base, idx := range byBase {
		maxWords := 0
		for _, i := range idx {
			if n := len(p[i].words); n > maxWords {
				maxWords = n
			}
		}
		depth := -1
		for n := 0; n <= maxWords; n++ {
			byStem := map[string][]byte{}
			ok := true
			for _, i := range idx {
				s := stem(base, p[i].words, n)
				prev, seen := byStem[s]
				if seen && string(prev) != string(e.calls[i].out) {
					ok = false
					break
				}
				byStem[s] = e.calls[i].out
			}
			if ok {
				depth = n
				break
			}
		}
		if depth < 0 {
			// Two invocations that differ only in an argument candidates()
			// discards — a format string, say — but returned different output.
			// No filename can tell them apart, so say so rather than writing a
			// fixture that silently drops one.
			return nil, fmt.Errorf(
				"two %s invocations produced different output but cannot be told apart "+
					"by name; capture them as separate fixtures", base)
		}
		for _, i := range idx {
			out[i] = stem(base, p[i].words, depth)
		}
	}
	return out, nil
}

// Verify replays the flushed directory and checks every recorded read resolves
// to the bytes that were captured.
//
// A capture that cannot be replayed is worthless — worse than worthless, since
// it would sit in the corpus looking like evidence until someone tried to use
// it. The naming rules in Flush are subtle enough that asserting the outcome is
// cheaper than reasoning about them, so this runs at capture time and reports a
// bug there rather than weeks later.
func (e *RecordingEnv) Verify(dir string) error {
	fe := NewFixtureEnv(dir)
	for _, c := range e.calls {
		var got []byte
		var err error
		if c.path != "" {
			got, err = fe.ReadFile(c.path)
		} else {
			got, err = fe.Run(context.Background(), c.name, c.args...)
		}
		if err != nil {
			return fmt.Errorf("captured %s but replay cannot find it: %w", describe(c), err)
		}
		if string(got) != string(c.out) {
			return fmt.Errorf("replay of %s returned different bytes than were captured", describe(c))
		}
	}
	return nil
}

func describe(c call) string {
	if c.path != "" {
		return c.path
	}
	return strings.TrimSpace(c.name + " " + strings.Join(c.args, " "))
}
