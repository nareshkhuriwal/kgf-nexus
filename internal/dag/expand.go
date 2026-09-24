// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package dag

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"sort"

	nexusv1 "github.com/khuriwalgroup/kgf-nexus/gen/nexus/v1"
	"google.golang.org/protobuf/proto"
)

// ── Dynamic expansion (§5.4): the pure half ───────────────────────────────────
//
// A map TEMPLATE is a node whose TaskSpec carries a MapSpec. It never executes.
// When its trigger rule fires, the service layer reads the collection named by
// the MapSpec from the upstream's output and materializes one CHILD node per
// element, in one transaction. Children are ordinary nodes to everything in this
// package: they get a frontier, trigger rules, propagation, retries and terminal
// detection from the same code that handles static nodes.
//
// The template's own state is an AGGREGATE of its children (AggregateChildren),
// and downstream nodes depend on the template rather than on the children. That
// keeps a fan-in over ten thousand items a node with ONE predecessor instead of
// ten thousand, which is what keeps the trigger tally and the task_edges table
// linear in the size of the spec rather than in the size of the data.
//
// Everything below is deterministic in its inputs. Child ids, their order, the
// element each receives and the fan-in value are pure functions of the
// collection bytes, so a crash-and-replay of an expansion produces byte-identical
// rows and the idempotency key of child i is the same on every attempt (§6.2).

// DefaultMaxFanoutCeiling is the platform-wide upper bound on a template's
// declared max_fanout when no operator override is configured.
//
// The spec's max_fanout is the AUTHOR's bound; this is the OPERATOR's. Without
// one, an author could declare max_fanout: 100000000 and the "mandatory bound"
// of §5.4 would bound nothing. 10,000 children is comfortably inside what one
// Postgres transaction inserts in well under a second, and the fan-in value that
// collects their outputs stays in the low megabytes at the inline threshold.
const DefaultMaxFanoutCeiling = 10000

// ChildID names expansion child i of a template. Deterministic by construction:
// the node id participates in the event log and the UI, and a replayed
// expansion must address the rows the first attempt wrote.
//
// Brackets are rejected in authored node ids (validateNodes), so a child id can
// never collide with a node the author declared.
func ChildID(templateID string, index int) string {
	return fmt.Sprintf("%s[%d]", templateID, index)
}

// ExpansionFailure is a DELIBERATE, author-visible refusal to expand. It is not
// an infrastructure error: retrying cannot change the outcome, so the template
// node fails and the reason is recorded, exactly as §5.4 requires of a runaway
// fan-out ("fail the expansion explicitly").
type ExpansionFailure struct {
	Reason string // stable, machine-readable
	Detail string
}

func (e *ExpansionFailure) Error() string { return e.Reason + ": " + e.Detail }

// Expansion failure reasons. Stable strings: they are stored in
// task_expansions.reason and rendered by the console.
const (
	ExpandMaxFanoutExceeded   = "max_fanout_exceeded"
	ExpandCollectionMissing   = "collection_missing"
	ExpandCollectionNotArray  = "collection_not_array"
	ExpandCollectionNotInline = "collection_not_inline"
)

// PlannedChild is one child an expansion will materialize.
type PlannedChild struct {
	NodeID   string
	MapIndex int
	// Item is the element's canonical JSON, delivered to the child's
	// item_input as an inline value.
	Item json.RawMessage
}

// PlanFromCollection decodes the upstream's collection and plans the children.
//
// `ref` is the upstream output exactly as stored in task_attempts.output_ref: a
// protojson ResultRef. Only an INLINE collection can be expanded. Reading a blob
// would require the control plane to fetch a task payload from the result store,
// which invariant 6 forbids; refusing is honest, and the failure names the fix
// (emit a smaller collection of references).
//
// maxFanout is the template's declared bound. A collection exactly at the bound
// is accepted; one element over it is refused with nothing materialized.
func PlanFromCollection(templateID string, ref json.RawMessage, maxFanout int) ([]PlannedChild, error) {
	if len(ref) == 0 || bytes.Equal(bytes.TrimSpace(ref), []byte("null")) {
		return nil, &ExpansionFailure{Reason: ExpandCollectionMissing,
			Detail: "the upstream produced no value for the mapped output"}
	}
	payload, inline, err := inlinePayload(ref)
	if err != nil {
		return nil, &ExpansionFailure{Reason: ExpandCollectionNotArray, Detail: err.Error()}
	}
	if !inline {
		return nil, &ExpansionFailure{Reason: ExpandCollectionNotInline,
			Detail: "the collection was stored as a blob; the control plane never reads task payloads " +
				"(invariant 6), so a mapped collection must fit the inline threshold -- emit a list of " +
				"references (object keys, ids) rather than the data itself"}
	}
	var items []json.RawMessage
	dec := json.NewDecoder(bytes.NewReader(payload))
	dec.UseNumber()
	if err := dec.Decode(&items); err != nil {
		return nil, &ExpansionFailure{Reason: ExpandCollectionNotArray,
			Detail: fmt.Sprintf("the mapped output is not a JSON array: %v", err)}
	}
	// PlanExpansion owns the bound so the count-based and collection-based paths
	// cannot disagree about what "over the limit" means.
	if _, err := PlanExpansion(ExpansionRequest{
		TemplateNodeID: templateID, Cardinality: len(items), MaxFanout: maxFanout,
	}); err != nil {
		return nil, &ExpansionFailure{Reason: ExpandMaxFanoutExceeded, Detail: err.Error()}
	}
	out := make([]PlannedChild, 0, len(items))
	for i, it := range items {
		canon, cerr := CanonicalJSON(it)
		if cerr != nil {
			return nil, &ExpansionFailure{Reason: ExpandCollectionNotArray,
				Detail: fmt.Sprintf("element %d is not valid JSON: %v", i, cerr)}
		}
		out = append(out, PlannedChild{NodeID: ChildID(templateID, i), MapIndex: i, Item: canon})
	}
	return out, nil
}

// ChildSpec derives a child's TaskSpec from its template.
//
// A deep copy, so the template's spec in the run graph can never be mutated
// through a child. The MapSpec is cleared: a child is an ordinary executable
// task, and leaving the MapSpec on it would make it look like a template to the
// scheduler-facing spec the worker receives. The id is the child's id, because
// the worker reports and the UI render per node.
func ChildSpec(template *nexusv1.TaskSpec, childID string) *nexusv1.TaskSpec {
	c := proto.Clone(template).(*nexusv1.TaskSpec)
	c.Id = childID
	c.Map = nil
	return c
}

// ChildEdges derives a child's incoming edges from the template's.
//
// A child depends on everything its template depended on, with the same input
// bindings: a template whose `config` input comes from an upstream `load_config`
// node must deliver that config to every child. Outgoing edges are NOT copied;
// downstream nodes depend on the template, whose state aggregates the children.
func ChildEdges(g *Graph, templateID, childID string) []Edge {
	var out []Edge
	for _, e := range g.edgeByTo[templateID] {
		m := make(map[string]string, len(e.OutputToInput))
		for k, v := range e.OutputToInput {
			m[k] = v
		}
		out = append(out, Edge{From: e.From, To: childID, OutputToInput: m})
	}
	return out
}

// AggregateChildren folds a template's children into the template's state.
// ok is false while any child is still non-terminal.
//
// The precedence is the same one the run outcome uses (§5.2): a failure
// dominates, then an operator's cancel, then a policy skip. A template whose
// children all succeeded -- including the vacuous case of zero children --
// SUCCEEDED, so an empty collection lets the fan-in run over an empty list
// rather than wedging.
func AggregateChildren(states []NodeState) (NodeState, bool) {
	var failed, cancelled, skipped int
	for _, s := range states {
		if !s.Terminal() {
			return "", false
		}
		switch s {
		case NodeFailed, NodeUpstreamFailed:
			failed++
		case NodeCancelled:
			cancelled++
		case NodeSkipped:
			skipped++
		}
	}
	switch {
	case failed > 0:
		return NodeFailed, true
	case cancelled > 0:
		return NodeCancelled, true
	case skipped > 0:
		return NodeSkipped, true
	default:
		return NodeSucceeded, true
	}
}

// foldTemplates settles every EXPANDED template whose children are all
// terminal. An expanded template is one in RUNNING: the expansion transaction
// moves it there atomically with inserting its children, so RUNNING on a
// template means exactly "expanded and not yet settled".
func foldTemplates(g *Graph, st States) []Transition {
	var out []Transition
	for _, id := range g.Templates() {
		if st.Get(id) != NodeRunning {
			continue
		}
		kids := g.children[id]
		states := make([]NodeState, len(kids))
		for i, k := range kids {
			states[i] = st.Get(k)
		}
		agg, ok := AggregateChildren(states)
		if !ok {
			continue
		}
		out = append(out, Transition{NodeID: id, From: NodeRunning, To: agg, Reason: ReasonExpansionSettled})
		st[id] = agg
	}
	return out
}

// DefaultMaxFanInBytes bounds the assembled fan-in array (the JSON payload,
// before its base64 wrapping in the ResultRef). The array is built IN THE
// CONTROL PLANE, so without a bound max_fanout x the inline threshold -- 10,000 x
// 64 KiB, 640 MiB -- is a single allocation inside the DAG engine and then a
// single row in task_attempts. 16 MiB is ~250x a typical per-child summary and
// comfortably inside what one Postgres row and one gRPC message carry.
const DefaultMaxFanInBytes int64 = 16 << 20

// MaxFanInBytesCeiling is the most an operator may configure. Above it the
// failure the limit exists to prevent (one reconcile holding hundreds of MiB)
// is back; a workflow that needs more should pass references, not values.
const MaxFanInBytesCeiling int64 = 256 << 20

// MinFanInBytes is the least an operator may configure: one inline child
// output must always fit, or no fan-in could ever run.
const MinFanInBytes int64 = 64 << 10

// FanInTooLarge is the deliberate refusal to assemble a fan-in over the limit.
// Like an ExpansionFailure it is the workflow's shape, not an infrastructure
// fault: retrying cannot change it, so the consuming node fails with this reason
// recorded.
type FanInTooLarge struct {
	Template string
	Output   string
	Limit    int64
	// Children is how many child values had been accepted when the limit was hit.
	Children int
	// Total is how many children the template has.
	Total int
}

// FanInTooLargeReason is the stable, machine-readable reason recorded for it.
const FanInTooLargeReason = "fan_in_too_large"

func (e *FanInTooLarge) Error() string {
	return fmt.Sprintf("%s: the fan-in of %s.%s exceeds the %d-byte limit after %d of %d children; "+
		"raise NEXUS_DAG_MAX_FANIN_BYTES (max %d) or have the children write large results as blob "+
		"outputs (passed by reference) or smaller summaries",
		FanInTooLargeReason, e.Template, e.Output, e.Limit, e.Children, e.Total, MaxFanInBytesCeiling)
}

// FanInBuilder assembles the value a downstream input receives from a
// template's output: a JSON array with one element per child, in map_index
// order, INCREMENTALLY and BOUNDED.
//
// Each element is the child's value for that output. An inline JSON value is
// embedded as itself; an inline non-JSON payload as {"inline_base64": "..."};
// a blob as {"result_ref": <ResultRef>} so the handler can fetch it directly
// from the result store (the control plane never does). A child that did not
// succeed contributes null, so element i always belongs to child i and a fan-in
// tolerating partial failure (all_done) can tell which items are missing.
//
// The size is checked BEFORE each element is appended, so an over-limit fan-in
// stops at the element that would cross the limit: the builder never holds
// more than `limit` bytes, and the caller -- which feeds it from a streaming
// query -- stops reading rows too. Build-then-measure would already have paid
// the allocation the limit exists to prevent.
type FanInBuilder struct {
	template, output string
	limit            int64
	total, n         int
	buf              bytes.Buffer
}

// NewFanInBuilder starts a fan-in of `total` children. limit <= 0 means
// DefaultMaxFanInBytes.
func NewFanInBuilder(template, output string, total int, limit int64) *FanInBuilder {
	if limit <= 0 {
		limit = DefaultMaxFanInBytes
	}
	b := &FanInBuilder{template: template, output: output, limit: limit, total: total}
	b.buf.WriteByte('[')
	return b
}

// Add appends child n's value (ref nil or empty when it did not succeed).
// Returns *FanInTooLarge, with nothing further appended, when the element would
// take the array (including its closing bracket) past the limit.
func (b *FanInBuilder) Add(ref json.RawMessage) error {
	elem := json.RawMessage("null")
	if len(ref) > 0 && !bytes.Equal(bytes.TrimSpace(ref), []byte("null")) {
		payload, inline, err := inlinePayload(ref)
		if err != nil {
			return fmt.Errorf("child %d output %q: %w", b.n, b.output, err)
		}
		switch {
		case !inline:
			elem = json.RawMessage(`{"result_ref":` + string(ref) + `}`)
		case json.Valid(payload):
			c, cerr := CanonicalJSON(payload)
			if cerr != nil {
				return fmt.Errorf("child %d output %q: %w", b.n, b.output, cerr)
			}
			elem = c
		default:
			enc, _ := json.Marshal(map[string]string{"inline_base64": base64.StdEncoding.EncodeToString(payload)})
			elem = enc
		}
	}
	sep := 0
	if b.n > 0 {
		sep = 1
	}
	// +1 for the closing bracket Finish will write.
	if int64(b.buf.Len()+sep+len(elem)+1) > b.limit {
		return &FanInTooLarge{Template: b.template, Output: b.output, Limit: b.limit, Children: b.n, Total: b.total}
	}
	if sep == 1 {
		b.buf.WriteByte(',')
	}
	b.buf.Write(elem)
	b.n++
	return nil
}

// Size is the assembled payload so far, closing bracket included.
func (b *FanInBuilder) Size() int { return b.buf.Len() + 1 }

// Finish closes the array and wraps it as an inline ResultRef.
func (b *FanInBuilder) Finish() (json.RawMessage, error) {
	b.buf.WriteByte(']')
	return InlineJSONRef(b.buf.Bytes())
}

// FanInValue is FanInBuilder over an in-memory slice, for callers that already
// hold the children's outputs. childOutputs[i] is child i's output map (nil when
// it did not succeed).
func FanInValue(childOutputs []map[string]json.RawMessage, output string, limit int64) (json.RawMessage, error) {
	b := NewFanInBuilder("", output, len(childOutputs), limit)
	for _, outs := range childOutputs {
		if err := b.Add(outs[output]); err != nil {
			return nil, err
		}
	}
	return b.Finish()
}

// InlineJSONRef wraps a JSON payload as an inline protojson ResultRef, the
// shape task_attempts.input_refs values take.
func InlineJSONRef(payload []byte) (json.RawMessage, error) {
	b, err := json.Marshal(map[string]any{
		"inline":       payload, // []byte marshals as base64, matching protojson bytes
		"content_type": "application/json",
		"size_bytes":   len(payload),
	})
	if err != nil {
		return nil, fmt.Errorf("marshal inline ref: %w", err)
	}
	return b, nil
}

// inlinePayload extracts the bytes of an inline ResultRef. inline is false for
// a blob reference. A value that is not a ResultRef object at all is treated as
// a bare inline JSON value, which is how older fixtures and hand-written test
// outputs look.
func inlinePayload(ref json.RawMessage) (payload []byte, inline bool, err error) {
	var obj map[string]json.RawMessage
	if json.Unmarshal(ref, &obj) != nil {
		// Not an object: a bare JSON value (array, number...). Treat as inline.
		return ref, true, nil
	}
	if _, ok := obj["blob"]; ok {
		return nil, false, nil
	}
	raw, ok := obj["inline"]
	if !ok {
		// An object that is not a ResultRef: the value itself.
		return ref, true, nil
	}
	var b []byte
	if err := json.Unmarshal(raw, &b); err != nil {
		return nil, false, fmt.Errorf("inline payload is not base64: %w", err)
	}
	return b, true, nil
}

// CanonicalJSON re-encodes a JSON value with sorted object keys and no
// insignificant whitespace, preserving number spelling exactly.
//
// Used wherever bytes are hashed or compared (memoization keys, expansion
// items), because two encodings of one value must be one key: a cache that
// silently misses on key order is far harder to notice than one that errors.
func CanonicalJSON(raw []byte) (json.RawMessage, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("decode json: %w", err)
	}
	var buf bytes.Buffer
	if err := writeCanonical(&buf, v); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func writeCanonical(buf *bytes.Buffer, v any) error {
	switch t := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		buf.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				buf.WriteByte(',')
			}
			kb, _ := json.Marshal(k)
			buf.Write(kb)
			buf.WriteByte(':')
			if err := writeCanonical(buf, t[k]); err != nil {
				return err
			}
		}
		buf.WriteByte('}')
	case []any:
		buf.WriteByte('[')
		for i, e := range t {
			if i > 0 {
				buf.WriteByte(',')
			}
			if err := writeCanonical(buf, e); err != nil {
				return err
			}
		}
		buf.WriteByte(']')
	case json.Number:
		buf.WriteString(t.String())
	default:
		b, err := json.Marshal(t)
		if err != nil {
			return fmt.Errorf("encode json: %w", err)
		}
		buf.Write(b)
	}
	return nil
}
