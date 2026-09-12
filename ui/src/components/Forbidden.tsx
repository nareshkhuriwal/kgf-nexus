/**
 * The permission-denied state, as a first-class screen rather than an error box.
 *
 * A 403 is not a failure the operator can retry their way out of — it is an
 * answer. Rendering it as a red "request failed" panel with a Retry button
 * invites them to click it repeatedly, so this says what is missing and what
 * would fix it instead.
 */

export function Forbidden({ what, detail }: { what: string; detail?: string }) {
  return (
    <div className="notice" role="status">
      <span className="notice-glyph" aria-hidden="true">
        🔒
      </span>
      <div>
        <div className="notice-title">Not permitted: {what}</div>
        <div className="notice-detail">
          {detail ?? 'Your credential does not carry the permission this screen needs.'}
        </div>
        <div className="notice-hint">
          Sign in with an admin account, or present an admin API key. A scoped token only
          carries the actions it was created with.
        </div>
      </div>
    </div>
  );
}
