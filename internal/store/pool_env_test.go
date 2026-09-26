// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestApplyPoolEnv(t *testing.T) {
	base := func() *pgxpool.Config {
		pc, err := pgxpool.ParseConfig("postgres://u@localhost/db")
		if err != nil {
			t.Fatal(err)
		}
		pc.MaxConns, pc.MinConns = 16, 2
		return pc
	}

	t.Run("unset keeps the service default", func(t *testing.T) {
		t.Setenv(EnvDBMaxConns, "")
		t.Setenv(EnvDBMinConns, "")
		pc := base()
		if err := ApplyPoolEnv(pc); err != nil || pc.MaxConns != 16 || pc.MinConns != 2 {
			t.Fatalf("got max=%d min=%d err=%v", pc.MaxConns, pc.MinConns, err)
		}
	})
	t.Run("overrides both", func(t *testing.T) {
		t.Setenv(EnvDBMaxConns, "4")
		t.Setenv(EnvDBMinConns, "1")
		pc := base()
		if err := ApplyPoolEnv(pc); err != nil || pc.MaxConns != 4 || pc.MinConns != 1 {
			t.Fatalf("got max=%d min=%d err=%v", pc.MaxConns, pc.MinConns, err)
		}
	})
	t.Run("min is clamped to max", func(t *testing.T) {
		t.Setenv(EnvDBMaxConns, "3")
		t.Setenv(EnvDBMinConns, "")
		pc := base()
		pc.MinConns = 8
		if err := ApplyPoolEnv(pc); err != nil || pc.MinConns != 3 {
			t.Fatalf("got min=%d err=%v", pc.MinConns, err)
		}
	})
	for _, bad := range []string{"0", "-2", "four", "99999999999"} {
		t.Run("rejects "+bad, func(t *testing.T) {
			t.Setenv(EnvDBMaxConns, bad)
			t.Setenv(EnvDBMinConns, "")
			if err := ApplyPoolEnv(base()); err == nil {
				t.Fatalf("%q accepted", bad)
			}
		})
	}
}
