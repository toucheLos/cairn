package collectors_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/touchelos/cairn/collectors"
)

// fakeEnv answers a fixed set of commands, so a recording test does not need a
// cluster or a real binary on PATH.
type fakeEnv struct {
	collectors.OSEnv
	cmds  map[string][]byte
	files map[string][]byte
}

func (f fakeEnv) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	key := name
	for _, a := range args {
		key += " " + a
	}
	if out, ok := f.cmds[key]; ok {
		return out, nil
	}
	return nil, collectors.ErrNotFound
}

func (f fakeEnv) ReadFile(path string) ([]byte, error) {
	if out, ok := f.files[path]; ok {
		return out, nil
	}
	return nil, collectors.ErrNotFound
}

func (f fakeEnv) LookPath(string) (string, error) { return "", collectors.ErrNotFound }
func (f fakeEnv) Hostname() string                { return "node-0001" }
func (f fakeEnv) Getenv(string) string            { return "" }

// TestRecordingRoundTrip is the property `cairn capture` depends on: whatever a
// collector read on a live host must resolve to the same bytes on replay.
//
// The naming rules in Flush are subtle — a job id and an option value both look
// like subcommand words to candidates() — so this asserts the outcome rather
// than the reasoning.
func TestRecordingRoundTrip(t *testing.T) {
	// The real invocations, verbatim. sacct carries a job id and journalctl
	// carries `-o short-iso`, which are exactly the arguments that would end up
	// in a filename if names were derived from the argument list.
	inner := fakeEnv{
		cmds: map[string][]byte{
			"sacct --parsable2 --noheader --format=JobID,State -j 918273": []byte("918273|FAILED\n"),
			"journalctl -o short-iso --no-pager --lines 10000":            []byte("journal body\n"),
			"scontrol show job 918273":                                    []byte("JobId=918273\n"),
			"scontrol show node node-0001":                                []byte("NodeName=node-0001\n"),
			"nvidia-smi":                                                  []byte("smi body\n"),
		},
		files: map[string][]byte{"/var/log/slurm/slurmd.log": []byte("slurmd body\n")},
	}

	rec := collectors.NewRecordingEnv(inner)
	ctx := context.Background()
	for cmdline := range inner.cmds {
		var name string
		var args []string
		for i, w := range splitWords(cmdline) {
			if i == 0 {
				name = w
				continue
			}
			args = append(args, w)
		}
		if _, err := rec.Run(ctx, name, args...); err != nil {
			t.Fatalf("%s: %v", cmdline, err)
		}
	}
	if _, err := rec.ReadFile("/var/log/slurm/slurmd.log"); err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	written, err := rec.Flush(dir)
	if err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if err := rec.Verify(dir); err != nil {
		t.Fatalf("Verify: %v", err)
	}

	// The documented convention (fixtures/README.md) is sacct.txt and
	// journalctl.txt — not sacct-918273.txt or journalctl-short-iso.txt.
	got := map[string]bool{}
	for _, w := range written {
		got[w] = true
	}
	for _, want := range []string{"sacct.txt", "journalctl.txt", "nvidia-smi.txt", "slurmd.log"} {
		if !got[want] {
			t.Errorf("expected %s among the captured files, got %v", want, written)
		}
	}
	for _, unwanted := range []string{"sacct-918273.txt", "journalctl-short-iso.txt"} {
		if got[unwanted] {
			t.Errorf("%s bakes an argument value into the filename: %v", unwanted, written)
		}
	}
	// Two scontrol calls with different output cannot share one name.
	if len(written) != 6 {
		t.Errorf("expected 6 distinct files, got %d: %v", len(written), written)
	}
}

// TestRecordingSkipsFailures: a command that did not run produced no evidence.
// Writing an empty file for it would turn "this host has no InfiniBand" into
// "the fabric reported nothing", which are opposite findings (§2.6).
func TestRecordingSkipsFailures(t *testing.T) {
	rec := collectors.NewRecordingEnv(fakeEnv{cmds: map[string][]byte{}})
	if _, err := rec.Run(context.Background(), "ibstat"); err == nil {
		t.Fatal("expected the absent command to error")
	}
	if rec.Captured() != 0 {
		t.Errorf("a failed command was recorded: %d call(s)", rec.Captured())
	}
	dir := t.TempDir()
	written, err := rec.Flush(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(written) != 0 {
		t.Errorf("expected nothing written, got %v", written)
	}
}

// TestRecordingCapturePermissions: a capture is a real site's producer output
// until somebody redacts it, so it must not be world-readable.
func TestRecordingCapturePermissions(t *testing.T) {
	inner := fakeEnv{cmds: map[string][]byte{"sacct": []byte("body\n")}}
	rec := collectors.NewRecordingEnv(inner)
	if _, err := rec.Run(context.Background(), "sacct"); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if _, err := rec.Flush(dir); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(filepath.Join(dir, "input", "sacct.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm&0o077 != 0 {
		t.Errorf("captured file is group/world readable: %o", perm)
	}
}

func splitWords(s string) []string {
	var out []string
	cur := ""
	for _, r := range s {
		if r == ' ' {
			if cur != "" {
				out = append(out, cur)
				cur = ""
			}
			continue
		}
		cur += string(r)
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}
