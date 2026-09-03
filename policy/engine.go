package policy

import (
	"fmt"
	"time"
)

// errInvalid is the package's validation error.
func errInvalid(format string, args ...any) error {
	return fmt.Errorf("policy: "+format, args...)
}

// Gate names the checks a request must pass, in the order they are applied.
//
// Every one is default-deny, and the first refusal is what gets recorded. The
// order is not arbitrary: the cheapest and most absolute checks come first, so
// that a request for something this build cannot do is refused before anything
// consults operator configuration.
const (
	GateUnknownAction = "unknown_action"
	GateNotAllowed    = "not_allowed"
	GateOutOfScope    = "out_of_scope"
	GateIrreversible  = "irreversible"
	GateNoAudit       = "no_audit"
	GateClusterName   = "wrong_cluster"
)

// Engine decides whether a proposed action may run.
//
// The action set is a field rather than a package-level registry, and there is
// no exported way to add to it. Production builds one from ShippedActions(),
// which is empty; a test builds one with its own fake action. So there is no
// code path — in tests or anywhere else — that can add an actuation to an engine
// running against a real cluster.
type Engine struct {
	actions map[Kind]ActionSpec
	policy  Policy
	audit   Sink
	cluster string
}

// New returns an engine over the actions this build ships: none.
//
// The signature takes no action set on purpose. A caller cannot widen what cairn
// can do by passing a different one, because there is no parameter to pass.
func New(p Policy, audit Sink, cluster string) *Engine {
	return newWithActions(ShippedActions(), p, audit, cluster)
}

// newWithActions is the constructor tests use. Unexported: an actuation reaches
// a production engine only by being added to shippedActions, which is a source
// change that fails a guard.
func newWithActions(specs []ActionSpec, p Policy, audit Sink, cluster string) *Engine {
	m := make(map[Kind]ActionSpec, len(specs))
	for _, s := range specs {
		m[s.Kind] = s
	}
	return &Engine{actions: m, policy: p, audit: audit, cluster: cluster}
}

// Policy returns the policy this engine was built with.
func (e *Engine) Policy() Policy { return e.policy }

// Actions returns what this engine can perform, sorted.
func (e *Engine) Actions() []ActionSpec {
	out := make([]ActionSpec, 0, len(e.actions))
	for _, s := range e.actions {
		out = append(out, s)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].Kind < out[j-1].Kind; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// Evaluate decides a request without executing anything and without recording.
//
// This is what `cairn policy check` calls. It is separate from Execute so that
// asking "would this be allowed?" is provably incapable of doing anything, and
// so that an operator exploring a policy does not fill the audit log with
// hypotheticals.
func (e *Engine) Evaluate(req Request, now time.Time) Decision {
	d := Decision{
		At:     now.UTC(),
		Kind:   req.Kind,
		Target: req.Target,
		Reason: req.Reason,
		DryRun: e.policy.DryRun,
	}

	// 1. Does this build implement the action at all?
	//
	// First because it is the only check that does not depend on configuration.
	// With the null action set every request stops here, which is exactly what
	// this phase is meant to demonstrate.
	spec, known := e.actions[req.Kind]
	if !known {
		d.Gate = GateUnknownAction
		d.Explain = fmt.Sprintf(
			"this build implements no action called %q. It ships %d action(s) in total.",
			req.Kind, len(e.actions))
		if len(e.actions) == 0 {
			d.Explain = fmt.Sprintf(
				"this build implements no actions at all, so %q cannot run. "+
					"cairn is read-only (CLAUDE.md §2.4); actuation is Phase 4 work that "+
					"deliberately comes after this gate is proven.", req.Kind)
		}
		return d
	}

	// 2. Is the action reversible? A spec that cannot say how it is undone is
	//    not one §6 permits, and that is checked here as well as at
	//    registration — the check that matters is the one on the hot path.
	if spec.Undo == "" {
		d.Gate = GateIrreversible
		d.Explain = fmt.Sprintf("%q does not declare how it is undone; only reversible actions may run", req.Kind)
		return d
	}

	// 3. Does the policy name this cluster? A policy written for one cluster
	//    must not authorize anything on another — the likeliest way a correct
	//    policy file does damage is by being copied.
	if e.policy.Cluster != "" && e.cluster != "" && e.policy.Cluster != e.cluster {
		d.Gate = GateClusterName
		d.Explain = fmt.Sprintf("this policy is written for cluster %q but cairn is running against %q",
			e.policy.Cluster, e.cluster)
		return d
	}

	// 4. Has the operator allowed this kind?
	if !e.policy.Allows(req.Kind) {
		d.Gate = GateNotAllowed
		where := "no policy file was found"
		if e.policy.Path != "" {
			where = e.policy.Path + " does not list it"
		}
		d.Explain = fmt.Sprintf("%q is not permitted: %s. Policy is default-deny.", req.Kind, where)
		return d
	}

	// 5. Is the target in scope? An empty list means none, never everything.
	if !e.policy.InScope(req.Target) {
		d.Gate = GateOutOfScope
		d.Explain = fmt.Sprintf("%s is outside the scope this policy declares; "+
			"an empty nodes/jobs list permits no targets, and fleet-wide scope must be written as \"*\"",
			req.Target)
		return d
	}

	d.Allowed = true
	return d
}

// Execute evaluates a request, records the decision, and performs the action.
//
// The ordering is the contract, and it is the one thing here that cannot be
// retrofitted: **the audit record is written before anything happens, and a
// failure to record is a denial.** An actuation that occurred with no record of
// it is worse than one that never occurred, because the second is recoverable by
// running the tool again and the first is only discoverable by noticing the
// damage.
//
// With the null action set this can never reach the execution step, and a test
// asserts exactly that.
func (e *Engine) Execute(req Request, now time.Time) (Decision, error) {
	d := e.Evaluate(req, now)

	if e.audit == nil {
		// Not an internal error: an engine with no sink is an engine that cannot
		// account for itself, and it must refuse rather than proceed silently.
		d.Allowed = false
		d.Executed = false
		d.Gate = GateNoAudit
		d.Explain = "no audit sink is configured, and cairn does not act without a record"
		return d, errInvalid("no audit sink configured")
	}

	if err := e.audit.Record(d); err != nil {
		denied := d
		denied.Allowed = false
		denied.Executed = false
		denied.Gate = GateNoAudit
		denied.Explain = fmt.Sprintf("the decision could not be recorded (%v), so it was refused", err)
		// Best effort: try to record the refusal itself. If that also fails
		// there is nothing further to do but return the error, which the caller
		// must treat as a denial.
		_ = e.audit.Record(denied)
		return denied, fmt.Errorf("policy: refusing to act without an audit record: %w", err)
	}

	if !d.Allowed {
		return d, nil
	}
	if d.DryRun {
		// Decided, recorded, not executed.
		return d, nil
	}

	// Execution would happen here. There is deliberately nothing to call: this
	// build ships no actions, and an Engine cannot be given any from outside the
	// package. The unreachability is the deliverable.
	//
	// When the three actuations arrive they attach here, behind everything
	// above — never beside it.
	d.Executed = false
	return d, errInvalid("action %q passed every gate but this build implements no executor for it", req.Kind)
}
