// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package dag

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
)

// InputsDigest reduces a node's resolved input_refs to a canonical document of
// CONTENT hashes, the "canonical_hash(inputs)" of §6.4.
//
// Each input becomes {"sha256": <hex of the value's bytes>, "content_type": ...}:
//
//   - an inline value is hashed directly;
//   - a blob is represented by its content address (BlobRef.sha256), which the
//     result store computes over the same bytes (ADR-0008), so one value hashes
//     identically whether it travelled inline or as a blob.
//
// Hashing content rather than the ref's JSON matters because refs have several
// encodings of one value (protojson's string-typed size_bytes, an omitted
// content_type, key order). A key over the encoding would silently miss, and a
// cache that misses is far harder to notice than one that errors.
//
// A blob ref with no sha256 cannot be content-addressed and returns an error;
// the caller then runs the task unmemoized rather than risk a wrong hit.
func InputsDigest(inputRefs []byte) ([]byte, error) {
	refs := map[string]json.RawMessage{}
	if len(inputRefs) > 0 {
		if err := json.Unmarshal(inputRefs, &refs); err != nil {
			return nil, fmt.Errorf("input_refs is not an object: %w", err)
		}
	}
	names := make([]string, 0, len(refs))
	for k := range refs {
		names = append(names, k)
	}
	sort.Strings(names)

	type entry struct {
		SHA256      string `json:"sha256"`
		ContentType string `json:"content_type"`
	}
	out := make(map[string]entry, len(refs))
	for _, name := range names {
		ref := refs[name]
		var meta struct {
			Blob *struct {
				SHA256 string `json:"sha256"`
			} `json:"blob"`
			ContentType  string `json:"content_type"`
			ContentType2 string `json:"contentType"`
		}
		_ = json.Unmarshal(ref, &meta)
		ct := meta.ContentType
		if ct == "" {
			ct = meta.ContentType2
		}
		if meta.Blob != nil {
			if meta.Blob.SHA256 == "" {
				return nil, fmt.Errorf("input %q is a blob with no content address", name)
			}
			out[name] = entry{SHA256: meta.Blob.SHA256, ContentType: ct}
			continue
		}
		payload, _, err := inlinePayload(ref)
		if err != nil {
			return nil, fmt.Errorf("input %q: %w", name, err)
		}
		sum := sha256.Sum256(payload)
		out[name] = entry{SHA256: hex.EncodeToString(sum[:]), ContentType: ct}
	}
	// encoding/json sorts map keys, so this is already canonical.
	b, err := json.Marshal(out)
	if err != nil {
		return nil, fmt.Errorf("marshal inputs digest: %w", err)
	}
	return b, nil
}
