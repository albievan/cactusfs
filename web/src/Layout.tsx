import type { ReactNode } from 'react';

import type { Identity } from './api/client';
import logo from './assets/brand/cactusfs-logo-reversed.svg';
import { routeHref, useRoute } from './router';

const SECTIONS = [
  { segment: 'buckets', label: 'Buckets' },
  { segment: 'users', label: 'Users' },
  { segment: 'groups', label: 'Groups' },
  { segment: 'roles', label: 'Roles' },
  { segment: 'policies', label: 'Policies' },
  { segment: 'cluster', label: 'Cluster' },
  { segment: 'audit', label: 'Audit log' },
];

interface LayoutProps {
  /** Who is signed in, as the API reported it. */
  identity: Identity;
  /** Ends the session. */
  onSignOut: () => void;
  /** The screen. */
  children: ReactNode;
}

/** The frame around every screen: the logo, who is signed in, and the sections. */
export function Layout({ identity, onSignOut, children }: LayoutProps) {
  const route = useRoute();
  const active = route.segments[0] ?? 'buckets';

  return (
    <div className="layout">
      <header className="topbar">
        <a className="topbar-brand" href={routeHref(['buckets'])}>
          <img className="topbar-logo" src={logo} alt="CactusFS" />
        </a>
        <div className="topbar-session">
          <span className="topbar-user">Signed in as {identity.user ?? identity.kind}</span>
          <button type="button" className="button button-on-dark" onClick={onSignOut}>
            Sign out
          </button>
        </div>
      </header>
      <nav className="sections" aria-label="Sections">
        <ul>
          {SECTIONS.map((section) => (
            <li key={section.segment}>
              <a
                href={routeHref([section.segment])}
                aria-current={section.segment === active ? 'page' : undefined}
              >
                {section.label}
              </a>
            </li>
          ))}
        </ul>
      </nav>
      <main className="screen">{children}</main>
    </div>
  );
}
