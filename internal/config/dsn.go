// Package config resolves configuration that every control-plane service needs
// to agree on.
package config

import "os"

// dsnEnvNames are the accepted spellings for the Postgres DSN, in precedence
// order.
//
// Six services were built in parallel and each chose its own name for the same
// value: NEXUS_DATABASE_URL, NEXUS_POSTGRES_DSN, and DATABASE_URL all appeared.
// Rather than renaming five services and breaking anyone's existing deployment
// or compose file, every service accepts all three. An operator should not have
// to remember which service wants which spelling -- that is exactly the kind of
// incidental knowledge that makes a platform unpleasant to run, and the kind of
// mismatch that fails at container start with a confusing "connection refused
// to localhost" rather than a clear configuration error.
var dsnEnvNames = []string{
	"NEXUS_DATABASE_URL",
	"DATABASE_URL",
	"NEXUS_POSTGRES_DSN",
	"NEXUS_DSN",
}

// DSN returns the first Postgres DSN found among the accepted environment
// variable names, falling back to def when none is set.
func DSN(def string) string {
	for _, name := range dsnEnvNames {
		if v := os.Getenv(name); v != "" {
			return v
		}
	}
	return def
}

// AddrEnvNames maps a logical listener to its accepted environment spellings,
// for the same reason as DSN: the services disagreed on naming.
func Addr(def string, names ...string) string {
	for _, name := range names {
		if v := os.Getenv(name); v != "" {
			return v
		}
	}
	return def
}
