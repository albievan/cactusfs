import { cleanup, render, screen } from '@testing-library/react';
import { afterEach, describe, expect, it } from 'vitest';

import { App } from './App';

afterEach(cleanup);

describe('App', () => {
  it('renders the console heading', () => {
    render(<App />);

    expect(screen.getByRole('heading', { level: 1 }).textContent).toBe('CactusFS Console');
  });

  it('shows the CactusFS logo from the brand assets', () => {
    render(<App />);

    const logo = screen.getByAltText('CactusFS');
    expect(logo.getAttribute('src')).toContain('cactusfs-logo.svg');
  });

  it('says that the console is not available yet', () => {
    render(<App />);

    expect(screen.getByText(/not available yet/i)).toBeDefined();
  });
});
