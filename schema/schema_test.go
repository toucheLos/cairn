package schema

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
	"time"
)

func mustTime(t *testing.T, s string) time.Time {
	t.Helper()
	ts, err := ParseTime(s)
	if err != nil {
		t.Fatalf("ParseTime(%q): %v", s, err)
	}
	return ts
}

func mustJobID(t *testing.T, s string) *JobID {
	t.Helper()
	j, err := ParseJobID(s)
	if err != nil {
		t.Fatalf("ParseJobID(%q): %v", s, err)
	}
	return j
}

// sampleEvents is the fixed corpus the golden encoding tests use. It is chosen
// to exercise every optional field and every awkward value: a nil job id, a
// missing node, an array task, an array mask, a heterogeneous component, a step
// suffix, an empty attrs map, non-ASCII text, and characters encoding/json would
// escape if HTML escaping were left on.
func sampleEvents(t *testing.T) []Event {
	t.Helper()
	return []Event{
		{
			TS:      mustTime(t, "2026-03-04T09:14:02.000000000Z"),
			Cluster: "cluster-a",
			Node:    "node-0042",
			JobID:   mustJobID(t, "918273.batch"),
			Source:  SourceSlurm,
			Class:   ClassResourceOOM,
			Detail: Detail{
				Signature: "slurm.sacct.state.OUT_OF_MEMORY",
				Attrs: map[string]string{
					"state":       "OUT_OF_MEMORY",
					"exit_code":   "0:125",
					"limit_bytes": "8589934592",
					"usage_bytes": "8590131200",
				},
			},
		},
		{
			// Same instant as the event above: ties are normal, and the total
			// order must resolve them the same way every run.
			TS:      mustTime(t, "2026-03-04T09:14:02.000000000Z"),
			Cluster: "cluster-a",
			Node:    "node-0042",
			JobID:   nil, // node-without-jobid
			Source:  SourceJournal,
			Class:   ClassGPUXid,
			Detail: Detail{
				Signature: "nvidia.xid.79",
				Attrs: map[string]string{
					"xid":       "79",
					"gpu_index": "3",
					"pci_addr":  "0000:c1:00.0",
				},
			},
		},
		{
			TS:      mustTime(t, "2026-03-04T09:15:30.500000000Z"),
			Cluster: "cluster-a",
			Node:    "", // jobid-without-node
			JobID:   mustJobID(t, "918274_[8-20%4]"),
			Source:  SourceSlurm,
			Class:   ClassSchedRequeued,
			Detail: Detail{
				Signature: "slurm.sacct.state.REQUEUED",
				Attrs:     map[string]string{"reason": "JobHeldByAdmin & requeued"},
			},
		},
		{
			TS:      mustTime(t, "2026-03-04T09:16:00.000000000Z"),
			Cluster: "cluster-a",
			Node:    "node-0043",
			JobID:   mustJobID(t, "918275_7"),
			Source:  SourceFabric,
			Class:   ClassFabricLinkFlap,
			Detail: Detail{
				Signature: "ib.link.state_change",
				Attrs: map[string]string{
					"device":     "mlx5_0",
					"port":       "1",
					"link_state": "Down",
					"phys_state": "Polling",
					"flap_count": "3",
				},
			},
		},
		{
			TS:      mustTime(t, "2026-03-04T09:17:00.000000000Z"),
			Cluster: "cluster-a",
			Node:    "node-0044",
			JobID:   mustJobID(t, "918276+1"),
			Source:  SourceGPU,
			Class:   ClassGPUDriverMismatch,
			Detail: Detail{
				Signature: "nvidia.driver.generation_mismatch",
				Attrs: map[string]string{
					"driver_version":          "535.104.05",
					"expected_driver_version": "550.54.15",
					"cuda_version":            "12.4",
				},
			},
		},
		{
			TS:      mustTime(t, "2026-03-04T09:18:00.000000000Z"),
			Cluster: "cluster-a",
			Node:    "node-0045",
			JobID:   nil,
			Source:  SourceBMC,
			Class:   ClassUnknown,
			Detail: Detail{
				// Non-ASCII and characters that encoding/json escapes by default.
				Signature: "bmc.sel.unrecognized",
				Attrs:     map[string]string{"severity": "warn <&> µs 日本語"},
			},
		},
		{
			TS:      mustTime(t, "2026-03-04T09:19:00.000000000Z"),
			Cluster: "cluster-a",
			Node:    "node-0046",
			JobID:   mustJobID(t, "918277"),
			Source:  SourceStorage,
			Class:   ClassStorageStaleHandle,
			Detail:  Detail{Signature: "lustre.estale"}, // no attrs at all
		},
	}
}

func TestClassValidity(t *testing.T) {
	for _, c := range AllClasses() {
		if !c.Valid() {
			t.Errorf("AllClasses returned %q but Valid() is false", c)
		}
		got, err := ParseClass(string(c))
		if err != nil {
			t.Errorf("ParseClass(%q): %v", c, err)
			continue
		}
		if got != c {
			t.Errorf("ParseClass(%q) = %q", c, got)
		}
		if fam := c.Family(); fam == "" {
			t.Errorf("class %q has an empty family", c)
		}
	}
	if _, err := ParseClass("gpu.definitely_not_a_class"); err == nil {
		t.Error("ParseClass accepted a non-member")
	}
	// ParseClass must not quietly downgrade an unknown string to ClassUnknown:
	// that would turn a producer/consumer version mismatch into silent data loss.
	if c, err := ParseClass("nonsense"); err == nil || c == ClassUnknown {
		t.Errorf("ParseClass fell back to unknown instead of erroring: %q, %v", c, err)
	}
}

func TestClassFamily(t *testing.T) {
	for _, tc := range []struct {
		in   Class
		want string
	}{
		{ClassGPUXid, "gpu"},
		{ClassUnknown, "unknown"},
		{ClassResourceWalltimeExceeded, "resource"},
	} {
		if got := tc.in.Family(); got != tc.want {
			t.Errorf("Class(%q).Family() = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestParseJobID(t *testing.T) {
	u32 := func(n uint32) *uint32 { return &n }

	for _, tc := range []struct {
		in   string
		want JobID
	}{
		{"12345", JobID{Raw: "12345", Base: 12345}},
		{"12345.batch", JobID{Raw: "12345.batch", Base: 12345, Step: "batch"}},
		{"12345.extern", JobID{Raw: "12345.extern", Base: 12345, Step: "extern"}},
		{"12345.0", JobID{Raw: "12345.0", Base: 12345, Step: "0"}},
		{"12345_7", JobID{Raw: "12345_7", Base: 12345, ArrayTask: u32(7)}},
		{"12345_0", JobID{Raw: "12345_0", Base: 12345, ArrayTask: u32(0)}},
		{"12345_[8-20]", JobID{Raw: "12345_[8-20]", Base: 12345, ArrayRange: "[8-20]"}},
		{"12345_[8-20%4]", JobID{Raw: "12345_[8-20%4]", Base: 12345, ArrayRange: "[8-20%4]"}},
		{"12345+0", JobID{Raw: "12345+0", Base: 12345, HetOffset: u32(0)}},
		{"12345_7.batch", JobID{Raw: "12345_7.batch", Base: 12345, ArrayTask: u32(7), Step: "batch"}},
		{"12345+1.extern", JobID{Raw: "12345+1.extern", Base: 12345, HetOffset: u32(1), Step: "extern"}},
		{"  12345  ", JobID{Raw: "12345", Base: 12345}},
	} {
		got, err := ParseJobID(tc.in)
		if err != nil {
			t.Errorf("ParseJobID(%q): unexpected error %v", tc.in, err)
			continue
		}
		if got.Raw != tc.want.Raw || got.Base != tc.want.Base ||
			got.ArrayRange != tc.want.ArrayRange || got.Step != tc.want.Step ||
			!eqU32(got.ArrayTask, tc.want.ArrayTask) || !eqU32(got.HetOffset, tc.want.HetOffset) {
			t.Errorf("ParseJobID(%q) = %+v, want %+v", tc.in, *got, tc.want)
		}
		if err := got.Validate(); err != nil {
			t.Errorf("ParseJobID(%q) produced an invalid JobID: %v", tc.in, err)
		}
	}

	for _, bad := range []string{
		"", "   ", "abc", "12345_", "12345_x", "12345+x",
		"12345_[8-20", "12345.", "12345_7+1", "-1",
	} {
		if got, err := ParseJobID(bad); err == nil {
			t.Errorf("ParseJobID(%q) succeeded with %+v, want an error", bad, *got)
		}
	}
}

func eqU32(a, b *uint32) bool {
	switch {
	case a == nil && b == nil:
		return true
	case a == nil || b == nil:
		return false
	default:
		return *a == *b
	}
}

// TestJobIDSameJob covers the predicate the join depends on: every step and
// array task of a job must resolve to the same base job.
func TestJobIDSameJob(t *testing.T) {
	base := mustJobID(t, "12345")
	for _, s := range []string{"12345", "12345.batch", "12345.extern", "12345.0", "12345_7", "12345+1"} {
		if !base.SameJob(mustJobID(t, s)) {
			t.Errorf("%q should belong to the same job as 12345", s)
		}
	}
	if base.SameJob(mustJobID(t, "12346")) {
		t.Error("12346 should not belong to the same job as 12345")
	}
	// A nil job id belongs to no job. It must not compare equal to another nil,
	// or every node-scoped event would join to every other node-scoped event.
	var nilA, nilB *JobID
	if nilA.SameJob(nilB) || base.SameJob(nil) || nilA.SameJob(base) {
		t.Error("a nil JobID must not compare as the same job")
	}
}

// TestEncodeDeterministicInProcess encodes the same logical events many times,
// rebuilding the attrs maps each round.
//
// Go randomizes map iteration order per map, so rebuilding the maps is what
// actually exercises the risk: encoding one map repeatedly can pass by luck.
func TestEncodeDeterministicInProcess(t *testing.T) {
	first, err := EncodeEvents(sampleEvents(t))
	if err != nil {
		t.Fatalf("EncodeEvents: %v", err)
	}
	for i := 0; i < 200; i++ {
		got, err := EncodeEvents(sampleEvents(t))
		if err != nil {
			t.Fatalf("EncodeEvents (round %d): %v", i, err)
		}
		if !bytes.Equal(first, got) {
			t.Fatalf("encoding is not deterministic; round %d differs:\n%s\nvs\n%s", i, first, got)
		}
	}
}

// TestEncodeIndependentOfInputOrder is the other half of §2.7: two collectors
// that discover the same events in different orders must produce byte-identical
// bundles, or nothing downstream can be diffed.
func TestEncodeIndependentOfInputOrder(t *testing.T) {
	evs := sampleEvents(t)
	want, err := EncodeEvents(evs)
	if err != nil {
		t.Fatalf("EncodeEvents: %v", err)
	}
	// Reverse, then rotate, rather than shuffling: a fixed permutation keeps the
	// test itself deterministic.
	rev := make([]Event, len(evs))
	for i := range evs {
		rev[i] = evs[len(evs)-1-i]
	}
	got, err := EncodeEvents(rev)
	if err != nil {
		t.Fatalf("EncodeEvents(reversed): %v", err)
	}
	if !bytes.Equal(want, got) {
		t.Errorf("encoding depends on input order:\n%s\nvs\n%s", want, got)
	}

	rot := append(append([]Event{}, evs[3:]...), evs[:3]...)
	got, err = EncodeEvents(rot)
	if err != nil {
		t.Fatalf("EncodeEvents(rotated): %v", err)
	}
	if !bytes.Equal(want, got) {
		t.Errorf("encoding depends on input order after rotation")
	}
}

// TestEncodeDoesNotMutateInput guards a subtle bug: sorting the caller's slice
// in place would make output depend on how many times the caller had already
// encoded it.
func TestEncodeDoesNotMutateInput(t *testing.T) {
	evs := sampleEvents(t)
	before := make([]string, len(evs))
	for i, e := range evs {
		before[i] = e.Detail.Signature
	}
	if _, err := EncodeEvents(evs); err != nil {
		t.Fatalf("EncodeEvents: %v", err)
	}
	for i, e := range evs {
		if e.Detail.Signature != before[i] {
			t.Fatalf("EncodeEvents reordered the caller's slice at index %d", i)
		}
	}
}

func TestRoundTrip(t *testing.T) {
	want := sampleEvents(t)
	encoded, err := EncodeEvents(want)
	if err != nil {
		t.Fatalf("EncodeEvents: %v", err)
	}
	got, err := DecodeEvents(encoded)
	if err != nil {
		t.Fatalf("DecodeEvents: %v", err)
	}
	reencoded, err := EncodeEvents(got)
	if err != nil {
		t.Fatalf("re-EncodeEvents: %v", err)
	}
	if !bytes.Equal(encoded, reencoded) {
		t.Errorf("round trip is not stable:\n%s\nvs\n%s", encoded, reencoded)
	}
	ok, _, err := IsCanonical(encoded)
	if err != nil {
		t.Fatalf("IsCanonical: %v", err)
	}
	if !ok {
		t.Error("EncodeEvents output is not reported as canonical")
	}
}

// TestIsCanonicalRejectsNoncanonical confirms the check has teeth: a file that
// decodes correctly but is formatted differently must be rejected, because a
// non-canonical golden file will start failing spuriously the first time a
// collector reorders its output.
func TestIsCanonicalRejectsNoncanonical(t *testing.T) {
	canonical, err := EncodeEvents(sampleEvents(t))
	if err != nil {
		t.Fatalf("EncodeEvents: %v", err)
	}
	// Same events, valid JSON, different bytes: indentation removed.
	compact := bytes.ReplaceAll(canonical, []byte("\n  "), []byte(""))
	compact = bytes.ReplaceAll(compact, []byte("\n"), []byte(""))
	ok, _, err := IsCanonical(compact)
	if err != nil {
		t.Fatalf("IsCanonical on compact form: %v", err)
	}
	if ok {
		t.Error("compact form was accepted as canonical")
	}
}

func TestDecodeRejectsUnknownField(t *testing.T) {
	data := []byte(`[{"ts":"2026-03-04T09:14:02.000000000Z","cluster":"c","jobid":null,` +
		`"source":"slurm","class":"unknown","detail":{"signature":"s"},"future_field":1}]`)
	if _, err := DecodeEvents(data); err == nil {
		t.Error("decode accepted an unknown field; a newer schema version must fail loudly")
	}
}

func TestDecodeRejectsUnknownClass(t *testing.T) {
	data := []byte(`[{"ts":"2026-03-04T09:14:02.000000000Z","cluster":"c","jobid":null,` +
		`"source":"slurm","class":"gpu.not_a_class","detail":{"signature":"s"}}]`)
	if _, err := DecodeEvents(data); err == nil {
		t.Error("decode accepted an unknown class")
	}
}

// TestValidateRejects covers every bound the schema claims to enforce. These are
// the checks that keep detail from becoming a log buffer (CLAUDE.md §4), so each
// one is exercised rather than assumed.
func TestValidateRejects(t *testing.T) {
	valid := func() Event {
		return Event{
			TS:      mustTime(t, "2026-03-04T09:14:02.000000000Z"),
			Cluster: "cluster-a",
			Node:    "node-0042",
			Source:  SourceSlurm,
			Class:   ClassResourceOOM,
			Detail:  Detail{Signature: "slurm.sacct.state.OUT_OF_MEMORY"},
		}
	}
	if err := valid().Validate(); err != nil {
		t.Fatalf("the baseline event should be valid: %v", err)
	}

	tooManyAttrs := map[string]string{}
	for i := 0; i < MaxAttrs+1; i++ {
		tooManyAttrs[fmt.Sprintf("k%02d", i)] = "v"
	}

	for name, mutate := range map[string]func(*Event){
		"zero ts":         func(e *Event) { e.TS = time.Time{} },
		"non-UTC ts":      func(e *Event) { e.TS = e.TS.In(time.FixedZone("CEST", 7200)) },
		"empty cluster":   func(e *Event) { e.Cluster = "" },
		"unknown source":  func(e *Event) { e.Source = Source("prometheus") },
		"unknown class":   func(e *Event) { e.Class = Class("gpu.melted") },
		"empty signature": func(e *Event) { e.Detail.Signature = "" },
		"oversized signature": func(e *Event) {
			e.Detail.Signature = strings.Repeat("s", MaxSignatureLen+1)
		},
		"unregistered attr": func(e *Event) {
			e.Detail.Attrs = map[string]string{"raw_log_line": "kernel: blah"}
		},
		"attr valid for another class": func(e *Event) {
			// "xid" is registered for gpu.xid, not for resource.oom. The
			// allowlist is per-class, not global.
			e.Detail.Attrs = map[string]string{"xid": "79"}
		},
		"oversized attr value": func(e *Event) {
			e.Detail.Attrs = map[string]string{"cgroup": strings.Repeat("x", MaxAttrValueLen+1)}
		},
		"too many attrs":   func(e *Event) { e.Detail.Attrs = tooManyAttrs },
		"invalid job id":   func(e *Event) { e.JobID = &JobID{Raw: ""} },
		"contradictory id": func(e *Event) { e.JobID = &JobID{Raw: "1_2", Base: 1, ArrayTask: new(uint32), ArrayRange: "[1-2]"} },
	} {
		e := valid()
		mutate(&e)
		if err := e.Validate(); err == nil {
			t.Errorf("Validate accepted an event with %s", name)
		}
		if _, err := EncodeEvent(e); err == nil {
			t.Errorf("EncodeEvent serialized an event with %s; invalid events must not be encodable", name)
		}
	}
}

// TestValidateErrorsAreStable: an event with several unregistered keys must
// report the same key every time, or CI failures cannot be diffed.
func TestValidateErrorsAreStable(t *testing.T) {
	build := func() Event {
		return Event{
			TS:      mustTime(t, "2026-03-04T09:14:02.000000000Z"),
			Cluster: "cluster-a",
			Source:  SourceSlurm,
			Class:   ClassResourceOOM,
			Detail: Detail{
				Signature: "s",
				Attrs:     map[string]string{"zzz": "1", "aaa": "2", "mmm": "3"},
			},
		}
	}
	first := build().Validate()
	if first == nil {
		t.Fatal("expected a validation error")
	}
	for i := 0; i < 100; i++ {
		if got := build().Validate(); got.Error() != first.Error() {
			t.Fatalf("validation error text is not stable:\n%v\nvs\n%v", first, got)
		}
	}
}

func TestUniversalAttrsAcceptedEverywhere(t *testing.T) {
	for _, c := range AllClasses() {
		for _, k := range []string{"exit_code", "signal", "state", "severity", "user", "account", "partition"} {
			if !AttrAllowed(c, k) {
				t.Errorf("universal attr %q rejected for class %q", k, c)
			}
		}
	}
}

// TestUnregisteredAttrsAreTreatedAsPII: the safe default for data nobody has
// assessed is to assume it identifies someone.
func TestUnregisteredAttrsAreTreatedAsPII(t *testing.T) {
	if !AttrIsPII(ClassResourceOOM, "some_key_nobody_registered") {
		t.Error("an unregistered attr must be treated as PII")
	}
	if AttrIsPII(ClassGPUXid, "xid") {
		t.Error("xid is a device error number and should not be marked PII")
	}
	if !AttrIsPII(ClassResourceOOM, "cgroup") {
		t.Error("cgroup paths contain user names and must be marked PII")
	}
}

func TestTimeFormatIsFixedWidth(t *testing.T) {
	// RFC3339Nano trims trailing zeros, which would make output width depend on
	// the value. Every timestamp cairn emits must be the same length.
	want := len("2026-03-04T09:14:02.000000000Z")
	for _, s := range []string{
		"2026-03-04T09:14:02.000000000Z",
		"2026-03-04T09:14:02.500000000Z",
		"2026-03-04T09:14:02.123456789Z",
	} {
		ts := mustTime(t, s)
		if got := FormatTime(ts); len(got) != want || got != s {
			t.Errorf("FormatTime(%q) = %q (len %d), want %q (len %d)", s, got, len(got), s, want)
		}
	}
	// A non-UTC instant must be normalized, not emitted with an offset.
	local := time.Date(2026, 3, 4, 11, 14, 2, 0, time.FixedZone("CEST", 7200))
	if got := FormatTime(local); got != "2026-03-04T09:14:02.000000000Z" {
		t.Errorf("FormatTime did not normalize to UTC: %q", got)
	}
}

func TestBundleRejectsBadInput(t *testing.T) {
	base := func() Bundle {
		return Bundle{
			Cluster:   "cluster-a",
			Window:    Window{Start: mustTime(t, "2026-03-04T09:00:00.000000000Z"), End: mustTime(t, "2026-03-04T10:00:00.000000000Z")},
			Redaction: Redaction{Mode: "none"},
		}
	}
	for name, mutate := range map[string]func(*Bundle){
		"empty cluster":       func(b *Bundle) { b.Cluster = "" },
		"inverted window":     func(b *Bundle) { b.Window.Start, b.Window.End = b.Window.End, b.Window.Start },
		"unknown redact mode": func(b *Bundle) { b.Redaction.Mode = "maybe" },
		"empty redact mode":   func(b *Bundle) { b.Redaction.Mode = "" },
	} {
		b := base()
		mutate(&b)
		if _, err := b.Encode(); err == nil {
			t.Errorf("Bundle.Encode accepted %s", name)
		}
	}
}

// TestBundleHasNoWallClockStamp: a generation timestamp would make two bundles
// covering the same window differ, defeating invariant §2.7 and the eval harness
// that depends on it.
func TestBundleHasNoWallClockStamp(t *testing.T) {
	b := Bundle{
		Cluster:   "cluster-a",
		Window:    Window{Start: mustTime(t, "2026-03-04T09:00:00.000000000Z"), End: mustTime(t, "2026-03-04T10:00:00.000000000Z")},
		Redaction: Redaction{Mode: "none"},
		Events:    sampleEvents(t),
	}
	first, err := b.Encode()
	if err != nil {
		t.Fatalf("Bundle.Encode: %v", err)
	}
	second, err := b.Encode()
	if err != nil {
		t.Fatalf("Bundle.Encode: %v", err)
	}
	if !bytes.Equal(first, second) {
		t.Fatal("two encodings of the same bundle differ")
	}
	for _, forbidden := range []string{"generated_at", "collected_at", "created"} {
		if bytes.Contains(first, []byte(forbidden)) {
			t.Errorf("bundle contains a wall-clock field %q; see Bundle.Encode", forbidden)
		}
	}
}

func TestNoHTMLEscaping(t *testing.T) {
	e := Event{
		TS:      mustTime(t, "2026-03-04T09:14:02.000000000Z"),
		Cluster: "cluster-a",
		Source:  SourceSlurm,
		Class:   ClassSchedCancelled,
		Detail:  Detail{Signature: "s", Attrs: map[string]string{"reason": "a<b&c>d"}},
	}
	got, err := EncodeEvent(e)
	if err != nil {
		t.Fatalf("EncodeEvent: %v", err)
	}
	if !bytes.Contains(got, []byte(`"a<b&c>d"`)) {
		t.Errorf("value was HTML-escaped: %s", got)
	}
}

func TestSortEventsIsTotal(t *testing.T) {
	evs := sampleEvents(t)
	a := make([]Event, len(evs))
	copy(a, evs)
	SortEvents(a)

	b := make([]Event, len(evs))
	for i := range evs {
		b[i] = evs[len(evs)-1-i]
	}
	SortEvents(b)

	for i := range a {
		if a[i].Detail.Signature != b[i].Detail.Signature || !a[i].TS.Equal(b[i].TS) {
			t.Fatalf("sort is not total: index %d differs (%q vs %q)",
				i, a[i].Detail.Signature, b[i].Detail.Signature)
		}
	}
}
