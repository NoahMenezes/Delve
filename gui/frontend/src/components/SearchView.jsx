import { useEffect, useRef, useState } from 'react';
import { api } from '../backend.js';

const LIMIT = 20;
const DEBOUNCE_MS = 350;

// Live search: results follow typing after a quiet window. The gen
// counter drops stale responses (a slow embedding call overtaken by
// newer keystrokes) — the TUI's debounce seq, as a React ref.
export default function SearchView() {
  const [query, setQuery] = useState('');
  const [semantic, setSemantic] = useState(false);
  const [results, setResults] = useState([]);
  const [selected, setSelected] = useState(null);
  const [searching, setSearching] = useState(false);
  const [error, setError] = useState('');
  const gen = useRef(0);

  useEffect(() => {
    if (!query.trim()) {
      setResults([]);
      setSelected(null);
      setSearching(false);
      setError('');
      return;
    }
    setSearching(true);
    const myGen = ++gen.current;
    const timer = setTimeout(() => {
      const call = semantic
        ? api.semanticSearch(query, LIMIT)
        : api.searchFiles(query, LIMIT);
      call.then(
        (rows) => {
          if (gen.current !== myGen) return; // superseded
          setResults(rows ?? []);
          setSelected(null);
          setSearching(false);
          setError('');
        },
        (err) => {
          if (gen.current !== myGen) return;
          setSearching(false);
          setError(String(err?.message ?? err));
        }
      );
    }, DEBOUNCE_MS);
    return () => clearTimeout(timer);
  }, [query, semantic]);

  return (
    <section>
      <div className="row">
        <input
          autoFocus
          value={query}
          onChange={(e) => setQuery(e.target.value)}
          placeholder="search files…"
          className="searchbox"
        />
        <button
          onClick={() => {
            setSemantic((s) => !s);
            setSelected(null);
          }}
          title="toggle keyword / semantic (like Tab in the terminal UI)"
        >
          {semantic ? 'semantic' : 'keyword'}
        </button>
      </div>
      {searching && <p className="muted">searching…</p>}
      {error && <p className="error">error: {error}</p>}
      {!searching && !error && query.trim() && results.length === 0 && (
        <p className="muted">
          {semantic
            ? 'no semantic results (need embedded files? scan with embedding on)'
            : 'no results'}
        </p>
      )}
      <ul className="results">
        {results.map((r) => (
          <li key={r.path}>
            <button
              className={selected?.path === r.path ? 'sel' : ''}
              onClick={() => setSelected(r)}
            >
              {r.score != null && (
                <span className="score">{r.score.toFixed(3)} </span>
              )}
              <strong>{r.name}</strong>
              <span className="muted"> {r.path}</span>
            </button>
          </li>
        ))}
      </ul>
      {selected && (
        <article className="detail">
          <h3>{selected.path}</h3>
          <p className="muted">
            {(selected.sizeBytes / 1024).toFixed(1)} KB · modified{' '}
            {new Date(selected.modifiedAt * 1000).toLocaleDateString()} ·{' '}
            {selected.extension || '(no ext)'}
            {selected.score != null && (
              <> · score {selected.score.toFixed(3)}</>
            )}
          </p>
          <p>{selected.snippet}</p>
        </article>
      )}
    </section>
  );
}
