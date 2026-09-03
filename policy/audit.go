package policy

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// AuditLogEnv overrides where the audit log is written.
const AuditLogEnv = "CAIRN_AUDIT_LOG"

// Sink records decisions.
//
// An interface so a test can supply one that fails, which is the case that
// matters: the engine must deny when it cannot record. That behavior is
// impossible to exercise against a real file without deleting the operator's
// directory mid-test.
type Sink interface {
	// Record persists one decision. A non-nil error must cause the caller to
	// deny, never to proceed.
	Record(Decision) error
	// Where names the sink for operator output.
	Where() string
}

// FileSink appends decisions to a JSONL file.
//
// The shape follows cmd/cairn/miss.go — append-only, 0600 file in a 0700
// directory — because that is the established way this project keeps local
// operator state, and an audit log has exactly the same requirements plus one:
// it must never be rewritten, only appended to.
type FileSink struct{ Path string }

// NewFileSink resolves the audit log path.
func NewFileSink(path string) (*FileSink, error) {
	if path == "" {
		path = os.Getenv(AuditLogEnv)
	}
	if path == "" {
		dir, err := os.UserConfigDir()
		if err != nil {
			return nil, fmt.Errorf("locating the audit log: %w (set %s instead)", err, AuditLogEnv)
		}
		path = filepath.Join(dir, "cairn", "audit.jsonl")
	}
	return &FileSink{Path: path}, nil
}

func (f *FileSink) Where() string { return f.Path }

func (f *FileSink) Record(d Decision) error {
	if err := os.MkdirAll(filepath.Dir(f.Path), 0o700); err != nil {
		return err
	}
	fh, err := os.OpenFile(f.Path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer fh.Close()
	if _, err := fh.Write(append(d.encode(), '\n')); err != nil {
		return err
	}
	// Flushed to disk before the caller is told it may proceed. Without this a
	// crash between the decision and the action would leave an actuation that
	// happened and no record that it did, which is the one failure this whole
	// file exists to prevent.
	return fh.Sync()
}

// Decision is the outcome of evaluating a request.
type Decision struct {
	// At is when the decision was made.
	//
	// A bundle deliberately carries no generation stamp, because two collections
	// over one window must compare byte-for-byte (schema/bundle.go). An audit
	// record is the opposite kind of artifact: it is *about* when something was
	// decided, and a log of identical undated lines would be useless. The
	// asymmetry is intentional, not an oversight of §2.7.
	At time.Time

	Allowed bool
	Kind    Kind
	Target  Target
	Reason  string

	// Gate names which check refused, empty when allowed. This is what makes a
	// denial actionable: "denied" tells an operator nothing, "denied: no such
	// action in this build" tells them whether to edit the policy or stop.
	Gate string

	// Explain is the human sentence for Gate.
	Explain string

	// DryRun records whether execution was suppressed. An allowed dry-run
	// decision and an allowed executed one must never look the same in the log.
	DryRun bool

	// Executed records whether anything actually happened.
	Executed bool
}

// encode renders a decision as one canonical JSON line.
//
// Hand-rolled with a fixed field order, like schema/encode.go: an audit log is
// read by grep and diff as often as by a parser, and stable field order is what
// makes those work.
func (d Decision) encode() []byte {
	var b strings.Builder
	b.WriteString(`{"at":`)
	b.WriteString(strconv.Quote(d.At.UTC().Format(time.RFC3339Nano)))
	b.WriteString(`,"allowed":`)
	b.WriteString(strconv.FormatBool(d.Allowed))
	b.WriteString(`,"executed":`)
	b.WriteString(strconv.FormatBool(d.Executed))
	b.WriteString(`,"dry_run":`)
	b.WriteString(strconv.FormatBool(d.DryRun))
	b.WriteString(`,"kind":`)
	b.WriteString(strconv.Quote(string(d.Kind)))
	b.WriteString(`,"node":`)
	b.WriteString(strconv.Quote(d.Target.Node))
	b.WriteString(`,"job":`)
	b.WriteString(strconv.Quote(d.Target.Job))
	b.WriteString(`,"gate":`)
	b.WriteString(strconv.Quote(d.Gate))
	b.WriteString(`,"explain":`)
	b.WriteString(strconv.Quote(d.Explain))
	b.WriteString(`,"reason":`)
	b.WriteString(strconv.Quote(d.Reason))
	b.WriteString("}")
	return []byte(b.String())
}

// String renders a decision for a terminal.
func (d Decision) String() string {
	verdict := "DENIED"
	if d.Allowed {
		verdict = "allowed"
		if !d.Executed {
			verdict = "allowed (dry run, nothing executed)"
		}
	}
	s := fmt.Sprintf("%s %s %s", verdict, d.Kind, d.Target)
	if d.Explain != "" {
		s += "\n  " + d.Explain
	}
	return s
}
