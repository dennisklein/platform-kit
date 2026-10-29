import { mount } from 'svelte';
import './app.css';
import App from './App.svelte';

// ?mock=1 serves the API from fixtures in the browser (dev and screenshots).
// The mock is a separate chunk that only loads with the flag.
async function boot() {
  if (new URLSearchParams(window.location.search).has('mock')) {
    const m = await import('./lib/api/mock');
    m.install();
  }
  mount(App, { target: document.getElementById('app')! });
}

void boot();
