package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/touchelos/cairn/collectors"
	"github.com/touchelos/cairn/collectors/slurm"
	"github.com/touchelos/cairn/schema"
	"github.com/touchelos/cairn/site"
)

func runDoctor(args []string) error {
	fs := flag.NewFlagSet("doctor", flag.ExitOnError)
	var common commonFlags
	common.register(fs)
	strict := fs.Bool("strict", false,
		"exit nonzero if any capability is unavailable (for CI or a health check)")
	all := fs.Bool("A", false,
		"list every configured cluster's profile, not just this host's")
	fs.Usage = func() {
		fmt.Fprint(fs.Output(), `usage: cairn doctor [flags]

Report what each collector can and cannot see on this host, and what each gap
costs you. A missing capability is not an error: "nvidia-smi not present" is the
correct and complete answer on a CPU-only node.

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
	set, sitePath, err := common.sites()
	if err != nil {
		return err
	}
	siteProfile, err := common.profileFor(set)
	if err != nil && !*all {
		return err
	}
	// doctor asks whether each producer is readable, not what it says. A narrow
	// window keeps that cheap — without one, probing journald means reading it.
	now := time.Now().UTC()
	req := collectors.Request{
		Cluster: common.clusterName(),
		Window:  schema.Window{Start: now.Add(-5 * time.Minute), End: now},
	}
	reg := registry()
	results := reg.Collect(context.Background(), env, req)

	// With several clusters configured and none named, cairn has not resolved a
	// cluster and must not print one. Falling back to "local" here would label
	// this host with a name that belongs to no profile in the set.
	if len(set.Profiles) > 1 && siteProfile.Cluster == "" {
		fmt.Printf("cairn doctor — this host (no cluster named; --cluster selects one)\n")
	} else {
		fmt.Printf("cairn doctor — cluster %s\n", common.clusterNameWith(siteProfile))
	}
	if common.fixture != "" {
		fmt.Printf("replaying fixture %s\n", common.fixture)
	}
	fmt.Printf("schema version %d\n", schema.Version)
	fmt.Print(siteSummary(set, sitePath, *all))
	fmt.Println()
	fmt.Print(collectors.Report(results))

	// Producers with no collector in this build. Reported explicitly, because an
	// admin reading a clean doctor output would otherwise reasonably conclude
	// cairn had looked at their fabric and found nothing wrong.
	have := map[schema.Source]bool{}
	for _, c := range reg {
		have[c.Source()] = true
	}
	var missing []schema.Source
	for _, s := range schema.AllSources() {
		// A derived source has no collector to be missing. Listing `site` here
		// would report a gap that no amount of work could close, and would
		// train an admin to ignore this section (schema/CHANGELOG.md v2).
		if !s.CollectorBacked() {
			continue
		}
		if !have[s] {
			missing = append(missing, s)
		}
	}
	if len(missing) > 0 {
		fmt.Printf("\nnot yet implemented in this build: ")
		for i, s := range missing {
			if i > 0 {
				fmt.Print(", ")
			}
			fmt.Print(s)
		}
		fmt.Printf("\n  cairn does not read these producers at all yet, so nothing here\n" +
			"  reflects their health. Phase 1+ work.\n")
	}

	if *all && len(set.Profiles) > 1 {
		// Said plainly, because the section above it looks like a fleet report
		// and is not. Only this host was read; the other clusters contributed a
		// committed profile and nothing else.
		fmt.Printf("\nOnly this host was probed. The other %d cluster(s) above are\n"+
			"configuration, not observation — cairn reaches no scheduler but the local\n"+
			"one, which is what \"no daemon, no server\" costs. Run doctor on each.\n",
			len(set.Profiles)-1)
	}

	var unavailable int
	for _, r := range results {
		unavailable += len(r.Missing())
	}
	if unavailable > 0 {
		fmt.Printf("\n%d capability(ies) unavailable. That is often correct — see the\n"+
			"\"without it\" lines above to judge whether any of them matter to you.\n", unavailable)
		if *strict {
			os.Exit(1)
		}
	}
	return nil
}

// collectForJob gathers everything bearing on a job, in two passes.
//
// The scheduler is asked first, alone. Its answer is what locates the job in
// time, and every other producer is then read only across that window. Without
// this the journal collector has no bound and reads a login node's entire
// journal — millions of lines covering a year — to answer a question about
// twenty minutes. That is the difference between a tool someone runs and one
// they run once.
//
// If the scheduler knows nothing about the job, the window stays zero and the
// remaining collectors fall back to their own caps. That is the right outcome:
// a job the scheduler has never heard of is a finding, not a reason to go
// trawling.
func collectForJob(env collectors.Env, req collectors.Request,
	before, after time.Duration) ([]schema.Event, []collectors.Result) {

	ctx := context.Background()
	sched := slurm.New()
	schedRes := sched.Collect(ctx, env, req)

	req.Window = windowAround(schedRes.Events, req.Job, before, after)

	results := []collectors.Result{schedRes}
	for _, c := range registry() {
		if c.Source() == sched.Source() {
			continue // already run, and re-running it would double every event
		}
		results = append(results, c.Collect(ctx, env, req))
	}
	sort.SliceStable(results, func(i, j int) bool { return results[i].Source < results[j].Source })
	return collectors.MergeEvents(results), results
}

// windowAround spans the job's own events, widened to catch the cause before and
// the consequence after. Zero if the job has no events.
func windowAround(evs []schema.Event, job *schema.JobID,
	before, after time.Duration) schema.Window {

	var lo, hi time.Time
	for _, e := range evs {
		if job != nil && !e.JobID.SameJob(job) {
			continue
		}
		if lo.IsZero() || e.TS.Before(lo) {
			lo = e.TS
		}
		if e.TS.After(hi) {
			hi = e.TS
		}
	}
	if lo.IsZero() {
		return schema.Window{}
	}
	return schema.Window{Start: lo.Add(-before), End: hi.Add(after)}
}

// siteSummary reports which site profiles are configured.
//
// A cluster name with no profile behind it is worth flagging: it means every
// value a profile would have supplied — scheduler, module system, driver
// generation — is absent from `context`, and the reader will substitute a guess.
func siteSummary(set site.Set, path string, all bool) string {
	if len(set.Profiles) == 0 {
		return "site profile: none — run `cairn init` to write one\n"
	}
	var b strings.Builder
	if len(set.Profiles) == 1 {
		fmt.Fprintf(&b, "site profile: %s (%s)\n", path, schedulerLabel(set.Profiles[0]))
		return b.String()
	}

	fmt.Fprintf(&b, "site profiles: %d clusters from %s\n", len(set.Profiles), path)
	if !all {
		b.WriteString("  (-A to list them)\n")
		return b.String()
	}
	for _, name := range set.Names() {
		p, _ := set.Find(name)
		fmt.Fprintf(&b, "  %-24s %s\n", name, schedulerLabel(p))
	}
	return b.String()
}

// schedulerLabel renders a profile's scheduler for a one-line summary.
//
// "none" is spelled out rather than printed as a bare word, because a profile
// recording that no scheduler was found and a profile that simply has no
// scheduler field would otherwise look identical here — and the first is a
// finding while the second is a malformed file.
func schedulerLabel(p site.Profile) string {
	switch p.Scheduler.Kind {
	case "":
		return "no scheduler recorded — correct this profile by hand"
	case "none":
		return "no scheduler found on the host that was probed"
	}
	if p.Scheduler.Version != "" {
		return p.Scheduler.Kind + " " + p.Scheduler.Version
	}
	return p.Scheduler.Kind
}
