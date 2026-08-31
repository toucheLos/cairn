package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/touchelos/cairn/schema"
)

// Miss is one recorded case of cairn being wrong.
//
// CLAUDE.md §6 makes this the input to Phase 3: "the miss log drives Phase 3,
// not our intuition." Four weeks of dogfooding produces a list of what cairn
// actually failed at, which is reliably different from the list of what its
// authors imagine it fails at — and only one of those lists is evidence.
type Miss struct {
	// When is supplied by the caller, not read from the clock, so a miss can be
	// recorded against an incident that happened last week.
	When string `json:"when"`

	Cluster string `json:"cluster"`
	Job     string `json:"job,omitempty"`

	// Kind classifies the failure of cairn itself, not of the cluster.
	Kind string `json:"kind"`

	// Expected is the class or cause cairn should have produced. Optional,
	// because "I could not tell what it should have said" is itself a finding.
	Expected string `json:"expected,omitempty"`

	// Note is what actually happened, in the operator's words.
	Note string `json:"note"`
}

// missKinds are the ways cairn can be wrong. Deliberately few: a taxonomy of
// failures that is finer than the evidence supports invites sorting rather than
// thinking.
var missKinds = map[string]string{
	"missed":     "cairn produced nothing, or nothing useful, for a real failure",
	"misclassed": "cairn produced a class, and it was the wrong one",
	"unparsed":   "a producer line carried the answer and no signature matched it",
	"noise":      "cairn produced events that buried the ones that mattered",
	"wrong-join": "the evidence existed but was not connected to the job",
	"unredacted": "identifying material survived redaction",
	"other":      "something else worth recording",
}

func runMiss(args []string) error {
	fs := flag.NewFlagSet("miss", flag.ExitOnError)
	var common commonFlags
	common.register(fs)
	var (
		job      = fs.String("job", "", "job id the miss concerns")
		kind     = fs.String("kind", "", "how cairn was wrong (see below)")
		expected = fs.String("expected", "", "the class or cause cairn should have produced")
		note     = fs.String("note", "", "what actually happened, in your words (required)")
		when     = fs.String("when", "", "when the incident occurred, RFC3339 (required)")
		list     = fs.Bool("list", false, "print the recorded misses instead of adding one")
	)
	fs.Usage = func() {
		fmt.Fprint(fs.Output(), `usage: cairn miss --when <ts> --kind <kind> --note "..." [flags]
       cairn miss --list

Record a case where cairn got it wrong. This log is the input to what gets built
next — CLAUDE.md §6 is explicit that it, and not intuition, decides Phase 3.

Recording a miss is cheap and undercounting is the failure mode. If you are
unsure whether something counts, it counts.

kinds:
`)
		var ks []string
		for k := range missKinds {
			ks = append(ks, k)
		}
		sort.Strings(ks)
		for _, k := range ks {
			fmt.Fprintf(fs.Output(), "  %-12s %s\n", k, missKinds[k])
		}
		fmt.Fprint(fs.Output(), "\nflags:\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}

	path, err := missPath()
	if err != nil {
		return err
	}

	if *list {
		return listMisses(path)
	}

	if *note == "" {
		fs.Usage()
		return fmt.Errorf("--note is required: a miss with no description is not evidence")
	}
	if *when == "" {
		fs.Usage()
		return fmt.Errorf("--when is required, RFC3339 (e.g. 2026-03-04T09:14:02Z)")
	}
	if _, err := schema.ParseTime(normalizeTS(*when)); err != nil {
		return fmt.Errorf("--when %q is not a usable timestamp: %w", *when, err)
	}
	if *kind == "" {
		fs.Usage()
		return fmt.Errorf("--kind is required")
	}
	if _, ok := missKinds[*kind]; !ok {
		return fmt.Errorf("unknown --kind %q; run `cairn miss -h` for the list", *kind)
	}
	if *expected != "" {
		if _, err := schema.ParseClass(*expected); err != nil {
			return fmt.Errorf("--expected: %w", err)
		}
	}
	if *job != "" {
		if _, err := schema.ParseJobID(*job); err != nil {
			return err
		}
	}

	m := Miss{
		When:     normalizeTS(*when),
		Cluster:  string(common.clusterName()),
		Job:      *job,
		Kind:     *kind,
		Expected: *expected,
		Note:     *note,
	}
	line, err := json.Marshal(m)
	if err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Write(append(line, '\n')); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "recorded in %s\n", path)
	return nil
}

// normalizeTS accepts the RFC3339 forms people actually type and renders them
// in cairn's canonical fixed-width form.
func normalizeTS(s string) string {
	s = strings.TrimSpace(s)
	for _, layout := range []string{
		"2006-01-02T15:04:05.000000000Z07:00",
		"2006-01-02T15:04:05Z07:00",
		"2006-01-02T15:04:05",
		"2006-01-02 15:04:05",
		"2006-01-02",
	} {
		if t, err := parseIn(layout, s); err == nil {
			return schema.FormatTime(t)
		}
	}
	return s
}

func listMisses(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Printf("No misses recorded yet (%s).\n\n"+
				"An empty miss log after real use is not a clean bill of health —\n"+
				"it usually means nobody is writing them down.\n", path)
			return nil
		}
		return err
	}
	byKind := map[string]int{}
	n := 0
	for _, line := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var m Miss
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			fmt.Fprintf(os.Stderr, "skipping unparseable record: %v\n", err)
			continue
		}
		n++
		byKind[m.Kind]++
		fmt.Printf("%s  %-12s %s\n", m.When, m.Kind, m.Note)
		if m.Job != "" || m.Expected != "" {
			fmt.Printf("%*s  ", len(m.When), "")
			if m.Job != "" {
				fmt.Printf("job=%s ", m.Job)
			}
			if m.Expected != "" {
				fmt.Printf("expected=%s", m.Expected)
			}
			fmt.Println()
		}
	}
	fmt.Printf("\n%d miss(es) in %s\n", n, path)
	var ks []string
	for k := range byKind {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	for _, k := range ks {
		fmt.Printf("  %-12s %d\n", k, byKind[k])
	}
	return nil
}

func missPath() (string, error) {
	if v := os.Getenv("CAIRN_MISS_LOG"); v != "" {
		return v, nil
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "cairn", "misses.jsonl"), nil
}
