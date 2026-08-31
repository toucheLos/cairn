package main

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/touchelos/cairn/collectors"
	"github.com/touchelos/cairn/join"
	"github.com/touchelos/cairn/schema"
)

// estimateTokens approximates the token cost of a string.
//
// Four bytes per token is the usual rough figure for English prose mixed with
// identifiers, and it is an estimate, not a measurement. cairn deliberately does
// not vendor a tokenizer: that would tie the binary to one model family's
// vocabulary, and invariant §2.1 is that everything works with inference
// switched off. The budget is a guard rail, so erring high is the safe
// direction — a bundle that comes in slightly under is fine, one that silently
// blows the context window is not.
func estimateTokens(s string) int {
	return (len(s) + 3) / 4
}

// row is one rendered timeline entry, possibly collapsing a run of identical
// consecutive events.
type row struct {
	ev       schema.Event
	relation join.Relation
	count    int       // 1 unless a run was collapsed
	last     time.Time // end of the collapsed run
	// order is the index in canonical order, used as the final tiebreak when
	// deciding what to drop so that two runs drop the same rows.
	order int
}

// relationMark renders how an event reached this result. Direct evidence is
// marked because the distinction is load-bearing: a direct event is a fact about
// the job, a node-scoped one is the join proposing a connection. Presenting them
// identically invites a confident conclusion from a coincidence.
func relationMark(r join.Relation) string {
	switch r {
	case join.RelDirect:
		return "*"
	case join.RelCluster:
		return "~"
	default:
		return " "
	}
}

// collapse merges runs of *consecutive* identical events into one row.
//
// Only consecutive runs, never scattered duplicates. On an InfiniBand port
// flapping down/up/down/up/down, the alternation is the diagnosis — merging the
// three "down" events across the two "up"s between them would destroy exactly
// the evidence that distinguishes a failing cable from a single reconfiguration.
func collapse(rel []join.Related) []row {
	var out []row
	key := func(e schema.Event, r join.Relation) string {
		var b strings.Builder
		b.WriteString(string(r))
		b.WriteByte('\x00')
		b.WriteString(string(e.Source))
		b.WriteByte('\x00')
		b.WriteString(string(e.Class))
		b.WriteByte('\x00')
		b.WriteString(e.Detail.Signature)
		b.WriteByte('\x00')
		b.WriteString(string(e.Node))
		b.WriteByte('\x00')
		b.WriteString(e.JobID.RawOrEmpty())
		b.WriteByte('\x00')
		keys := make([]string, 0, len(e.Detail.Attrs))
		for k := range e.Detail.Attrs {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			b.WriteString(k)
			b.WriteByte('=')
			b.WriteString(e.Detail.Attrs[k])
			b.WriteByte('\x00')
		}
		return b.String()
	}

	var prevKey string
	for i, r := range rel {
		k := key(r.Event, r.Relation)
		if len(out) > 0 && k == prevKey {
			out[len(out)-1].count++
			out[len(out)-1].last = r.Event.TS
			continue
		}
		out = append(out, row{ev: r.Event, relation: r.Relation, count: 1, last: r.Event.TS, order: i})
		prevKey = k
	}
	return out
}

func attrString(e schema.Event) string {
	if len(e.Detail.Attrs) == 0 {
		return ""
	}
	keys := make([]string, 0, len(e.Detail.Attrs))
	for k := range e.Detail.Attrs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+e.Detail.Attrs[k])
	}
	return strings.Join(parts, " ")
}

type renderOpts struct {
	// Budget is a token ceiling for the whole report. Zero means unlimited.
	Budget int
	// Verbose includes collector warnings — the lines no signature matched.
	Verbose bool
}

// renderText produces the report meant to be pasted into a model.
//
// Ordering is entirely inherited from the join, which is a total order, so two
// runs over the same window produce byte-identical output (§2.7).
func renderText(res join.Result, results []collectors.Result, cluster schema.ClusterName,
	redaction schema.Redaction, opts renderOpts) string {

	var b strings.Builder

	// --- header ---
	fmt.Fprintf(&b, "cairn context — job %s on %s\n", res.Job.RawOrEmpty(), cluster)
	direct := len(res.Direct())
	fmt.Fprintf(&b, "schema v%d · %d events (%d carry this job's id) · %d node(s)\n",
		schema.Version, len(res.Events), direct, len(res.Nodes))
	if !res.Window.Start.IsZero() {
		fmt.Fprintf(&b, "window %s .. %s (UTC)\n",
			res.Window.Start.Format("2006-01-02 15:04:05"),
			res.Window.End.Format("2006-01-02 15:04:05"))
	}
	if redaction.Mode == "pseudonymize" {
		fmt.Fprintf(&b, "redaction: pseudonymized, salt %s\n", redaction.SaltID)
	} else {
		fmt.Fprintf(&b, "redaction: none — this bundle contains real host and account names\n")
	}
	if len(res.Nodes) > 0 {
		fmt.Fprintf(&b, "nodes: %s\n", joinNodes(res.Nodes))
	}

	if len(res.Events) == 0 {
		b.WriteString("\nNo events. Either nothing recorded this job, or the collectors could\n" +
			"not see the producers that would have. Check `cairn doctor`.\n")
		b.WriteString(renderCapabilities(results))
		return b.String()
	}

	// The capability section is rendered first and reserved out of the budget.
	// What cairn could NOT see must never be dropped to make room for more of
	// what it could: a truncated evidence list still reads as complete if the
	// gaps are missing.
	caps := renderCapabilities(results)
	reserved := estimateTokens(b.String()) + estimateTokens(caps) + 64 // 64 for the trailer

	rows := collapse(res.Events)
	kept, dropped := budget(rows, opts.Budget, reserved)

	// --- timeline ---
	b.WriteString("\nTIMELINE   * = carries this job's id · blank = node-scoped evidence · ~ = cluster-scoped\n")
	b.WriteString(renderRows(kept))

	if dropped > 0 {
		fmt.Fprintf(&b, "\n%d node-scoped event(s) omitted to fit a %d-token budget, furthest\n"+
			"from this job's own events first. Re-run with --budget 0 for everything.\n",
			dropped, opts.Budget)
	}

	b.WriteString(caps)

	if opts.Verbose {
		b.WriteString(renderWarnings(results))
	} else if n := countWarnings(results); n > 0 {
		fmt.Fprintf(&b, "\n%d log line(s) matched no known signature. Re-run with -v to see them —\n"+
			"they are the miss log that decides what gets built next.\n", n)
	}

	return b.String()
}

func renderRows(rows []row) string {
	if len(rows) == 0 {
		return "  (none)\n"
	}
	// Column widths from the data, so short reports stay narrow.
	var wSrc, wCls int
	for _, r := range rows {
		wSrc = max(wSrc, len(r.ev.Source))
		wCls = max(wCls, len(r.ev.Class))
	}

	var b strings.Builder
	day := ""
	for _, r := range rows {
		if d := r.ev.TS.Format("2006-01-02"); d != day {
			fmt.Fprintf(&b, "  [%s]\n", d)
			day = d
		}
		fmt.Fprintf(&b, "  %s %s %-*s %-*s %s",
			r.ev.TS.Format("15:04:05"), relationMark(r.relation),
			wSrc, r.ev.Source, wCls, r.ev.Class, r.ev.Detail.Signature)
		if r.count > 1 {
			fmt.Fprintf(&b, " ×%d through %s", r.count, r.last.Format("15:04:05"))
		}
		if job := r.ev.JobID.RawOrEmpty(); job != "" && r.relation == join.RelDirect {
			fmt.Fprintf(&b, " job=%s", job)
		}
		if a := attrString(r.ev); a != "" {
			fmt.Fprintf(&b, " %s", a)
		}
		b.WriteByte('\n')
	}
	return b.String()
}

// budget drops rows until the timeline fits, and reports how many went.
//
// Direct events are never dropped — they are the answer to the question that was
// asked, and a report that omits them to fit a limit has failed at its only job.
// Among node-scoped rows, the ones furthest in time from the job's own events go
// first: correlation weakens with distance, so that is where the least evidence
// per token sits.
func budget(rows []row, budgetTokens, reserved int) (kept []row, dropped int) {
	if budgetTokens <= 0 {
		return rows, 0
	}
	avail := budgetTokens - reserved
	if avail < 0 {
		avail = 0
	}
	if estimateTokens(renderRows(rows)) <= avail {
		return rows, 0
	}

	// Anchor: the time span of the direct events.
	var lo, hi time.Time
	for _, r := range rows {
		if r.relation != join.RelDirect {
			continue
		}
		if lo.IsZero() || r.ev.TS.Before(lo) {
			lo = r.ev.TS
		}
		if r.ev.TS.After(hi) {
			hi = r.ev.TS
		}
	}
	distance := func(r row) time.Duration {
		if lo.IsZero() {
			return 0
		}
		switch {
		case r.ev.TS.Before(lo):
			return lo.Sub(r.ev.TS)
		case r.ev.TS.After(hi):
			return r.ev.TS.Sub(hi)
		default:
			return 0
		}
	}

	// Candidates for removal, furthest first; canonical order breaks ties so the
	// same rows drop on every run.
	cand := make([]int, 0, len(rows))
	for i, r := range rows {
		if r.relation != join.RelDirect {
			cand = append(cand, i)
		}
	}
	sort.SliceStable(cand, func(a, b int) bool {
		da, db := distance(rows[cand[a]]), distance(rows[cand[b]])
		if da != db {
			return da > db
		}
		return rows[cand[a]].order > rows[cand[b]].order
	})

	drop := map[int]bool{}
	for _, i := range cand {
		drop[i] = true
		dropped++
		var remaining []row
		for j, r := range rows {
			if !drop[j] {
				remaining = append(remaining, r)
			}
		}
		if estimateTokens(renderRows(remaining)) <= avail {
			return remaining, dropped
		}
	}

	// Only direct events left and still over budget. Keep them: exceeding the
	// budget is visible and recoverable, silently dropping the answer is not.
	for j, r := range rows {
		if !drop[j] {
			kept = append(kept, r)
		}
	}
	return kept, dropped
}

func renderCapabilities(results []collectors.Result) string {
	var b strings.Builder
	b.WriteString("\nWHAT CAIRN COULD NOT SEE\n")
	any := false
	for _, r := range results {
		for _, c := range r.Missing() {
			any = true
			fmt.Fprintf(&b, "  %s/%s (%s)", r.Source, c.Name, c.Level)
			if c.Detail != "" {
				fmt.Fprintf(&b, " — %s", c.Detail)
			}
			b.WriteByte('\n')
			if c.Reveals != "" {
				fmt.Fprintf(&b, "      lost: %s\n", c.Reveals)
			}
		}
	}
	if !any {
		b.WriteString("  (nothing — every collector saw everything it asked for)\n")
	}
	return b.String()
}

func countWarnings(results []collectors.Result) int {
	n := 0
	for _, r := range results {
		n += len(r.Warnings)
	}
	return n
}

func renderWarnings(results []collectors.Result) string {
	if countWarnings(results) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\nUNMATCHED (the miss log — these drive what gets built next)\n")
	for _, r := range results {
		for _, w := range r.Warnings {
			fmt.Fprintf(&b, "  [%s] %s\n", r.Source, w)
		}
	}
	return b.String()
}

func joinNodes(ns []schema.Hostname) string {
	parts := make([]string, len(ns))
	for i, n := range ns {
		parts[i] = string(n)
	}
	return strings.Join(parts, ", ")
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
