package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/touchelos/cairn/site"
)

func runInit(args []string) error {
	fs := flag.NewFlagSet("init", flag.ExitOnError)
	var common commonFlags
	common.register(fs)
	var (
		out    = fs.String("o", "site.yaml", "where to write the profile; \"-\" for stdout")
		force  = fs.Bool("force", false, "overwrite an existing profile, discarding any corrections in it")
		dryRun = fs.Bool("n", false, "print the profile that would be written, and write nothing")
	)
	fs.Usage = func() {
		fmt.Fprint(fs.Output(), `usage: cairn init [flags]

Probe this host's stack and write a site profile: scheduler and version, module
system, Spack and EasyBuild roots, distro and kernel, mounts, fabric, GPUs, BMC,
and any telemetry already deployed here.

The result is meant to be read, corrected, and committed to git. cairn guesses,
and a guess nobody has checked is not a site profile. That is also why init
refuses to overwrite an existing file without --force: your corrections are the
point of the file, and clobbering them would defeat it.

The profile becomes the header on "cairn context", which is what stops a model
answering a Slurm question in PBS syntax.

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

	profile := site.Discover(context.Background(), env, common.clusterName())
	data, err := profile.EncodeYAML()
	if err != nil {
		return err
	}

	// Round-trip before writing. The file we hand an admin must be one we can
	// read back — a profile that encodes but fails to decode would be found by
	// the operator, at the worst moment, rather than here.
	if _, err := site.DecodeYAML(data); err != nil {
		return fmt.Errorf("internal: generated a profile cairn cannot read back: %w", err)
	}

	if *out == "-" || *dryRun {
		os.Stdout.Write(data)
		if *dryRun && *out != "-" {
			fmt.Fprintf(os.Stderr, "\n(--n: nothing written to %s)\n", *out)
		}
		reportProbes(profile)
		return nil
	}

	if existing, err := os.ReadFile(*out); err == nil && !*force {
		if bytes.Equal(existing, data) {
			fmt.Printf("%s is already up to date.\n", *out)
			reportProbes(profile)
			return nil
		}
		return existingProfileError(*out, existing, data)
	} else if err != nil && !os.IsNotExist(err) {
		return err
	}

	if dir := filepath.Dir(*out); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	if err := os.WriteFile(*out, data, 0o644); err != nil {
		return err
	}
	fmt.Printf("wrote %s — read it, correct what cairn guessed wrong, and commit it.\n", *out)
	reportProbes(profile)
	return nil
}

// existingProfileError reports what init would change, without changing it.
//
// A bare "file exists, use --force" would push an admin toward --force without
// ever seeing the diff, and the corrections they would lose are the entire value
// of the file. Showing the change makes --force a decision rather than a reflex.
func existingProfileError(path string, existing, generated []byte) error {
	oldP, oldErr := site.DecodeYAML(existing)
	newP, _ := site.DecodeYAML(generated)

	var b bytes.Buffer
	fmt.Fprintf(&b, "%s already exists and differs from what this host probes.\n\n", path)
	if oldErr != nil {
		fmt.Fprintf(&b, "  the existing file does not parse: %v\n\n", oldErr)
	} else {
		diffs := site.CompareProfiles(oldP, newP)
		if len(diffs) == 0 {
			b.WriteString("  the values match; only comments or formatting differ.\n\n")
		} else {
			b.WriteString("  in file                          probed now\n")
			for _, d := range diffs {
				fmt.Fprintf(&b, "  %-32s %s\n", d.Key+": "+orNone(d.Recorded), orNone(d.Probed))
			}
			b.WriteString("\n")
		}
	}
	b.WriteString("If the file is right and the probe is wrong, leave it alone.\n")
	b.WriteString("If the probe is right, re-run with --force. Your edits will be lost.\n")
	return fmt.Errorf("%s", b.String())
}

func orNone(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}

// reportProbes prints what discovery could not see, in the shape `doctor` uses.
//
// A profile that simply omitted the sections it could not fill would read as a
// simple site rather than an incompletely probed one, and those need opposite
// responses from the admin.
func reportProbes(p site.Profile) {
	missing := p.Missing()
	if len(missing) == 0 {
		return
	}
	fmt.Printf("\nWHAT CAIRN COULD NOT PROBE\n")
	for _, pr := range missing {
		fmt.Printf("  %s (%s) — %s\n", pr.Name, pr.Level, pr.Detail)
		if pr.Reveals != "" {
			fmt.Printf("      lost: %s\n", pr.Reveals)
		}
	}
	fmt.Printf("\n%d of these are often correct — a CPU-only site has no GPU stack. Fill in\n"+
		"by hand anything cairn could not reach but you know to be there.\n", len(missing))
}
