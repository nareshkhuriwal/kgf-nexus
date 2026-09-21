package rest

import (
	"errors"
	"github.com/jackc/pgx/v5"
	"github.com/khuriwalgroup/kgf-nexus/internal/store"
	"github.com/khuriwalgroup/kgf-nexus/services/api-gateway/authn"
	"net/http"
)

// deleteRun removes terminal runs from listings while preserving execution and deduplication records.
func (a *API) deleteRun(w http.ResponseWriter, r *http.Request, p authn.Principal) {
	tx, err := a.db.Begin(r.Context())
	if err != nil {
		a.fail(w, r, "delete run", err)
		return
	}
	defer tx.Rollback(r.Context())
	var state string
	err = tx.QueryRow(r.Context(), `SELECT state::text FROM workflow_runs WHERE id::text=$1 AND namespace=$2`, r.PathValue("id"), p.Namespace).Scan(&state)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, 404, "not_found", "Run not found")
		return
	}
	if err != nil {
		a.fail(w, r, "delete run", err)
		return
	}
	if state != "SUCCEEDED" && state != "FAILED" && state != "CANCELLED" && state != "TIMED_OUT" {
		writeError(w, 409, "run_active", "Cancel or finish the run before deleting it")
		return
	}
	tag, err := tx.Exec(r.Context(), `INSERT INTO run_deletions(run_id,namespace) SELECT id,namespace FROM workflow_runs WHERE id::text=$1 AND namespace=$2 AND state IN ('SUCCEEDED','FAILED','CANCELLED','TIMED_OUT') ON CONFLICT DO NOTHING`, r.PathValue("id"), p.Namespace)
	if err == nil && tag.RowsAffected() == 0 {
		var deleted bool
		err = tx.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM run_deletions WHERE run_id::text=$1 AND namespace=$2)`, r.PathValue("id"), p.Namespace).Scan(&deleted)
		if err == nil && !deleted {
			writeError(w, 409, "run_active", "Run state changed; refresh before deleting")
			return
		}
	}
	if err == nil && tag.RowsAffected() > 0 {
		err = store.GatewayAppendAudit(r.Context(), tx, store.AuditEntry{Namespace: p.Namespace, ActorKind: string(p.Kind), ActorLabel: p.Name, Action: "run.delete", TargetKind: "run", TargetID: r.PathValue("id"), Outcome: "success", ClientIP: clientIP(r)})
	}
	if err != nil {
		a.fail(w, r, "delete run", err)
		return
	}
	if err = tx.Commit(r.Context()); err != nil {
		a.fail(w, r, "delete run", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
