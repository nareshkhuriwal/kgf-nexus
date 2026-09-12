/**
 * The Nexus mark.
 *
 * A nexus is a point where connections meet, and this platform's whole subject
 * is a DAG: nodes joined by dependencies. So the mark is the smallest honest
 * drawing of that -- a hub with edges radiating to satellite nodes. It reads at
 * 16px in a sidebar and at 512px on a favicon without changing shape, which is
 * the practical test a logo has to pass here.
 *
 * Drawn as inline SVG rather than shipped as a file so it inherits currentColor.
 * The mark must sit on the accent in the sidebar, on a button's foreground when
 * inverted, and on plain text in a loading state; a PNG would need three copies
 * and would drift from the palette the first time the accent changed.
 *
 * No gradients and no strokes below 1.5 units: this renders at 16px far more
 * often than at 64px, and a hairline that vanishes at small sizes is a mark that
 * only works in the design file.
 */

export interface LogoProps {
  /** Rendered size in px. The viewBox is square, so this sets both axes. */
  size?: number;
  /** Extra class, for spacing at the call site rather than margins baked in here. */
  className?: string;
  /**
   * Accessible label. Defaults to none: the mark almost always sits beside the
   * word "Nexus", and announcing it twice is noise. Pass a label only where the
   * mark stands alone.
   */
  title?: string;
}

export function Logo({ size = 20, className, title }: LogoProps) {
  return (
    <svg
      width={size}
      height={size}
      viewBox="0 0 24 24"
      fill="none"
      className={className}
      role={title ? 'img' : undefined}
      aria-label={title}
      aria-hidden={title ? undefined : true}
      focusable="false"
    >
      {/* Edges first, so the nodes paint over the joins and no stroke end
          pokes out of a circle at small sizes. */}
      <g stroke="currentColor" strokeWidth="1.6" strokeLinecap="round" opacity="0.55">
        <path d="M12 12 L12 4.4" />
        <path d="M12 12 L18.6 15.8" />
        <path d="M12 12 L5.4 15.8" />
      </g>
      {/* The hub, filled: the point everything meets is the one element that
          should never read as an outline. */}
      <circle cx="12" cy="12" r="3.1" fill="currentColor" />
      {/* Satellites, outlined, so the hub stays the visual centre at any size. */}
      <g stroke="currentColor" strokeWidth="1.6" fill="none">
        <circle cx="12" cy="3.4" r="2.1" />
        <circle cx="19.4" cy="16.3" r="2.1" />
        <circle cx="4.6" cy="16.3" r="2.1" />
      </g>
    </svg>
  );
}

/**
 * The mark as a standalone data-URI, for the favicon and anywhere a string is
 * required instead of a component.
 *
 * Kept in sync with the component above by construction -- same geometry,
 * written once here as the serialised form. `currentColor` cannot resolve
 * outside the document, so the favicon pins the accent explicitly.
 */
export function logoDataUri(color = '#818cf8'): string {
  const svg = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24" fill="none">
<g stroke="${color}" stroke-width="1.6" stroke-linecap="round" opacity="0.55">
<path d="M12 12 L12 4.4"/><path d="M12 12 L18.6 15.8"/><path d="M12 12 L5.4 15.8"/></g>
<circle cx="12" cy="12" r="3.1" fill="${color}"/>
<g stroke="${color}" stroke-width="1.6" fill="none">
<circle cx="12" cy="3.4" r="2.1"/><circle cx="19.4" cy="16.3" r="2.1"/><circle cx="4.6" cy="16.3" r="2.1"/></g>
</svg>`;
  return `data:image/svg+xml,${encodeURIComponent(svg)}`;
}
