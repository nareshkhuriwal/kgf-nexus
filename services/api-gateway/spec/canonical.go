// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

// Package spec implements the submission pipeline: parse (JSON or YAML) ->
// validate structurally against the JSON Schema -> validate semantically via
// internal/dag -> canonicalize -> content-address.
//
// The pipeline order matters. Structural validation runs before semantic
// validation because the semantic checks assume well-formed types, and
// canonicalization runs last so the hash is only ever computed over a document
// we have already accepted.
package spec

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
)

// Canonicalize renders a decoded JSON value into deterministic bytes.
//
// This function IS the content address, so its output must be stable forever:
// a change to its formatting would re-hash every existing spec, breaking the
// "byte-identical resubmit is a no-op" guarantee and orphaning every stored
// version (ADR-0004). The rules are therefore explicit rather than delegated to
// encoding/json's defaults:
//
//   - Object keys sorted by UTF-8 code point. Go map iteration is deliberately
//     randomized, so without sorting the same document would hash differently on
//     consecutive calls in the same process.
//   - No insignificant whitespace, so pretty-printed and minified input agree.
//   - Numbers normalized through a single integer/shortest-float rule, so 1,
//     1.0, and 1e0 do not produce three different content addresses for one
//     document.
//   - Strings escaped WITHOUT Go's HTML escaping, which would otherwise turn
//     an innocuous & in a description into & and make the hash depend on an
//     encoder flag rather than on the document.
//
// Arrays are NOT sorted: element order is meaningful in tasks[] and edges[], and
// reordering would change the document's identity.
func Canonicalize(v any) ([]byte, error) {
	var buf bytes.Buffer
	if err := writeCanonical(&buf, v); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// CanonicalizeJSON parses raw JSON and re-renders it canonically.
//
// json.Number is used rather than float64 so an integer beyond 2^53 survives the
// round trip. Silently rounding memory_bytes for a large-memory task would be a
// real bug, not a theoretical one.
func CanonicalizeJSON(raw []byte) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("parse json: %w", err)
	}
	// Reject trailing content: two concatenated documents must not silently
	// canonicalize to just the first one.
	if dec.More() {
		return nil, fmt.Errorf("parse json: unexpected trailing content after top-level value")
	}
	return Canonicalize(v)
}

// ContentAddress returns the sha256 hex of canonical bytes. Callers pass bytes
// already produced by Canonicalize; hashing non-canonical bytes would make an
// identical document look like a new version.
func ContentAddress(canonical []byte) string {
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:])
}

func writeCanonical(buf *bytes.Buffer, v any) error {
	switch t := v.(type) {
	case nil:
		buf.WriteString("null")
	case bool:
		if t {
			buf.WriteString("true")
		} else {
			buf.WriteString("false")
		}
	case string:
		return writeCanonicalString(buf, t)
	case json.Number:
		return writeCanonicalNumber(buf, t.String())
	case float64:
		return writeCanonicalNumber(buf, strconv.FormatFloat(t, 'g', -1, 64))
	case int:
		buf.WriteString(strconv.Itoa(t))
	case int64:
		buf.WriteString(strconv.FormatInt(t, 10))
	case []any:
		buf.WriteByte('[')
		for i, elem := range t {
			if i > 0 {
				buf.WriteByte(',')
			}
			if err := writeCanonical(buf, elem); err != nil {
				return err
			}
		}
		buf.WriteByte(']')
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		// Plain byte-wise sort gives UTF-8 code point order for Go strings,
		// which is the same order a JCS-style canonicalizer produces.
		sort.Strings(keys)
		buf.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				buf.WriteByte(',')
			}
			if err := writeCanonicalString(buf, k); err != nil {
				return err
			}
			buf.WriteByte(':')
			if err := writeCanonical(buf, t[k]); err != nil {
				return err
			}
		}
		buf.WriteByte('}')
	default:
		return fmt.Errorf("canonicalize: unsupported type %T", v)
	}
	return nil
}

// writeCanonicalNumber normalizes a numeric literal.
//
// Integers are emitted as plain decimal integers; anything else uses the
// shortest round-trippable float form. One rule, applied to every number,
// regardless of how the author wrote it.
func writeCanonicalNumber(buf *bytes.Buffer, lit string) error {
	if i, err := strconv.ParseInt(lit, 10, 64); err == nil {
		buf.WriteString(strconv.FormatInt(i, 10))
		return nil
	}
	f, err := strconv.ParseFloat(lit, 64)
	if err != nil {
		return fmt.Errorf("canonicalize: %q is not a valid number: %w", lit, err)
	}
	// NaN and Inf have no JSON representation, so admitting them would produce
	// bytes we could never parse back.
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return fmt.Errorf("canonicalize: %q is not representable in JSON", lit)
	}
	// A float that happens to be integral collapses to the integer form, so
	// 10.0 and 10 are one document rather than two.
	if f == math.Trunc(f) && math.Abs(f) < 1e15 {
		buf.WriteString(strconv.FormatInt(int64(f), 10))
		return nil
	}
	buf.WriteString(strconv.FormatFloat(f, 'g', -1, 64))
	return nil
}

// writeCanonicalString emits a JSON string with minimal, deterministic escaping.
//
// Hand-rolled rather than encoding/json because json.Marshal escapes <, >, and &
// by default (an HTML-safety measure irrelevant to a stored spec). Depending on
// that flag would mean the content address changes if the flag ever does.
func writeCanonicalString(buf *bytes.Buffer, s string) error {
	buf.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			buf.WriteString(`\"`)
		case '\\':
			buf.WriteString(`\\`)
		case '\n':
			buf.WriteString(`\n`)
		case '\r':
			buf.WriteString(`\r`)
		case '\t':
			buf.WriteString(`\t`)
		case '\b':
			buf.WriteString(`\b`)
		case '\f':
			buf.WriteString(`\f`)
		default:
			if r < 0x20 {
				fmt.Fprintf(buf, `\u%04x`, r)
				continue
			}
			// Everything else, including all non-ASCII, is emitted literally as
			// UTF-8. Escaping it to \uXXXX would be equally valid JSON but is a
			// second representation of the same string, and two representations
			// mean two possible hashes.
			buf.WriteRune(r)
		}
	}
	buf.WriteByte('"')
	return nil
}
