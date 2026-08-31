package schema

import (
	"bytes"
	"encoding/json"
	"fmt"
)

type wireJobID struct {
	Raw        string  `json:"raw"`
	Base       uint64  `json:"base"`
	ArrayTask  *uint32 `json:"array_task,omitempty"`
	ArrayRange string  `json:"array_range,omitempty"`
	HetOffset  *uint32 `json:"het_offset,omitempty"`
	Step       string  `json:"step,omitempty"`
}

type wireDetail struct {
	Signature string            `json:"signature"`
	Attrs     map[string]string `json:"attrs,omitempty"`
}

type wireEvent struct {
	TS      string     `json:"ts"`
	Cluster string     `json:"cluster"`
	Node    string     `json:"node,omitempty"`
	JobID   *wireJobID `json:"jobid"`
	Source  string     `json:"source"`
	Class   string     `json:"class"`
	Detail  wireDetail `json:"detail"`
}

// DecodeEvents reads a canonical event stream.
//
// Unknown fields are rejected rather than ignored. A bundle written by a newer
// schema version must fail loudly here; silently dropping fields would let a
// version mismatch degrade into wrong answers, which is worse than an error.
func DecodeEvents(data []byte) ([]Event, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()

	var wire []wireEvent
	if err := dec.Decode(&wire); err != nil {
		return nil, fmt.Errorf("schema: decoding events: %w", err)
	}

	out := make([]Event, 0, len(wire))
	for i, w := range wire {
		ts, err := ParseTime(w.TS)
		if err != nil {
			return nil, fmt.Errorf("schema: event %d: %w", i, err)
		}
		src := Source(w.Source)
		if !src.Valid() {
			return nil, fmt.Errorf("schema: event %d: unknown source %q", i, w.Source)
		}
		cls, err := ParseClass(w.Class)
		if err != nil {
			return nil, fmt.Errorf("schema: event %d: %w", i, err)
		}

		var jid *JobID
		if w.JobID != nil {
			jid = &JobID{
				Raw:        w.JobID.Raw,
				Base:       w.JobID.Base,
				ArrayTask:  w.JobID.ArrayTask,
				ArrayRange: w.JobID.ArrayRange,
				HetOffset:  w.JobID.HetOffset,
				Step:       w.JobID.Step,
			}
		}

		e := Event{
			TS:      ts,
			Cluster: ClusterName(w.Cluster),
			Node:    Hostname(w.Node),
			JobID:   jid,
			Source:  src,
			Class:   cls,
			Detail:  Detail{Signature: w.Detail.Signature, Attrs: w.Detail.Attrs},
		}
		if err := e.Validate(); err != nil {
			return nil, fmt.Errorf("schema: event %d: %w", i, err)
		}
		out = append(out, e)
	}
	return out, nil
}

// IsCanonical reports whether data is exactly what EncodeEvents would produce
// for the events it contains.
//
// This is the check that makes the fixture corpus usable as an eval set: a
// golden file that decodes correctly but is not byte-canonical will start
// failing the moment a collector produces the same events in a different order,
// and the failure will look like a regression rather than a formatting problem.
func IsCanonical(data []byte) (bool, []byte, error) {
	evs, err := DecodeEvents(data)
	if err != nil {
		return false, nil, err
	}
	want, err := EncodeEvents(evs)
	if err != nil {
		return false, nil, err
	}
	return bytes.Equal(data, want), want, nil
}
