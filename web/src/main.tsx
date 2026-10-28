import { StrictMode } from 'react';
import { createRoot } from 'react-dom/client';
import './ds/themes/hud.css';
import './ds/base.css';
import { App } from './app/App';
import { boot } from './app/boot';
import { registerCoreCommands } from './app/commands/core';
import { applyPrefs } from './app/prefs';
import { registerServiceWorker } from './app/pwa/push';

applyPrefs();
registerCoreCommands();
void boot();
void registerServiceWorker();

createRoot(document.getElementById('root')!).render(
  <StrictMode>
    <App />
  </StrictMode>,
);
