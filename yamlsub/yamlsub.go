// Package yamlsub reads and writes the bounded YAML subset cairn uses for its
// reviewable configuration files: site.yaml and policy.yaml.
//
// It exists because scripts/verify-guards.sh §8 asserts that the shipped binary
// links no third-party code. cairn is aimed at sites that read what they deploy,
// and gopkg.in/yaml.v3 — a real dependency of the fixture loader — must not
// reach cmd/cairn. schema/encode.go already makes this trade for canonical JSON,
// and the reasoning carries: a bounded format we emit and consume ourselves is
// cheaper to review than a general parser, and it cannot surprise us.
//
// YAML rather than JSON because these files exist to be corrected by a human,
// and a file nobody can annotate is a file nobody will correct. Comments are the
// point.
//
// The supported subset is deliberately small:
//
//	key: scalar
//	key:            (nested block, two-space indent)
//	  child: scalar
//	list:
//	  - scalar
//	  - key: scalar  (mapping items)
//	empty: []
//	# comment
//
// Everything else — anchors, aliases, flow collections, block scalars, multiple
// documents, tabs — is rejected with an error naming the line. Silently
// accepting a construct we do not model would mean an admin's hand edit is read
// as something other than what they wrote, which is worse than refusing it.
//
// Decoding rejects unknown keys, for the same reason (schema/DESIGN.md §8): in a
// hand-edited file a typo must fail loudly rather than leave the file saying one
// thing and the tool doing another.
package yamlsub

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// nodeKind is the shape of a document node.
type nodeKind int

const (
	yScalar nodeKind = iota
	yRaw
	yMap
	ySeq
)

// field is one key/value pair in a mapping. Ordered: the emitted file's field
// order is fixed by construction, so two runs over the same input are
// byte-identical (invariant §2.7).
type field struct {
	key     string
	comment string
	val     *Node
}

// Node is a document node.
type Node struct {
	kind   nodeKind
	scalar string
	fields []field
	items  []*Node
}

func Scalar(s string) *Node { return &Node{kind: yScalar, scalar: s} }

func Map() *Node { return &Node{kind: yMap} }

func Seq() *Node { return &Node{kind: ySeq} }

// set appends a field. Empty scalars are dropped rather than emitted as `key: ""`
// — an absent fact and a fact discovered to be the empty string are the same
// thing here, and the shorter file is the one an admin will actually read.
func (n *Node) Set(key, comment, value string) {
	if value == "" {
		return
	}
	n.fields = append(n.fields, field{key: key, comment: comment, val: Scalar(value)})
}

// setRaw appends a field whose value is emitted unquoted.
//
// For the values that are genuinely not strings — the version integer and the
// booleans. Passing them through set would render `version: "1"`, which is a
// string containing a digit, and any other YAML Reader an admin points at this
// file would agree with that reading and be wrong.
func (n *Node) SetRaw(key, comment, value string) {
	n.fields = append(n.fields, field{key: key, comment: comment, val: &Node{kind: yRaw, scalar: value}})
}

// setNode appends a field holding a collection.
func (n *Node) SetNode(key, comment string, val *Node) {
	if val == nil {
		return
	}
	if val.kind == yMap && len(val.fields) == 0 {
		return
	}
	n.fields = append(n.fields, field{key: key, comment: comment, val: val})
}

// setList appends a sequence of scalars, omitting it entirely when empty.
func (n *Node) SetList(key, comment string, values []string) {
	if len(values) == 0 {
		return
	}
	s := Seq()
	for _, v := range values {
		s.items = append(s.items, Scalar(v))
	}
	n.fields = append(n.fields, field{key: key, comment: comment, val: s})
}

// ---------- emitting ----------

// encode renders a node at the given indent depth.
func (n *Node) render(b *strings.Builder, depth int) {
	pad := strings.Repeat("  ", depth)
	switch n.kind {
	case yMap:
		for _, f := range n.fields {
			writeComment(b, pad, f.comment)
			switch f.val.kind {
			case yScalar:
				fmt.Fprintf(b, "%s%s: %s\n", pad, f.key, quote(f.val.scalar))
			case yRaw:
				fmt.Fprintf(b, "%s%s: %s\n", pad, f.key, f.val.scalar)
			default:
				fmt.Fprintf(b, "%s%s:\n", pad, f.key)
				f.val.render(b, depth+1)
			}
		}
	case ySeq:
		for _, it := range n.items {
			switch it.kind {
			case yScalar:
				fmt.Fprintf(b, "%s- %s\n", pad, quote(it.scalar))
			case yMap:
				// The first field goes on the dash line, the rest align under it.
				for i, f := range it.fields {
					lead := pad + "  "
					if i == 0 {
						lead = pad + "- "
					}
					if f.val.kind == yScalar {
						fmt.Fprintf(b, "%s%s: %s\n", lead, f.key, quote(f.val.scalar))
						continue
					}
					if f.val.kind == yRaw {
						fmt.Fprintf(b, "%s%s: %s\n", lead, f.key, f.val.scalar)
						continue
					}
					fmt.Fprintf(b, "%s%s:\n", lead, f.key)
					f.val.render(b, depth+2)
				}
			}
		}
	}
}

func writeComment(b *strings.Builder, pad, comment string) {
	if comment == "" {
		return
	}
	for _, line := range strings.Split(comment, "\n") {
		if line == "" {
			b.WriteString(strings.TrimRight(pad+"#", " "))
			b.WriteString("\n")
			continue
		}
		fmt.Fprintf(b, "%s# %s\n", pad, line)
	}
}

// quote wraps a scalar in double quotes when leaving it bare would change its
// meaning — to us, or to any other YAML Reader an admin points at this file.
func quote(s string) string {
	if s == "" {
		return `""`
	}
	if needsQuote(s) {
		return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
	}
	return s
}

func needsQuote(s string) bool {
	if s != strings.TrimSpace(s) {
		return true
	}
	// A version like "9.3" or a value like "true" must survive as a string.
	if _, err := strconv.ParseFloat(s, 64); err == nil {
		return true
	}
	switch strings.ToLower(s) {
	case "true", "false", "null", "yes", "no", "on", "off", "~":
		return true
	}
	if strings.ContainsAny(s, ":#\"'{}[]&*|>%@`\n\t,") {
		return true
	}
	switch s[0] {
	case '-', '?', '!':
		return true
	}
	return false
}

// ---------- parsing ----------

// Parse reads the supported subset into a document node.
func Parse(data []byte) (*Node, error) {
	lines := strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")

	// Strip comments and blanks up front, keeping the original line number so
	// an error can name the line the admin actually edited.
	type srcLine struct {
		num    int
		indent int
		text   string
	}
	var src []srcLine
	for i, raw := range lines {
		num := i + 1
		if strings.ContainsRune(raw, '\t') {
			return nil, fmt.Errorf("line %d: tab character; YAML indentation must be spaces", num)
		}
		trimmed := strings.TrimSpace(raw)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if strings.HasPrefix(trimmed, "---") || strings.HasPrefix(trimmed, "...") {
			return nil, fmt.Errorf("line %d: multiple documents are not supported", num)
		}
		indent := len(raw) - len(strings.TrimLeft(raw, " "))
		if indent%2 != 0 {
			return nil, fmt.Errorf("line %d: indent of %d spaces; this file uses two-space indentation", num, indent)
		}
		src = append(src, srcLine{num: num, indent: indent, text: trimmed})
	}

	pos := 0
	var parseBlock func(indent int) (*Node, error)

	parseBlock = func(indent int) (*Node, error) {
		var node *Node
		for pos < len(src) {
			ln := src[pos]
			if ln.indent < indent {
				break
			}
			if ln.indent > indent {
				return nil, fmt.Errorf("line %d: unexpected indentation", ln.num)
			}

			if strings.HasPrefix(ln.text, "- ") || ln.text == "-" {
				if node == nil {
					node = Seq()
				}
				if node.kind != ySeq {
					return nil, fmt.Errorf("line %d: list item inside a mapping", ln.num)
				}
				item := strings.TrimSpace(strings.TrimPrefix(ln.text, "-"))
				pos++
				if item == "" {
					return nil, fmt.Errorf("line %d: empty list item", ln.num)
				}
				// A mapping item: the dash line carries the first key, and any
				// continuation is indented two past the dash.
				if key, val, ok := splitField(item); ok {
					m := Map()
					if err := addField(m, key, val, ln.num); err != nil {
						return nil, err
					}
					rest, err := parseBlock(indent + 2)
					if err != nil {
						return nil, err
					}
					if rest != nil {
						if rest.kind != yMap {
							return nil, fmt.Errorf("line %d: expected more keys for this list item", ln.num)
						}
						m.fields = append(m.fields, rest.fields...)
					}
					node.items = append(node.items, m)
					continue
				}
				scalar, err := unquote(item, ln.num)
				if err != nil {
					return nil, err
				}
				node.items = append(node.items, Scalar(scalar))
				continue
			}

			key, val, ok := splitField(ln.text)
			if !ok {
				return nil, fmt.Errorf("line %d: %q is neither `key: value` nor a `- ` list item", ln.num, ln.text)
			}
			if node == nil {
				node = Map()
			}
			if node.kind != yMap {
				return nil, fmt.Errorf("line %d: mapping key inside a list", ln.num)
			}
			pos++
			if val != "" {
				if err := addField(node, key, val, ln.num); err != nil {
					return nil, err
				}
				continue
			}
			child, err := parseBlock(indent + 2)
			if err != nil {
				return nil, err
			}
			if child == nil {
				// `key:` with nothing under it. An empty collection.
				child = Map()
			}
			node.fields = append(node.fields, field{key: key, val: child})
		}
		return node, nil
	}

	root, err := parseBlock(0)
	if err != nil {
		return nil, err
	}
	if pos != len(src) {
		return nil, fmt.Errorf("line %d: unexpected indentation", src[pos].num)
	}
	if root == nil {
		return Map(), nil
	}
	return root, nil
}

// splitField splits `key: value` on the first colon-space. A trailing bare colon
// means the value is a nested block.
func splitField(s string) (key, val string, ok bool) {
	if strings.HasSuffix(s, ":") {
		key = strings.TrimSpace(strings.TrimSuffix(s, ":"))
		if key == "" || strings.ContainsAny(key, ":") {
			return "", "", false
		}
		return key, "", true
	}
	i := strings.Index(s, ": ")
	if i < 0 {
		return "", "", false
	}
	key = strings.TrimSpace(s[:i])
	val = strings.TrimSpace(s[i+2:])
	if key == "" {
		return "", "", false
	}
	return key, val, true
}

func addField(n *Node, key, val string, num int) error {
	if val == "[]" {
		n.fields = append(n.fields, field{key: key, val: Seq()})
		return nil
	}
	s, err := unquote(val, num)
	if err != nil {
		return err
	}
	n.fields = append(n.fields, field{key: key, val: Scalar(s)})
	return nil
}

// unquote resolves a scalar, rejecting the constructs this subset does not model.
func unquote(s string, num int) (string, error) {
	if s == "" {
		return "", nil
	}
	switch s[0] {
	case '&', '*':
		return "", fmt.Errorf("line %d: anchors and aliases are not supported", num)
	case '{', '[':
		return "", fmt.Errorf("line %d: flow collections are not supported; use a block list", num)
	case '|', '>':
		return "", fmt.Errorf("line %d: block scalars are not supported", num)
	}
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		out, err := strconv.Unquote(s)
		if err != nil {
			return "", fmt.Errorf("line %d: malformed quoted string: %w", num, err)
		}
		return out, nil
	}
	if len(s) >= 2 && s[0] == '\'' && s[len(s)-1] == '\'' {
		return strings.ReplaceAll(s[1:len(s)-1], "''", "'"), nil
	}
	// A bare scalar may still carry a trailing comment.
	if i := strings.Index(s, " #"); i >= 0 {
		s = strings.TrimSpace(s[:i])
	}
	return s, nil
}

// ---------- typed access, with unknown-key rejection ----------

// Reader walks a parsed mapping, tracking which keys were consumed so that
// anything left over can be reported.
//
// schema/DESIGN.md §8 makes this the rule for decoding bundles, and it matters
// more here: site.yaml is hand-edited by design, so `partitons:` must be an
// error. Ignoring it would leave an admin looking at a file that says one thing
// and a tool doing another, which is the specific failure this whole feature
// exists to prevent.
type Reader struct {
	node *Node
	used map[string]bool
	path string
	errs []string
}

func NewReader(n *Node, path string) *Reader {
	return &Reader{node: n, used: map[string]bool{}, path: path}
}

func (r *Reader) field(key string) *Node {
	r.used[key] = true
	if r.node == nil {
		return nil
	}
	for _, f := range r.node.fields {
		if f.key == key {
			return f.val
		}
	}
	return nil
}

func (r *Reader) Str(key string) string {
	n := r.field(key)
	if n == nil || (n.kind != yScalar && n.kind != yRaw) {
		return ""
	}
	return n.scalar
}

func (r *Reader) Bool(key string) bool {
	switch strings.ToLower(r.Str(key)) {
	case "true", "yes", "on":
		return true
	}
	return false
}

func (r *Reader) IntOr(key string, def int) int {
	s := r.Str(key)
	if s == "" {
		return def
	}
	v, err := strconv.Atoi(s)
	if err != nil {
		r.errs = append(r.errs, fmt.Sprintf("%s%s: %q is not a number", r.path, key, s))
		return def
	}
	return v
}

func (r *Reader) List(key string) []string {
	n := r.field(key)
	if n == nil || n.kind != ySeq {
		return nil
	}
	var out []string
	for _, it := range n.items {
		if it.kind == yScalar {
			out = append(out, it.scalar)
		}
	}
	return out
}

// sub returns a Reader over a nested mapping.
func (r *Reader) Sub(key string) *Reader {
	n := r.field(key)
	if n == nil || n.kind != yMap {
		return NewReader(nil, r.path+key+".")
	}
	return NewReader(n, r.path+key+".")
}

// seq returns readers over each mapping in a sequence.
func (r *Reader) Each(key string) []*Reader {
	n := r.field(key)
	if n == nil || n.kind != ySeq {
		return nil
	}
	var out []*Reader
	for i, it := range n.items {
		if it.kind != yMap {
			continue
		}
		out = append(out, NewReader(it, fmt.Sprintf("%s%s[%d].", r.path, key, i)))
	}
	return out
}

// checkUnknown records any key the caller never asked for.
func (r *Reader) checkUnknown() {
	if r.node == nil {
		return
	}
	var unknown []string
	for _, f := range r.node.fields {
		if !r.used[f.key] {
			unknown = append(unknown, f.key)
		}
	}
	sort.Strings(unknown)
	where := "at the top level"
	if r.path != "" {
		where = "under " + strings.TrimSuffix(r.path, ".")
	}
	for _, k := range unknown {
		r.errs = append(r.errs, fmt.Sprintf("unknown key %q %s", k, where))
	}
}

// collect gathers errors from this Reader and its children.
func CollectErrors(rs ...*Reader) []string {
	var out []string
	for _, r := range rs {
		if r == nil {
			continue
		}
		r.checkUnknown()
		out = append(out, r.errs...)
	}
	return out
}

// Append adds an item to a sequence node.
//
// Exported so a caller building a list of mappings never touches Node's fields;
// the document model stays this package's business.
func (n *Node) Append(item *Node) { n.items = append(n.items, item) }

// IsMap reports whether the node is a mapping. Used to check that a parsed
// document has the shape a decoder expects before it starts reading keys.
func (n *Node) IsMap() bool { return n != nil && n.kind == yMap }

// Render returns the node as YAML text, starting at the left margin.
func (n *Node) Render() string {
	var b strings.Builder
	n.render(&b, 0)
	return b.String()
}
