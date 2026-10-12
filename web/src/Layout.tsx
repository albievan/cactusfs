import { useState, type ReactNode } from 'react';

import type { Identity } from './api/client';
import logo from './assets/brand/cactusfs-logo-reversed.svg';
import mark from './assets/brand/cactusfs-mark.svg';
import { routeHref, useRoute } from './router';

const SECTIONS = [
  { segment: 'buckets', label: 'Buckets', icon: 'bi-bucket' },
  { segment: 'users', label: 'Users', icon: 'bi-person' },
  { segment: 'groups', label: 'Groups', icon: 'bi-people' },
  { segment: 'roles', label: 'Roles', icon: 'bi-person-badge' },
  { segment: 'policies', label: 'Policies', icon: 'bi-shield-lock' },
  { segment: 'cluster', label: 'Cluster', icon: 'bi-hdd-network' },
  { segment: 'audit', label: 'Audit log', icon: 'bi-journal-text' },
];

const FOLDED_KEY = 'cactusfs.nav.folded';

function readFolded(): boolean {
  try {
    return window.localStorage.getItem(FOLDED_KEY) === '1';
  } catch {
    return false;
  }
}

interface LayoutProps {
  /** Who is signed in, as the API reported it. */
  identity: Identity;
  /** Ends the session. */
  onSignOut: () => void;
  /** The screen. */
  children: ReactNode;
}

/**
 * The frame around every screen: the navigation down the left side - the
 * logo, the sections, who is signed in - and the screen beside it. The
 * navigation folds to its icons, and remembers that it was folded.
 */
export function Layout({ identity, onSignOut, children }: LayoutProps) {
  const route = useRoute();
  const active = route.segments[0] ?? 'buckets';
  const [folded, setFolded] = useState(readFolded);

  const fold = () => {
    setFolded((was) => {
      try {
        window.localStorage.setItem(FOLDED_KEY, was ? '0' : '1');
      } catch {
        // Not remembered, then.
      }
      return !was;
    });
  };

  return (
    <div className={folded ? 'layout layout-folded' : 'layout'}>
      <header className="sidebar">
        <a className="sidebar-brand" href={routeHref(['buckets'])}>
          <img className="sidebar-logo" src={folded ? mark : logo} alt="CactusFS" />
        </a>
        <nav className="sections" aria-label="Sections">
          <ul className="nav nav-pills flex-column">
            {SECTIONS.map((section) => (
              <li className="nav-item" key={section.segment}>
                <a
                  className={section.segment === active ? 'nav-link active' : 'nav-link'}
                  href={routeHref([section.segment])}
                  aria-current={section.segment === active ? 'page' : undefined}
                  title={folded ? section.label : undefined}
                >
                  <i className={`bi ${section.icon}`} aria-hidden="true" />
                  <span className="nav-label">{section.label}</span>
                </a>
              </li>
            ))}
          </ul>
        </nav>
        <div className="sidebar-session">
          <span className="sidebar-user">
            <i className="bi bi-person-circle" aria-hidden="true" />
            <span className="nav-label">Signed in as {identity.user ?? identity.kind}</span>
          </span>
          <button type="button" className="btn btn-outline-light btn-sm sidebar-button" onClick={onSignOut} title="End this session">
            <i className="bi bi-box-arrow-right" aria-hidden="true" />
            <span className="nav-label">Sign out</span>
          </button>
          <button
            type="button"
            className="btn btn-outline-light btn-sm sidebar-button sidebar-fold"
            onClick={fold}
            aria-pressed={folded}
            aria-label="Show icons only"
            title={folded ? 'Show the names of the sections' : 'Fold the navigation to icons'}
          >
            <i className={folded ? 'bi bi-chevron-double-right' : 'bi bi-chevron-double-left'} aria-hidden="true" />
            <span className="nav-label" aria-hidden="true">
              Fold
            </span>
          </button>
        </div>
      </header>
      <main className="screen">{children}</main>
    </div>
  );
}
