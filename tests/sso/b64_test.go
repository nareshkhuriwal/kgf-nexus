// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0
//go:build sso_e2e

package sso_test

import "encoding/base64"

func decodeB64(s string) (string, error) {
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return "", errNotB64
	}
	return string(b), nil
}

func encodeB64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }
