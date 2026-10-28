import { ArrowDownToLine, CircleAlert, FileText, Link2, LoaderCircle, LogOut, Play, Square, Store } from 'lucide-react';
import { useConfirmation } from './Confirmation';
import type { AppSnapshot, CatalogProduct, EAAppStatus } from '../shared/types';

export const isEAPlay = (product: Pick<CatalogProduct, 'distributor'>) => product.distributor === 'ea-app';

export const maximaReady = (data: AppSnapshot) => Boolean(data.maxima?.available && !data.maxima.buildRequired && data.maxima.signedIn);

// EA games install with Maxima into the Nexus queue. The EA app is only the
// fallback when Maxima is not set up and the EA app is signed in.
export const eaInstallThroughApp = (data: AppSnapshot) => !maximaReady(data) && Boolean(data.eaApp?.installed && data.eaApp.signedIn);

// queued is false when nothing was queued: the EA app took over, or an
// existing EA installation was added to the library.
export async function installEAGame(data: AppSnapshot, product: CatalogProduct, destination: string): Promise<{ message: string; queued: boolean }> {
  if (eaInstallThroughApp(data)) return { message: await openInEAApp(product), queued: false };
  const id = await window.launcher.startEAInstall({ productId: product.productId, title: product.title, destination });
  return id
    ? { message: `${product.title} added to Install Queue. Maxima installs it.`, queued: true }
    : { message: `${product.title} is already installed for your EA account and was added to the library.`, queued: false };
}

// Store metadata carries no EA offer ID, so the EA app opens on its home page
// with the title on the clipboard for its search.
export async function openInEAApp(product: CatalogProduct): Promise<string> {
  await window.launcher.openEAAppFor(product.title);
  return `EA app opened. Search for “${product.title}” there (copied to the clipboard) and install it.`;
}

type Invoke = (name: string, fn: () => Promise<unknown>, success?: string) => Promise<void>;

// EA Play titles install through the EA app, kept in its own Proton prefix.
// Sign-in and Xbox linking happen in EA's own windows; Nexus sees no EA credentials.
export function EAAppCard({ status, busy, invoke }: { status?: EAAppStatus; busy?: string; invoke: Invoke }) {
  const confirm = useConfirmation();
  const working = Boolean(busy) || Boolean(status?.busy);
  const label = status?.running ? 'RUNNING' : status?.installed ? 'INSTALLED' : 'NOT INSTALLED';
  const tone = status?.running || status?.installed ? 'ready' : 'off';
  return <section className="settings-card ea-app-card">
    <div className="settings-heading"><div className="settings-icon"><Store /></div><div><span className="overline">EA PLAY</span><h2>EA app</h2><p>{status?.proton ? `Runs in its own Proton prefix with ${status.proton}` : 'No Proton build found. Install GE-Proton or Proton Experimental through Steam.'}{status?.installed ? status.signedIn ? ' · Signed in' : ' · Not signed in — sign in inside the EA app' : ''}</p></div><span className={`status-label ${tone}`}><span className={`status-dot ${tone === 'ready' ? 'green' : 'red'}`} />{label}</span></div>
    <ol className="ea-app-steps">
      <li>Install the EA app. EA Play games such as Dead Space 2 have no Microsoft Store package; the EA app downloads them and checks their license.</li>
      <li>In the EA app, choose <strong>Sign in with Xbox</strong> and use the same Microsoft account as Xodus.</li>
      <li>Link the Xbox account to your EA account once. With Game Pass Ultimate, EA Play then turns on automatically.</li>
      <li>Install and play EA Play games from the EA app.</li>
    </ol>
    {status?.error ? <div className="alert amber"><CircleAlert /><span>{status.error}</span></div> : null}
    {status?.phase ? <div className="runtime-phase"><LoaderCircle className={status.busy ? 'spin' : ''} /><span>{status.phase}</span></div> : null}
    <div className="button-row">
      {!status?.installed ? <button className="primary" disabled={working || !status?.proton} onClick={() => void invoke('ea-install', () => window.launcher.installEAApp())}>{busy === 'ea-install' || status?.busy ? <LoaderCircle className="spin" /> : <ArrowDownToLine />}Install EA app</button> : null}
      {status?.installed && !status.running ? <button className="primary" disabled={working} onClick={() => void invoke('ea-launch', () => window.launcher.launchEAApp())}>{busy === 'ea-launch' ? <LoaderCircle className="spin" /> : <Play />}Open EA app</button> : null}
      {status?.running ? <button className="secondary" disabled={Boolean(busy)} title="Closes the EA app and every game running in its prefix." onClick={() => void invoke('ea-stop', () => window.launcher.stopEAApp(), 'The EA app was stopped.')}><Square />Stop EA app</button> : null}
      {status?.installed ? <button className="secondary" disabled={working} title="Closes the EA app and removes its saved sign-in. Installed games are kept." onClick={async () => { if (await confirm({ title: 'Sign out of the EA app?', message: 'The EA app and any game running in it will close. Installed games are kept; the next start asks for an account.', confirmLabel: 'Sign out' })) void invoke('ea-signout', () => window.launcher.signOutEAApp(), 'Signed out of the EA app.'); }}><LogOut />Sign out</button> : null}
      <button className="secondary" disabled={Boolean(busy)} title="Opens EA account connections in the browser. Connect Xbox there if the EA app does not offer it." onClick={() => void invoke('ea-link', () => window.launcher.openEAAccountLinking())}><Link2 />Link Xbox account</button>
      {status?.logPath ? <button className="secondary" disabled={Boolean(busy)} onClick={() => void invoke('ea-log', () => window.launcher.openPath(status.logPath!))}><FileText />EA app log</button> : null}
    </div>
  </section>;
}
