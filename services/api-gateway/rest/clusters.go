// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package rest

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"

	"github.com/khuriwalgroup/kgf-nexus/internal/store"
	"github.com/khuriwalgroup/kgf-nexus/services/api-gateway/authn"
)

// registerClusterRoutes adds the cluster registry (docs/CLUSTER-SCALING.md).
//
// The gateway owns the registry -- operator intent -- and only READS what the
// in-cluster controller reported through the resource manager. Every route is
// guarded and mapped in routePermissions like the platform-config routes it
// sits beside: viewing needs infrastructure.view, any change
// infrastructure.configure.
func (a *API) registerClusterRoutes(mux *http.ServeMux) {
	mux.Handle("GET /v1/namespaces/{ns}/platform/clusters", a.guard(authn.ActionAdminWrite, a.clustersList))
	mux.Handle("PUT /v1/namespaces/{ns}/platform/clusters/{name}", a.guard(authn.ActionAdminWrite, a.clusterPut))
	mux.Handle("DELETE /v1/namespaces/{ns}/platform/clusters/{name}", a.guard(authn.ActionAdminWrite, a.clusterDelete))
}

func (a *API) clustersList(w http.ResponseWriter, r *http.Request, p authn.Principal) {
	views, err := store.GatewayClusterList(r.Context(), a.db, p.Namespace)
	if err != nil {
		a.fail(w, r, "list clusters", err)
		return
	}
	// Always a list: a namespace with no clusters is `[]`, never null. The
	// store promises the same; the wire contract is pinned here so it does not
	// depend on that promise holding.
	if views == nil {
		views = []store.ClusterView{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"clusters": views, "observed_at": a.clock().UTC()})
}

func (a *API) clusterPut(w http.ResponseWriter, r *http.Request, p authn.Principal) {
	body, err := readBody(r, 64<<10)
	if err != nil {
		writeBodyError(w, err)
		return
	}
	var spec store.ClusterSpec
	d := json.NewDecoder(bytes.NewReader(body))
	d.DisallowUnknownFields()
	if err = d.Decode(&spec); err == nil {
		var extra any
		if d.Decode(&extra) != io.EOF {
			err = errors.New("expected one JSON document")
		}
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_cluster", err.Error())
		return
	}
	if !revisionOK(w, spec.DesiredRevision) {
		return
	}
	// The path is authoritative for the name, so a body cannot rename a
	// different cluster than the URL the permission check was made against.
	if spec.Name != "" && spec.Name != r.PathValue("name") {
		writeError(w, http.StatusBadRequest, "invalid_cluster", "body name does not match the URL")
		return
	}
	spec.Name = r.PathValue("name")
	if spec.Bindings == nil {
		spec.Bindings = []store.ClusterBindingSpec{}
	}
	saved, err := store.GatewayClusterPut(r.Context(), a.db, p.Namespace, p.Name, spec,
		store.AuditEntry{ActorKind: string(p.Kind), ActorLabel: p.Name, ClientIP: clientIP(r)})
	switch {
	case errors.Is(err, store.ErrClusterInvalid):
		writeError(w, http.StatusBadRequest, "invalid_cluster", err.Error())
	case errors.Is(err, store.ErrClusterConflict):
		writeError(w, http.StatusConflict, "revision_conflict", err.Error())
	case err != nil:
		a.fail(w, r, "save cluster", err)
	default:
		writeJSON(w, http.StatusOK, saved)
	}
}

func (a *API) clusterDelete(w http.ResponseWriter, r *http.Request, p authn.Principal) {
	rev, err := strconv.ParseInt(r.URL.Query().Get("revision"), 10, 64)
	if err != nil || rev < 1 {
		// Required so a delete is made against the revision the operator saw,
		// never against one somebody changed in the meantime.
		writeError(w, http.StatusBadRequest, "invalid_request", "revision query parameter is required")
		return
	}
	err = store.GatewayClusterDelete(r.Context(), a.db, p.Namespace, r.PathValue("name"), rev,
		store.AuditEntry{ActorKind: string(p.Kind), ActorLabel: p.Name, ClientIP: clientIP(r)})
	switch {
	case errors.Is(err, store.ErrClusterMissing):
		writeError(w, http.StatusNotFound, "not_found", err.Error())
	case errors.Is(err, store.ErrClusterInvalid):
		writeError(w, http.StatusConflict, "cluster_applying", err.Error())
	case errors.Is(err, store.ErrClusterConflict):
		writeError(w, http.StatusConflict, "revision_conflict", err.Error())
	case err != nil:
		a.fail(w, r, "delete cluster", err)
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}
