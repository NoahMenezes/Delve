import { useState } from 'react';
import { api, pickDirectory } from '../backend.js';

// Folder picker → ScanDirectory binding → result line. The promise
// stays pending for long scans, so the button shows an indeterminate
// state (per-path progress events are a follow-up, not this phase).
export default function ScanView() {
  const [root, setRoot] = useState('');
  const [extract, setExtract] = useState(true);
  const [embed, setEmbed] = useState(true);
  const [scanning, setScanning] = useState(false);
  const [result, setResult] = useState('');
  const [error, setError] = useState('');

  async function choose() {
    try {
      const dir = await pickDirectory();
      if (dir) setRoot(dir);
    } catch (e) {
      // No bridge dialog (plain browser): the typed path stays.
    }
  }

  async function scan() {
    setScanning(true);
    setResult('');
    setError('');
    try {
      const r = await api.scanDirectory(root || '.', extract, embed);
      setResult(`indexed ${r.indexed} files → ${r.database}`);
    } catch (e) {
      setError(String(e?.message ?? e));
    } finally {
      setScanning(false);
    }
  }

  return (
    <section>
      <div className="row">
        <input
          value={root}
          onChange={(e) => setRoot(e.target.value)}
          placeholder="~/Documents (or pick…)"
          className="searchbox"
        />
        <button onClick={choose}>pick…</button>
        <button onClick={scan} disabled={scanning}>
          {scanning ? 'scanning…' : 'scan'}
        </button>
      </div>
      <label>
        <input
          type="checkbox"
          checked={extract}
          onChange={(e) => setExtract(e.target.checked)}
        />{' '}
        extract content
      </label>{' '}
      <label>
        <input
          type="checkbox"
          checked={embed}
          onChange={(e) => setEmbed(e.target.checked)}
        />{' '}
        embed
      </label>
      {result && <p>{result}</p>}
      {error && <p className="error">error: {error}</p>}
    </section>
  );
}
