import logo from './assets/brand/cactusfs-logo.svg';

// Placeholder shell for the CactusFS console. The screens (buckets, object
// browser, users, groups, roles and policies, API keys, cluster status, audit
// log) arrive with the admin API in milestone 5. The console holds no business
// or authorization logic: it only renders what the admin API returns.
export function App() {
  return (
    <main>
      <img src={logo} alt="CactusFS" height={48} />
      <h1>CactusFS Console</h1>
      <p>The console is not available yet. It arrives with the management API.</p>
    </main>
  );
}
