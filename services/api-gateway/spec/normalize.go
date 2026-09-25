// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package spec

import "github.com/khuriwalgroup/kgf-nexus/internal/specenum"

// NormalizeEnums rewrites every enum-valued field in a validated document to its
// SHORT canonical spelling, in place.
//
// Why normalize at all, rather than merely accepting both spellings: the stored
// document is CONTENT-ADDRESSED (ADR-0004). Without this, the same workflow
// written by the Python SDK ("IDEMPOTENCY_MODE_REQUIRED") and by hand
// ("required") would canonicalize to different bytes and therefore to different
// sha256 spec versions. The platform would report them as two distinct
// workflows, "resubmitting a byte-identical spec is a no-op" would quietly stop
// holding across authoring surfaces, and a team that moved a workflow from YAML
// to the SDK would see a spurious new version with no semantic change.
// Normalizing before canonicalization is what makes ADR-0004's "both surfaces
// produce the identical document" literally true of the stored bytes.
//
// SHORT is the canonical direction because it is the published authoring form:
// architecture 4.3's YAML, the schema's `default` keywords and the documentation
// all use it, and storing the protobuf constant would mean the document an
// operator reads back does not match the document the docs told them to write.
// The proto spelling remains ACCEPTED on input forever; only the stored form is
// pinned.
//
// This runs AFTER ValidateStructure, so every value it sees is already one of
// the two known spellings and an unrecognized string is impossible. Anything it
// does not recognize is therefore left untouched rather than defaulted, since
// silently rewriting an unknown value would be worse than storing it verbatim.
//
// The tables live in internal/specenum, shared with nexusctl, because a client
// that computes the content address must normalize exactly as the server does
// (CONTRACT-FINDINGS F124).
func NormalizeEnums(doc any) {
	specenum.Normalize(doc)
}
