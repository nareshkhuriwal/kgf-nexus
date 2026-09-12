// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package workergrpc

import (
	"context"
	"errors"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/khuriwalgroup/kgf-nexus/services/api-gateway/authn"
)

// AuthUnaryInterceptor authenticates every worker RPC.
//
// An interceptor rather than a check inside each handler: the worker protocol has
// seven RPCs and will grow, and one RPC that forgot to authenticate is a full
// bypass of the credential. Putting it in the chain makes "authenticated" a
// property of the transport rather than of each author's diligence.
func AuthUnaryInterceptor(auth *authn.Authenticator) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		md, ok := metadata.FromIncomingContext(ctx)
		if !ok {
			return nil, status.Error(codes.Unauthenticated, "missing metadata")
		}

		presented := firstNonEmpty(md.Get("authorization"), md.Get("x-api-key"))
		p, err := auth.Authenticate(ctx, authn.ExtractBearer(presented))
		if errors.Is(err, authn.ErrUnauthenticated) {
			// Opaque, with no key material and no hint about why. The error text
			// reaches an untrusted caller, and any detail here would help it probe
			// for valid keys.
			return nil, status.Error(codes.Unauthenticated, "invalid credentials")
		}
		if err != nil {
			// A backend failure is Unavailable, not Unauthenticated: a whole fleet
			// re-registering because the database blipped would be a self-inflicted
			// stampede.
			return nil, status.Error(codes.Unavailable, "authentication temporarily unavailable")
		}

		return handler(authn.WithPrincipal(ctx, p), req)
	}
}

func firstNonEmpty(groups ...[]string) string {
	for _, g := range groups {
		for _, v := range g {
			if v != "" {
				return v
			}
		}
	}
	return ""
}
