// Package redact performs deterministic pseudonymization at the boundary.
//
// CLAUDE.md §10: "Redaction is applied at the boundary, not by the caller. If a
// code path can emit an unredacted hostname, that's a bug, not a configuration
// choice." So this package takes a whole bundle and returns a whole bundle.
// There is no per-field API for a collector to call and forget.
//
// It is built in Phase 1 rather than retrofitted (§6) because retrofitting means
// auditing every code path that already exists, and the audit is the expensive
// part. Building it now means the redaction boundary is the only way out.
package redact

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/touchelos/cairn/schema"
)

// Mode selects what the redactor does.
type Mode string

const (
	// ModeNone passes values through unchanged. Correct for a bundle that never
	// leaves the site. It is still recorded on the bundle header, so a recipient
	// can tell an unredacted bundle from a redacted one rather than guessing.
	ModeNone Mode = "none"
	// ModePseudonymize replaces identifying values with stable pseudonyms.
	ModePseudonymize Mode = "pseudonymize"
)

// kind namespaces the pseudonym space so a hostname and a username that happen
// to share a string do not share a pseudonym.
type kind string

const (
	kindHost    kind = "host"
	kindUser    kind = "user"
	kindAccount kind = "account"
	kindCluster kind = "cluster"
	kindOpaque  kind = "opaque"
)

var prefixes = map[kind]string{
	kindHost:    "node-",
	kindUser:    "user-",
	kindAccount: "acct-",
	kindCluster: "cluster-",
	kindOpaque:  "redacted-",
}

// Redactor maps identifying values to stable pseudonyms.
//
// Pseudonyms are derived by HMAC from a site-held salt, not assigned by counting
// the values in front of us. That matters more than it looks: counting would
// make a host's pseudonym depend on which other hosts happened to appear in the
// same bundle, so two bundles from one site would disagree about who is who —
// and comparing incidents across time is the entire point of SaltID.
type Redactor struct {
	mode Mode
	salt []byte

	// mapping is original -> pseudonym, per kind. Kept so a site can resolve its
	// own bundles. It never leaves the process on the outbound path.
	mapping map[kind]map[string]string
	// taken is pseudonym -> original, per kind, used to detect collisions.
	taken map[kind]map[string]string
}

// New returns a redactor. A nil or empty salt with ModePseudonymize is an error:
// an unsalted pseudonym is a plain hash of the hostname, which anyone holding a
// list of plausible hostnames can reverse in seconds.
func New(mode Mode, salt []byte) (*Redactor, error) {
	switch mode {
	case ModeNone:
	case ModePseudonymize:
		if len(salt) < 16 {
			return nil, fmt.Errorf("redact: pseudonymization needs a salt of at least 16 bytes, got %d", len(salt))
		}
	default:
		return nil, fmt.Errorf("redact: unknown mode %q", mode)
	}
	return &Redactor{
		mode:    mode,
		salt:    append([]byte(nil), salt...),
		mapping: map[kind]map[string]string{},
		taken:   map[kind]map[string]string{},
	}, nil
}

// SaltID is a fingerprint of the salt — never the salt.
//
// Two bundles carrying the same SaltID use the same pseudonyms and can be
// compared. A bundle leaving the site carries the identifier so that correlation
// stays possible while the original names stay unrecoverable.
func (r *Redactor) SaltID() string {
	if r.mode == ModeNone {
		return ""
	}
	sum := sha256.Sum256(append([]byte("cairn-salt-id\x00"), r.salt...))
	return "sha256:" + hex.EncodeToString(sum[:4])
}

// Mode returns the configured mode.
func (r *Redactor) Mode() Mode { return r.mode }

// pseudonym returns the stable pseudonym for a value, allocating on first sight.
func (r *Redactor) pseudonym(k kind, value string) (string, error) {
	if value == "" {
		return "", nil
	}
	if r.mapping[k] == nil {
		r.mapping[k] = map[string]string{}
		r.taken[k] = map[string]string{}
	}
	if p, ok := r.mapping[k][value]; ok {
		return p, nil
	}

	mac := hmac.New(sha256.New, r.salt)
	mac.Write([]byte(string(k)))
	mac.Write([]byte{0})
	mac.Write([]byte(value))
	sum := mac.Sum(nil)

	// Eight decimal digits: short enough to read in a terminal and to say out
	// loud on a call, wide enough that a collision across a large site is rare
	// and — because collisions are rejected below — never silent.
	n := uint64(sum[0])<<24 | uint64(sum[1])<<16 | uint64(sum[2])<<8 | uint64(sum[3])
	p := fmt.Sprintf("%s%08d", prefixes[k], n%100000000)

	if prev, clash := r.taken[k][p]; clash && prev != value {
		// Loud on purpose. Two hosts sharing a pseudonym would silently merge
		// two machines in the join, and the resulting bundle would be wrong in a
		// way no downstream check could detect.
		return "", fmt.Errorf("redact: pseudonym collision: %q and %q both map to %q", prev, value, p)
	}
	r.mapping[k][value] = p
	r.taken[k][p] = value
	return p, nil
}

// MappingEntry is one original-to-pseudonym pair.
type MappingEntry struct {
	Kind      string
	Original  string
	Pseudonym string
}

// Mapping returns every pseudonym assigned, sorted.
//
// This is the site's key to its own bundles: it resolves node-41938274 back to a
// real machine. It must be kept locally and must never be attached to anything
// that leaves — shipping it alongside a redacted bundle un-redacts the bundle.
func (r *Redactor) Mapping() []MappingEntry {
	var out []MappingEntry
	for k, m := range r.mapping {
		for orig, p := range m {
			out = append(out, MappingEntry{Kind: string(k), Original: orig, Pseudonym: p})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		return out[i].Original < out[j].Original
	})
	return out
}

var homePath = regexp.MustCompile(`(/(?:home|users|u)/)([^/\s:,]+)`)

// Bundle redacts a whole bundle.
//
// Two passes, and the order matters. The first learns every identifier from the
// structured fields, where the redactor knows for certain what a value is. The
// second substitutes those learned identifiers inside free-form values such as a
// cgroup path or a mount point.
//
// Doing it in one pass would miss the common case: a hostname that appears only
// inside a path, on an event processed before the event that named that host in
// its Node field.
func (r *Redactor) Bundle(b schema.Bundle) (schema.Bundle, error) {
	if r.mode == ModeNone {
		b.Redaction = schema.Redaction{Mode: string(ModeNone)}
		return b, nil
	}

	// Pass 1: learn identifiers from structured fields.
	if _, err := r.pseudonym(kindCluster, string(b.Cluster)); err != nil {
		return schema.Bundle{}, err
	}
	for _, e := range b.Events {
		if e.Node != "" {
			if _, err := r.pseudonym(kindHost, string(e.Node)); err != nil {
				return schema.Bundle{}, err
			}
		}
		for key, v := range e.Detail.Attrs {
			switch key {
			case "user":
				if _, err := r.pseudonym(kindUser, v); err != nil {
					return schema.Bundle{}, err
				}
			case "account":
				if _, err := r.pseudonym(kindAccount, v); err != nil {
					return schema.Bundle{}, err
				}
			}
		}
	}
	for _, c := range b.Clocks {
		if c.Node != "" {
			if _, err := r.pseudonym(kindHost, string(c.Node)); err != nil {
				return schema.Bundle{}, err
			}
		}
	}

	// Pass 2: rewrite.
	out := b
	out.Cluster = schema.ClusterName(r.mapping[kindCluster][string(b.Cluster)])

	out.Clocks = make([]schema.ClockOffset, len(b.Clocks))
	for i, c := range b.Clocks {
		c.Node = schema.Hostname(r.mapping[kindHost][string(c.Node)])
		out.Clocks[i] = c
	}

	out.Events = make([]schema.Event, len(b.Events))
	for i, e := range b.Events {
		red, err := r.event(e)
		if err != nil {
			return schema.Bundle{}, err
		}
		out.Events[i] = red
	}

	out.Redaction = schema.Redaction{Mode: string(ModePseudonymize), SaltID: r.SaltID()}
	return out, nil
}

func (r *Redactor) event(e schema.Event) (schema.Event, error) {
	e.Cluster = schema.ClusterName(r.mapping[kindCluster][string(e.Cluster)])
	if e.Node != "" {
		e.Node = schema.Hostname(r.mapping[kindHost][string(e.Node)])
	}
	if len(e.Detail.Attrs) == 0 {
		return e, nil
	}

	attrs := make(map[string]string, len(e.Detail.Attrs))
	for k, v := range e.Detail.Attrs {
		if !schema.AttrIsPII(e.Class, k) {
			attrs[k] = v
			continue
		}
		switch k {
		case "user":
			attrs[k] = r.mapping[kindUser][v]
		case "account":
			attrs[k] = r.mapping[kindAccount][v]
		default:
			red, err := r.text(v)
			if err != nil {
				return schema.Event{}, err
			}
			attrs[k] = red
		}
	}
	e.Detail.Attrs = attrs
	return e, nil
}

// text substitutes learned identifiers inside a free-form value.
//
// This is best-effort, and deliberately so. It preserves structure — a cgroup
// path stays a recognizable cgroup path, which is most of its diagnostic value —
// at the cost of not being able to guarantee that an identifier nobody named
// elsewhere is caught.
//
// That residual risk is exactly why schema/DESIGN.md §2 keeps free-form values
// rare and bounded, and why the scanner in redact/scan exists as a backstop. If
// this function is ever doing heavy lifting, the schema has grown a field it
// should not have.
func (r *Redactor) text(v string) (string, error) {
	// A username in a home path may never appear in a `user` attr on any event,
	// so catch it structurally before falling back to substitution.
	var perr error
	v = homePath.ReplaceAllStringFunc(v, func(m string) string {
		parts := homePath.FindStringSubmatch(m)
		p, err := r.pseudonym(kindUser, parts[2])
		if err != nil {
			perr = err
			return m
		}
		return parts[1] + p
	})
	if perr != nil {
		return "", perr
	}

	// Longest original first, so that a host named node-004 cannot partially
	// rewrite an occurrence of node-0042.
	type sub struct{ from, to string }
	var subs []sub
	for _, k := range []kind{kindHost, kindUser, kindAccount, kindCluster} {
		for orig, p := range r.mapping[k] {
			subs = append(subs, sub{orig, p})
		}
	}
	sort.Slice(subs, func(i, j int) bool {
		if len(subs[i].from) != len(subs[j].from) {
			return len(subs[i].from) > len(subs[j].from)
		}
		return subs[i].from < subs[j].from
	})
	for _, s := range subs {
		v = strings.ReplaceAll(v, s.from, s.to)
	}
	return v, nil
}
