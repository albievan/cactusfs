import { useEffect, useRef, useState, type FormEvent, type ReactNode } from 'react';

import type { Identity, ServerVersion } from './api/client';
import logo from './assets/brand/cactusfs-logo-reversed.svg';
import mark from './assets/brand/cactusfs-mark.svg';
import { navigate, routeHref, useRoute } from './router';

const SECTIONS = [
  { segment: 'buckets', label: 'Buckets', icon: 'bi-bucket' },
  { segment: 'users', label: 'Users', icon: 'bi-person' },
  { segment: 'groups', label: 'Groups', icon: 'bi-people' },
  { segment: 'roles', label: 'Roles', icon: 'bi-person-badge' },
  { segment: 'policies', label: 'Policies', icon: 'bi-shield-lock' },
  { segment: 'cluster', label: 'Cluster', icon: 'bi-hdd-network' },
  { segment: 'settings', label: 'Settings', icon: 'bi-gear' },
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

function rememberFolded(folded: boolean): void {
  try {
    window.localStorage.setItem(FOLDED_KEY, folded ? '1' : '0');
  } catch {
    // Not remembered, then.
  }
}

/**
 * Which build the console talks to, as the line under the logo says it and
 * as its hint does. A build "0" was not numbered: a local build.
 */
export function describeBuild(server: ServerVersion): { line: string; hint: string } {
  const build = server.build === '0' || server.build === '' ? 'local build' : `build ${server.build}`;
  const commit = server.commit === undefined || server.commit === '' ? '' : `, commit ${server.commit}`;
  return {
    line: `v${server.version} \u00b7 ${build}`,
    hint: `Version ${server.version}, ${build}${commit}`,
  };
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
 * logo, under it the version and the build of what answers, the search
 * field, the sections, who is signed in - and the screen beside it. The
 * navigation folds to its icons, and remembers that it was folded; the
 * search field is then a magnifier that unfolds it, and the version line is
 * not shown. On a phone the line is beside the logo.
 */
export function Layout({ identity, onSignOut, children }: LayoutProps) {
  const route = useRoute();
  const active = route.segments[0] ?? 'buckets';
  const [folded, setFolded] = useState(readFolded);
  // What is typed into the search field; on the search screen, what was
  // searched for.
  const searched = route.segments[0] === 'search' ? (route.query.get('q') ?? '') : null;
  const [sought, setSought] = useState(searched ?? '');
  // The field follows the address: when the search that is on screen
  // changes - by the form of the screen, a link, the back button - it says
  // that search. Elsewhere it keeps what was typed.
  const [followed, setFollowed] = useState(searched);
  if (searched !== followed) {
    setFollowed(searched);
    if (searched !== null) {
      setSought(searched);
    }
  }
  const searchField = useRef<HTMLInputElement>(null);
  const focusSearch = useRef(false);
  // A node of the release before this line was added answers without
  // `server`, and during a rolling upgrade it may be the one that answers:
  // then there is no line, and nothing else is the worse for it.
  const server: ServerVersion | undefined = identity.server;
  const build = server === undefined ? null : describeBuild(server);

  const fold = () => {
    rememberFolded(!folded);
    setFolded(!folded);
  };

  // The magnifier of a folded navigation: unfold it, and put the cursor in
  // the field once it is there to take it.
  const openSearch = () => {
    focusSearch.current = true;
    rememberFolded(false);
    setFolded(false);
  };

  useEffect(() => {
    if (!folded && focusSearch.current) {
      focusSearch.current = false;
      searchField.current?.focus();
    }
  }, [folded]);

  const search = (event: FormEvent) => {
    event.preventDefault();
    const q = sought.trim();
    if (q !== '') {
      navigate(routeHref(['search'], { q }));
    }
  };

  return (
    <div className={folded ? 'layout layout-folded' : 'layout'}>
      <header className="sidebar">
        <div className="sidebar-head">
          <a className="sidebar-brand" href={routeHref(['buckets'])}>
            <img className="sidebar-logo" src={folded ? mark : logo} alt="CactusFS" />
          </a>
          {build !== null && (
            <p className="sidebar-version" title={build.hint}>
              {build.line}
            </p>
          )}
        </div>
        <form className="sidebar-search" role="search" aria-label="Search" onSubmit={search}>
          <label className="visually-hidden" htmlFor="sidebar-search-field">
            Search buckets and objects
          </label>
          <input
            ref={searchField}
            id="sidebar-search-field"
            className="form-control form-control-sm"
            type="search"
            name="q"
            placeholder="Search"
            maxLength={256}
            autoCapitalize="none"
            autoComplete="off"
            spellCheck={false}
            value={sought}
            onChange={(event) => {
              setSought(event.target.value);
            }}
          />
          <button
            type="submit"
            className="btn btn-outline-light btn-sm sidebar-button"
            title="Search buckets and objects: text, wild*card? or rx:regular expression"
          >
            <i className="bi bi-search" aria-hidden="true" />
            <span className="visually-hidden">Search</span>
          </button>
        </form>
        <button
          type="button"
          className="btn btn-outline-light btn-sm sidebar-button sidebar-search-open"
          onClick={openSearch}
          aria-label="Show the search field"
          title="Search buckets and objects"
        >
          <i className="bi bi-search" aria-hidden="true" />
        </button>
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
