package schema

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"time"
)

// tsLayout is the only timestamp format cairn emits.
//
// It is written out in full rather than using time.RFC3339Nano, which trims
// trailing zeros and so produces variable-width output: the same instant would
// serialize as "...:05.5Z" or "...:05.123456789Z" depending on its value. Fixed
// width keeps output byte-identical and column-aligned for diffing (§2.7).
const tsLayout = "2006-01-02T15:04:05.000000000Z07:00"

// FormatTime renders an instant in cairn's canonical form. It converts to UTC
// rather than trusting the caller, because a single event carrying a local
// offset would desynchronize an entire bundle.
func FormatTime(t time.Time) string {
	return t.UTC().Format(tsLayout)
}

// ParseTime reads a canonical timestamp back.
func ParseTime(s string) (time.Time, error) {
	t, err := time.Parse(tsLayout, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("schema: bad timestamp %q: %w", s, err)
	}
	return t.UTC(), nil
}

// Canonical JSON.
//
// These helpers assemble objects field by field in a fixed declared order rather
// than delegating to encoding/json's struct marshaling. The reason is that
// invariant §2.7 — byte-identical output across runs — is the one property most
// easily lost by accident: a reordered struct field, a map iterated directly, a
// float, an HTML-escaped ampersand. Making the byte layout explicit here means
// determinism is enforced in one file rather than hoped for in twenty.

// jstr encodes a string with HTML escaping disabled. encoding/json escapes <, >,
// and & by default, which is meaningless outside a browser and would make output
// depend on incidental content.
func jstr(s string) []byte {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(s); err != nil {
		// Encoding a Go string cannot fail; invalid UTF-8 is replaced, not rejected.
		panic("schema: string encode failed: " + err.Error())
	}
	return bytes.TrimRight(b.Bytes(), "\n")
}

func juint(n uint64) []byte { return []byte(strconv.FormatUint(n, 10)) }
func jint(n int64) []byte   { return []byte(strconv.FormatInt(n, 10)) }

// jfield is one key/value pair of a canonical object.
type jfield struct {
	key string
	raw []byte // nil means "omit this field entirely"
}

// jobj assembles an object, preserving the given order and dropping omitted
// fields. Order is the declaration order at the call site, never map order.
func jobj(fields ...jfield) []byte {
	var b bytes.Buffer
	b.WriteByte('{')
	first := true
	for _, f := range fields {
		if f.raw == nil {
			continue
		}
		if !first {
			b.WriteByte(',')
		}
		first = false
		b.Write(jstr(f.key))
		b.WriteByte(':')
		b.Write(f.raw)
	}
	b.WriteByte('}')
	return b.Bytes()
}

// encodeAttrs renders the attrs map with keys in sorted order.
//
// encoding/json documents that it sorts map keys, but relying on that would make
// a load-bearing invariant depend on a library implementation detail. Sorting
// here states the requirement where it is enforced.
func encodeAttrs(attrs map[string]string) []byte {
	if len(attrs) == 0 {
		return nil
	}
	keys := make([]string, 0, len(attrs))
	for k := range attrs {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var b bytes.Buffer
	b.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(',')
		}
		b.Write(jstr(k))
		b.WriteByte(':')
		b.Write(jstr(attrs[k]))
	}
	b.WriteByte('}')
	return b.Bytes()
}

func encodeJobID(j *JobID) []byte {
	if j == nil {
		return []byte("null")
	}
	var arrayTask, hetOffset, arrayRange, step []byte
	if j.ArrayTask != nil {
		arrayTask = juint(uint64(*j.ArrayTask))
	}
	if j.HetOffset != nil {
		hetOffset = juint(uint64(*j.HetOffset))
	}
	if j.ArrayRange != "" {
		arrayRange = jstr(j.ArrayRange)
	}
	if j.Step != "" {
		step = jstr(j.Step)
	}
	return jobj(
		jfield{"raw", jstr(j.Raw)},
		jfield{"base", juint(j.Base)},
		jfield{"array_task", arrayTask},
		jfield{"array_range", arrayRange},
		jfield{"het_offset", hetOffset},
		jfield{"step", step},
	)
}

func encodeDetail(d Detail) []byte {
	return jobj(
		jfield{"signature", jstr(d.Signature)},
		jfield{"attrs", encodeAttrs(d.Attrs)},
	)
}

// EncodeEvent renders one event as a single-line canonical JSON object.
//
// Field order matches the schema as declared in CLAUDE.md §4:
// ts, cluster, node, jobid, source, class, detail.
//
// The event is validated first. Bounds that are only checked in tests are not
// bounds, so an invalid event is not serializable at all.
func EncodeEvent(e Event) ([]byte, error) {
	if err := e.Validate(); err != nil {
		return nil, err
	}
	var node []byte
	if e.Node != "" {
		node = jstr(string(e.Node))
	}
	return jobj(
		jfield{"ts", jstr(FormatTime(e.TS))},
		jfield{"cluster", jstr(string(e.Cluster))},
		jfield{"node", node},
		jfield{"jobid", encodeJobID(e.JobID)},
		jfield{"source", jstr(string(e.Source))},
		jfield{"class", jstr(string(e.Class))},
		jfield{"detail", encodeDetail(e.Detail)},
	), nil
}

// EncodeEvents renders a canonical event stream: a JSON array with one event per
// line, terminated by a newline.
//
// One event per line is deliberate. These files are hand-reviewed during
// redaction and diffed when a collector changes; a single-line array is
// unreadable and a fully-indented one buries a one-field change in noise.
//
// The input is sorted into canonical order first, on a copy. Canonical order is
// part of canonical form, so requiring callers to sort beforehand would make
// every caller a place the invariant can be broken.
func EncodeEvents(evs []Event) ([]byte, error) {
	sorted := make([]Event, len(evs))
	copy(sorted, evs)
	SortEvents(sorted)

	var b bytes.Buffer
	b.WriteString("[\n")
	for i, e := range sorted {
		raw, err := EncodeEvent(e)
		if err != nil {
			return nil, fmt.Errorf("event %d: %w", i, err)
		}
		b.WriteString("  ")
		b.Write(raw)
		if i < len(sorted)-1 {
			b.WriteByte(',')
		}
		b.WriteByte('\n')
	}
	b.WriteString("]\n")
	return b.Bytes(), nil
}
