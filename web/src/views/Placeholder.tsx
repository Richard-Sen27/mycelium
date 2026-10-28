import { EmptyState, Frame } from '../ds';
import { NAV, type RouteId } from '../app/nav';

/**
 * RoutePlaceholder stands in for a view another work package delivers.
 * Replace the route module (src/views/<id>/index.tsx) with the real view;
 * nothing else in the shell needs to change.
 */
export function RoutePlaceholder({ id, pkg, children }: { id: RouteId; pkg: string; children?: React.ReactNode }) {
  const i = NAV.findIndex((n) => n.id === id);
  const label = NAV[i]?.label ?? id;
  return (
    <Frame title={label} index={String(i + 1).padStart(2, '0')}>
      <EmptyState title={`${label} is not built yet`}>{children ?? `This view arrives with ${pkg}.`}</EmptyState>
    </Frame>
  );
}
