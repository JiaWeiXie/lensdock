import { StrictMode } from 'react';
import { createRoot } from 'react-dom/client';
import './fonts';
import './styles/tokens.css';
import './styles/base.css';
import './styles/app.css';
import './styles/editor.css';
import { App } from './App';

const container = document.getElementById('root');
if (!container) throw new Error('missing #root');
createRoot(container).render(
  <StrictMode>
    <App />
  </StrictMode>,
);
