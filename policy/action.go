package policy

import "sort"

// Kind names something cairn could be asked to do to a cluster.
//
// **There are no members.** That is the null action set, and it is the whole
// point of this phase: the gate is built and proven while the blast radius is
// provably zero, because every mistake in an authorization path is discovered
// the first time it wrongly permits something — and by then it has permitted it.
//
// CLAUDE.md §6 names the three actuations that will eventually exist, and they
// are named here as comments rather than constants so that adding one is a
// deliberate act rather than uncommenting a line:
//
//   - drain node
//   - requeue job
//   - rerun health check
//
// All three are reversible. Config edits are not on the list and never will be
// (§6: "No config edits, ever").
type Kind string

// shippedActions is what this build can perform: nothing.
//
// A slice rather than a map so the emptiness is visible at a glance, and a
// package-level value rather than a registry with an Add function — there is
// deliberately no way for any code path, test or otherwise, to append to it.
// Tests construct an Engine with their own action set instead.
var shippedActions = []ActionSpec{}

// ShippedActions returns the actions this build can perform.
//
// scripts/verify-guards.sh asserts the result is empty. That guard is what makes
// "null action set" a fact about the binary rather than a claim in a comment,
// and adding an actuation must fail it until someone deliberately changes it.
func ShippedActions() []ActionSpec {
	out := make([]ActionSpec, len(shippedActions))
	copy(out, shippedActions)
	sort.Slice(out, func(i, j int) bool { return out[i].Kind < out[j].Kind })
	return out
}

// ActionSpec describes an action this build knows how to perform.
type ActionSpec struct {
	Kind Kind

	// Undo says how the action is reversed, in an operator's words.
	//
	// Required. §6 permits exactly three actuations and all three are
	// reversible, so an action that cannot say how it is undone is not one cairn
	// may perform. Making it a field rather than a convention means
	// irreversibility has nowhere to hide: an empty Undo is a validation error,
	// not an omission somebody might not notice in review.
	Undo string

	// Privileged records whether the action needs more than an ordinary user.
	// Recorded because invariant §2.2 is that cairn runs unprivileged, so an
	// action requiring root is a deployment decision, not a detail.
	Privileged bool
}

// Valid reports whether a spec is usable.
func (a ActionSpec) Valid() error {
	if a.Kind == "" {
		return errInvalid("action has no kind")
	}
	if a.Undo == "" {
		return errInvalid("action %q does not say how it is undone; "+
			"§6 permits only reversible actuations", string(a.Kind))
	}
	return nil
}

// Target is what an action would act on.
//
// Deliberately narrow. A target that could name a config file or an arbitrary
// path would make "no config edits, ever" a matter of discipline rather than of
// what is representable.
type Target struct {
	Node string
	Job  string
}

// Empty reports whether the target names nothing.
func (t Target) Empty() bool { return t.Node == "" && t.Job == "" }

// String renders a target for an audit record and for operator output.
func (t Target) String() string {
	switch {
	case t.Node != "" && t.Job != "":
		return "node=" + t.Node + " job=" + t.Job
	case t.Node != "":
		return "node=" + t.Node
	case t.Job != "":
		return "job=" + t.Job
	}
	return "(none)"
}

// Request is a proposed action.
type Request struct {
	Kind   Kind
	Target Target
	// Reason is why the caller wants this. Recorded in the audit log: a record
	// of what happened without why it was thought a good idea is half a record.
	Reason string
}
