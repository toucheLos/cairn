package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/touchelos/cairn/policy"
)

// `cairn policy` is show-only. There is no subcommand that executes anything,
// and with the null action set there would be nothing to execute if there were.
//
// It exists so an operator can see the gate before there is ever anything behind
// it. A policy engine nobody has looked at is not "proven correct" in any sense
// that matters, and §6 asks for exactly that ordering.

func runPolicy(args []string) error {
	// `policy log` and `policy check` read naturally as operands, and Go's flag
	// package stops parsing at the first non-flag argument — the same footgun
	// `diff` hit. Lift the subcommand out before parsing.
	sub, rest := splitLeadingOperand(args)

	fs := flag.NewFlagSet("policy", flag.ExitOnError)
	var common commonFlags
	common.register(fs)
	var (
		path     = fs.String("policy", "", "policy file (default $CAIRN_POLICY, else ./policy.yaml)")
		auditLog = fs.String("audit-log", "", "audit log path (default $CAIRN_AUDIT_LOG, else the user config dir)")
		kind     = fs.String("kind", "", "action kind to check")
		node     = fs.String("target-node", "", "node the checked action would act on")
		job      = fs.String("target-job", "", "job the checked action would act on")
		n        = fs.Int("n", 20, "how many audit entries to show")
		example  = fs.Bool("example", false, "print a commented starter policy.yaml to stdout")
	)
	fs.Usage = func() {
		fmt.Fprint(fs.Output(), `usage: cairn policy [show | check | log] [flags]

Show what cairn is permitted to do to this cluster, and why.

  show    the loaded policy and the actions this build can perform (default)
  check   decide a hypothetical action without doing anything or recording it
  log     recent audit entries

cairn is read-only in every code path (CLAUDE.md §2.4). This build ships no
actuations at all: the policy engine is deliberately built and proven against an
empty action set before anything can execute, never the other way around. So
every answer "check" gives is a denial, and that is the demonstration.

flags:
`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(rest); err != nil {
		return err
	}

	if *example {
		out, err := policy.Deny().Encode()
		if err != nil {
			return err
		}
		os.Stdout.Write(out)
		return nil
	}

	p, err := policy.Load(*path)
	if err != nil {
		// Reported, not fatal: a policy that fails to load denies everything, and
		// the operator needs to see both facts at once.
		fmt.Fprintf(os.Stderr, "cairn: %v\n\nThe policy could not be read, so nothing is permitted.\n\n", err)
	}

	sink, err := policy.NewFileSink(*auditLog)
	if err != nil {
		return err
	}

	set, _, err := common.sites()
	if err != nil {
		return err
	}
	profile, err := common.profileFor(set)
	if err != nil {
		return err
	}
	cluster := string(common.clusterNameWith(profile))
	engine := policy.New(p, sink, cluster)

	switch sub {
	case "", "show":
		return showPolicy(engine, p, sink, cluster)
	case "check":
		if *kind == "" {
			return fmt.Errorf("check needs --kind (and usually --target-node or --target-job)")
		}
		d := engine.Evaluate(policy.Request{
			Kind:   policy.Kind(*kind),
			Target: policy.Target{Node: *node, Job: *job},
			Reason: "cairn policy check",
		}, time.Now())
		fmt.Println(d.String())
		fmt.Printf("\nNothing was executed and nothing was recorded: `check` decides only.\n")
		if !d.Allowed {
			// Nonzero so a script can gate on it, and because "would this be
			// allowed?" answered "no" is a negative result.
			os.Exit(1)
		}
		return nil
	case "log":
		return showAudit(sink, *n)
	default:
		fs.Usage()
		return fmt.Errorf("unknown subcommand %q", sub)
	}
}

func showPolicy(e *policy.Engine, p policy.Policy, sink policy.Sink, cluster string) error {
	fmt.Printf("cairn policy — cluster %s\n\n", orNone(cluster))

	// The action set first, because it bounds everything below it. A reader who
	// stops after one line should still come away knowing cairn cannot act.
	acts := e.Actions()
	fmt.Printf("ACTIONS THIS BUILD CAN PERFORM\n")
	if len(acts) == 0 {
		fmt.Printf("  none.\n\n" +
			"  cairn is read-only. The policy engine is built and proven against an\n" +
			"  empty action set first, and the three reversible actuations §6 permits —\n" +
			"  drain node, requeue job, rerun health check — come afterwards. Nothing\n" +
			"  below can permit anything, because there is nothing to permit.\n")
	} else {
		for _, a := range acts {
			level := "unprivileged"
			if a.Privileged {
				level = "privileged"
			}
			fmt.Printf("  %-24s (%s) undone by: %s\n", a.Kind, level, a.Undo)
		}
	}

	fmt.Printf("\nPOLICY\n")
	if p.Path == "" {
		fmt.Printf("  no policy file found — nothing is permitted.\n")
		fmt.Printf("  `cairn policy --example > policy.yaml` writes a starter file.\n")
	} else {
		fmt.Printf("  from %s\n", p.Path)
	}
	fmt.Printf("  allow:   %s\n", orNone(kindList(p.Allow)))
	fmt.Printf("  nodes:   %s\n", orNone(strings.Join(p.Nodes, " ")))
	fmt.Printf("  jobs:    %s\n", orNone(strings.Join(p.Jobs, " ")))
	fmt.Printf("  dry run: %v", p.DryRun)
	if p.DryRun {
		fmt.Printf("  (decide everything, execute nothing)")
	}
	fmt.Println()

	fmt.Printf("\nAUDIT LOG\n  %s\n", sink.Where())
	fmt.Printf("  Every decision is recorded before it takes effect, denials included.\n" +
		"  If the log cannot be written, the action is refused.\n")
	return nil
}

func kindList(ks []policy.Kind) string {
	out := make([]string, len(ks))
	for i, k := range ks {
		out[i] = string(k)
	}
	return strings.Join(out, " ")
}

// showAudit prints recent entries.
//
// It decodes rather than cat-ing the file so that a corrupt line is reported as
// one rather than printed as though it were a record.
func showAudit(sink policy.Sink, n int) error {
	path := sink.Where()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Printf("No decisions recorded yet (%s).\n\n"+
				"That is expected: this build ships no actions, so there has been\n"+
				"nothing to decide.\n", path)
			return nil
		}
		return err
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if n > 0 && len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var rec struct {
			At       string `json:"at"`
			Allowed  bool   `json:"allowed"`
			Executed bool   `json:"executed"`
			Kind     string `json:"kind"`
			Node     string `json:"node"`
			Job      string `json:"job"`
			Gate     string `json:"gate"`
			Explain  string `json:"explain"`
		}
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			fmt.Fprintf(os.Stderr, "unparseable audit record: %v\n", err)
			continue
		}
		verdict := "DENIED "
		if rec.Allowed {
			verdict = "allowed"
			if !rec.Executed {
				verdict = "dry-run"
			}
		}
		target := strings.TrimSpace(rec.Node + " " + rec.Job)
		fmt.Printf("%s  %s  %-20s %s\n", rec.At, verdict, rec.Kind, target)
		if rec.Gate != "" {
			fmt.Printf("%*s  gate=%s\n", len(rec.At), "", rec.Gate)
		}
	}
	fmt.Printf("\n%s\n", path)
	return nil
}
