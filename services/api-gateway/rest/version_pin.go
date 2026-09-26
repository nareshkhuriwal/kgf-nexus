// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package rest

import (
	"bytes"
	"encoding/json"
	"errors"
)

// versionPin is a trigger or sensor request's workflow_version, which has three
// meanings a *json.Number cannot tell apart:
//
//   - absent: keep the current pin (an update that does not mention it);
//   - null:   unpin, so the newest version runs;
//   - N:      pin version N.
//
// Before it existed an explicit null decoded as absent, so once a pin was set
// no request could remove it short of switching workflows.
type versionPin struct {
	set  bool
	null bool
	num  json.Number
}

func (p *versionPin) UnmarshalJSON(b []byte) error {
	p.set = true
	b = bytes.TrimSpace(b)
	if bytes.Equal(b, []byte("null")) {
		p.null = true
		return nil
	}
	if len(b) == 0 || b[0] == '"' {
		return errors.New("workflow_version must be an integer or null")
	}
	return json.Unmarshal(b, &p.num)
}

// apply returns the pin after this request. A request that changes the
// workflow without naming a version drops the old pin: a version number of one
// workflow means nothing for another.
func (p versionPin) apply(cur *int32, workflowChanged bool) (*int32, error) {
	switch {
	case p.set && p.null:
		return nil, nil
	case p.set:
		v, err := numToInt32(&p.num, 0, 1, 1<<30, "workflow_version")
		if err != nil {
			return nil, err
		}
		return &v, nil
	case workflowChanged:
		return nil, nil
	default:
		return cur, nil
	}
}
