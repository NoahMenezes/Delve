// Bridge to the Go backend. Wails injects bound methods at
// window.go.main.App.* when running inside the desktop shell — no
// generated wailsjs import needed, so `npm run build` works without
// ever launching Wails. Outside the shell (plain browser) every call
// rejects with a clear message instead of crashing on undefined.
//
// Every function returns a promise: resolve with the DTO, reject with
// the Go error string. Stale responses (a slow semantic search
// overtaken by newer typing) are the caller's job — see SearchView's
// generation counter, the same idea as the TUI's debounce seq.
function app() {
  const a = window.go?.main?.App;
  if (!a) {
    return null;
  }
  return a;
}

function call(method, ...args) {
  const a = app();
  if (!a) {
    return Promise.reject(new Error('not running inside Delve desktop (no Go bridge)'));
  }
  return a[method](...args);
}

// Folder picker via the Wails runtime bridge (also injected). Falls
// back to null so callers keep the typed path when the bridge or
// dialog is unavailable.
export function pickDirectory() {
  const rt = window.runtime;
  if (!rt?.BrowserOpenDirectory) {
    return Promise.resolve(null);
  }
  return rt.BrowserOpenDirectory({ Title: 'Choose a folder to index' });
}

export const api = {
  scanDirectory: (root, extractContent, embed) =>
    call('ScanDirectory', root, extractContent, embed),
  searchFiles: (query, limit) => call('SearchFiles', query, limit),
  semanticSearch: (query, limit) => call('SemanticSearch', query, limit),
  suggestOrganization: (root, threshold, staleDays) =>
    call('SuggestOrganization', root, threshold, staleDays),
};
