import { StrictMode } from 'react';
import { createRoot } from 'react-dom/client';

import { App } from './App';

// Bootstrap and its icons are bundled with the console: nothing is fetched
// from elsewhere. The console's own stylesheet comes last and themes them.
import 'bootstrap/dist/css/bootstrap.min.css';
import 'bootstrap-icons/font/bootstrap-icons.min.css';
import './styles/console.css';

const container = document.getElementById('root');
if (container === null) {
  throw new Error('CactusFS console: #root element is missing from index.html');
}

createRoot(container).render(
  <StrictMode>
    <App />
  </StrictMode>,
);
