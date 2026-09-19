import { useState } from 'react';
import { api } from '../backend.js';

// Dry-run suggestions with per-suggestion approve/reject checkboxes.
// TRUST RULE, enforced in the open: the engine cannot move files, so
// selection is UI-only state and the Apply button stays disabled with
// the reason printed next to it. Nothing here can rename anything —
// there is no binding for it to call.
export default function OrganizeView() {
  const [root, setRoot] = useState('');
  const [report, setReport] = useState(null);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState('');
  // approved maps currentPath -> true (approve) / false (reject).
  // Undefined means undecided. Pure React state, never sent anywhere.
  const [approved, setApproved] = useState({});

  async function preview() {
    setLoading(true);
    setError('');
    try {
      const r = await api.suggestOrganization(root || '.', 0.75, 180);
      setReport(r);
      setApproved({});
    } catch (e) {
      setError(String(e?.message ?? e));
    } finally {
      setLoading(false);
    }
  }

  function vote(path, value) {
    setApproved((prev) => ({ ...prev, [path]: value }));
  }

  const decided = Object.keys(approved).length;
  const approvedCount = Object.values(approved).filter(Boolean).length;

  return (
    <section>
      <div className="row">
        <input
          value={root}
          onChange={(e) => setRoot(e.target.value)}
          placeholder="~/Downloads (or another messy folder)"
          className="searchbox"
        />
        <button onClick={preview} disabled={loading}>
          {loading ? 'analyzing…' : 'preview'}
        </button>
      </div>
      {error && <p className="error">error: {error}</p>}
      {report && (
        <>
          <p className="muted">
            {report.total} files suggested to move across {report.folderCount}{' '}
            new folders · {approvedCount} approved, {decided - approvedCount}{' '}
            rejected
          </p>
          {report.groups.map((g) => (
            <article key={g.destination}>
              <h3>{g.destination}/</h3>
              <ul className="results">
                {g.suggestions.map((s) => (
                  <li key={s.currentPath} className="suggestion">
                    <div>
                      <code>{s.currentPath}</code> →{' '}
                      <code>{s.suggestedPath}</code>
                      <span className="muted">
                        {' '}
                        [{s.kind}] {s.reason}
                      </span>
                    </div>
                    <div className="row">
                      <button
                        className={approved[s.currentPath] === true ? 'sel' : ''}
                        onClick={() => vote(s.currentPath, true)}
                      >
                        approve
                      </button>
                      <button
                        className={approved[s.currentPath] === false ? 'sel' : ''}
                        onClick={() => vote(s.currentPath, false)}
                      >
                        reject
                      </button>
                    </div>
                  </li>
                ))}
              </ul>
            </article>
          ))}
          {report.unmoved?.length > 0 && (
            <>
              <h3>already unique</h3>
              <ul className="results">
                {report.unmoved.map((u) => (
                  <li key={u.currentPath} className="muted">
                    {u.currentPath} ({u.reason})
                  </li>
                ))}
              </ul>
            </>
          )}
          <button disabled title="moving lands in a future phase — the engine is dry-run only">
            apply (disabled — engine is dry-run only)
          </button>
        </>
      )}
    </section>
  );
}
