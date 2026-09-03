package yamlsub

import (
	"strings"
	"testing"
)

// This package was extracted from site/ so that policy.yaml could share it, and
// that raised its stakes: it now parses the file that gates actuation. The site
// tests exercise it indirectly through a Profile; these exercise the subset
// itself, so a change here fails next to the code that caused it.

// TestRejectsUnsupportedConstructs is the property that makes a hand-edited file
// safe: anything this subset does not model is refused, never guessed at.
func TestRejectsUnsupportedConstructs(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"tab indent", "a:\n\tb: 1\n", "tab character"},
		{"flow mapping", "a: {b: 1}\n", "flow collections"},
		{"flow sequence", "a: [1, 2]\n", "flow collections"},
		{"anchor", "a: &x\n  b: 1\n", "anchors"},
		{"alias", "a: *x\n", "anchors"},
		{"block scalar", "a: |\n  text\n", "block scalars"},
		{"second document", "a: 1\n---\nb: 2\n", "multiple documents"},
		{"odd indent", "a:\n   b: 1\n", "two-space indentation"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := Parse([]byte(c.in))
			if err == nil {
				t.Fatalf("%s was accepted; this subset must refuse what it cannot model", c.name)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error %q does not explain %q", err, c.want)
			}
		})
	}
}

// TestErrorsNameTheLine: an operator correcting a file needs to be told where,
// and the parser strips comments and blanks before parsing, so the line number
// has to survive that.
func TestErrorsNameTheLine(t *testing.T) {
	in := "# a comment\n\nfirst: 1\n\n# another\nsecond: {flow: 1}\n"
	_, err := Parse([]byte(in))
	if err == nil {
		t.Fatal("expected a rejection")
	}
	if !strings.Contains(err.Error(), "line 6") {
		t.Errorf("error should name line 6, got %q", err)
	}
}

// TestUnknownKeysAreReported is schema/DESIGN.md §8 for hand-edited files: a
// typo must fail loudly rather than silently reverting to a default.
func TestUnknownKeysAreReported(t *testing.T) {
	n, err := Parse([]byte("known: 1\nunkown: 2\nnested:\n  good: 1\n  bad: 2\n"))
	if err != nil {
		t.Fatal(err)
	}
	r := NewReader(n, "")
	r.Str("known")
	sub := r.Sub("nested")
	sub.Str("good")

	errs := CollectErrors(r, sub)
	joined := strings.Join(errs, "\n")
	for _, want := range []string{`"unkown"`, `"bad"`} {
		if !strings.Contains(joined, want) {
			t.Errorf("unknown key %s not reported; got: %s", want, joined)
		}
	}
	// Every unknown key is collected, not just the first: an operator correcting
	// a file wants the whole list, not one error per edit-and-rerun cycle.
	if len(errs) != 2 {
		t.Errorf("expected 2 findings, got %d: %v", len(errs), errs)
	}
}

// TestRoundTrip: what this package writes, it reads back to the same thing.
func TestRoundTrip(t *testing.T) {
	root := Map()
	root.SetRaw("version", "a number, not a string", "1")
	root.Set("name", "with a comment above it", "cluster-a")
	root.Set("quoted", "", "9.3")
	root.SetList("items", "", []string{"a", "b"})

	inner := Map()
	inner.Set("kind", "", "slurm")
	root.SetNode("nested", "", inner)

	seq := Seq()
	m := Map()
	m.Set("k", "", "v")
	seq.Append(m)
	root.SetNode("list_of_maps", "", seq)

	first := root.Render()
	parsed, err := Parse([]byte(first))
	if err != nil {
		t.Fatalf("cannot read back what we wrote: %v\n%s", err, first)
	}
	r := NewReader(parsed, "")
	if got := r.IntOr("version", 0); got != 1 {
		t.Errorf("version = %d, want 1", got)
	}
	if got := r.Str("name"); got != "cluster-a" {
		t.Errorf("name = %q", got)
	}
	// A version-like string must survive as a string rather than becoming a
	// float, which is why the writer quotes it.
	if got := r.Str("quoted"); got != "9.3" {
		t.Errorf("quoted = %q, want 9.3", got)
	}
	if got := r.List("items"); len(got) != 2 || got[0] != "a" {
		t.Errorf("items = %v", got)
	}
	if got := r.Sub("nested").Str("kind"); got != "slurm" {
		t.Errorf("nested.kind = %q", got)
	}
	each := r.Each("list_of_maps")
	if len(each) != 1 || each[0].Str("k") != "v" {
		t.Errorf("list_of_maps did not round-trip")
	}
}

// TestRenderIsDeterministic: field order is fixed by construction, so two
// renders of the same document are byte-identical (invariant §2.7).
func TestRenderIsDeterministic(t *testing.T) {
	build := func() *Node {
		n := Map()
		n.Set("b", "", "2")
		n.Set("a", "", "1")
		n.SetList("l", "", []string{"z", "y"})
		return n
	}
	first := build().Render()
	for i := 0; i < 20; i++ {
		if got := build().Render(); got != first {
			t.Fatalf("render %d differs:\n%s\nvs\n%s", i, first, got)
		}
	}
	// Insertion order is preserved rather than sorted: the emitting code decides
	// the order, so that a generated file reads in a deliberate sequence.
	if strings.Index(first, "b:") > strings.Index(first, "a:") {
		t.Error("field order was not preserved")
	}
}

// TestEmptyValuesAreOmitted keeps generated files short enough to actually read.
func TestEmptyValuesAreOmitted(t *testing.T) {
	n := Map()
	n.Set("present", "", "x")
	n.Set("absent", "", "")
	n.SetList("empty_list", "", nil)
	out := n.Render()
	for _, unwanted := range []string{"absent", "empty_list"} {
		if strings.Contains(out, unwanted) {
			t.Errorf("%q should have been omitted:\n%s", unwanted, out)
		}
	}
}
