import { useState } from 'react';
import SearchView from './components/SearchView.jsx';
import OrganizeView from './components/OrganizeView.jsx';
import ScanView from './components/ScanView.jsx';

const TABS = [
  ['search', 'Search'],
  ['organize', 'Organize'],
  ['scan', 'Scan'],
];

export default function App() {
  const [tab, setTab] = useState('search');
  return (
    <main>
      <header>
        <h1>delve</h1>
        <nav>
          {TABS.map(([id, label]) => (
            <button
              key={id}
              className={tab === id ? 'sel' : ''}
              onClick={() => setTab(id)}
            >
              {label}
            </button>
          ))}
        </nav>
      </header>
      {tab === 'search' && <SearchView />}
      {tab === 'organize' && <OrganizeView />}
      {tab === 'scan' && <ScanView />}
      <footer className="muted">
        local-first · offline · nothing moves without your approval
      </footer>
    </main>
  );
}
