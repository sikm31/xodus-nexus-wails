import { useEffect, useState } from 'react';
import { CircleAlert, Hammer, Library, LoaderCircle, LogIn, RefreshCw } from 'lucide-react';
import type { EATitle, MaximaStatus } from '../shared/types';

type Invoke = (name: string, fn: () => Promise<unknown>, success?: string) => Promise<void>;

const short = (commit?: string) => commit?.slice(0, 10) ?? '—';

// Maxima (GPL-3.0, ArmchairDevelopers) installs and launches EA games without
// the EA app window. Nexus builds the pinned commit and runs it as a separate program.
export function MaximaCard({ status, busy, invoke }: { status?: MaximaStatus; busy?: string; invoke: Invoke }) {
  const [logs, setLogs] = useState<string[]>([]);
  const [library, setLibrary] = useState<EATitle[]>();
  useEffect(() => window.launcher.onOperationLog(({ scope, line }) => {
    if (scope === 'maxima') setLogs(items => [...items.slice(-199), line]);
  }), []);
  const working = Boolean(busy) || Boolean(status?.building || status?.signingIn);
  const ready = Boolean(status?.available && !status.buildRequired);
  const label = status?.building ? 'BUILDING' : !status?.available ? 'NOT BUILT' : status.buildRequired ? 'UPDATE REQUIRED' : 'READY';
  const newerUpstream = status?.upstreamCommit && status.upstreamCommit !== status.pinnedCommit;
  return <section className="settings-card maxima-card">
    <div className="settings-heading"><div className="settings-icon"><Library /></div><div><span className="overline">EA GAMES</span><h2>Maxima</h2><p>{status?.available ? `Active build ${short(status.commit)} · expected ${short(status.pinnedCommit)}` : `Build Maxima ${short(status?.pinnedCommit)} to install EA and EA Play games in Nexus.`}</p></div><span className={`status-label ${ready ? 'ready' : 'off'}`}><span className={`status-dot ${ready ? 'green' : status?.available ? 'amber' : 'red'}`} />{label}</span></div>
    <p className="fine-print">Installs EA games into the Nexus download queue, without the EA app window. <a href="https://github.com/ArmchairDevelopers/Maxima" target="_blank" rel="noreferrer">Maxima</a> is open-source software by ArmchairDevelopers under GPL-3.0, run as a separate program.</p>
    {status?.error ? <div className="alert amber"><CircleAlert /><span>{status.error}</span></div> : null}
    {status?.phase ? <div className="runtime-phase"><LoaderCircle className="spin" /><span>{status.phase}</span></div> : null}
    {!status?.canBuild ? <div className="alert amber"><CircleAlert /><span>Building Maxima needs Git and Cargo: sudo dnf install cargo rust</span></div> : null}
    {newerUpstream ? <p className="fine-print">Newer Maxima upstream: {short(status.upstreamCommit)}. Nexus builds the tested commit; newer ones arrive with launcher updates.</p> : null}
    <div className="meta-grid"><div><span>EA ACCOUNT</span><strong>{status?.signedIn ? status.account ?? 'Signed in' : 'Not signed in'}</strong></div><div><span>PINNED</span><strong className="mono">{short(status?.pinnedCommit)}</strong></div></div>
    <div className="button-row">
      {!ready ? <button className="primary" disabled={working || !status?.canBuild} onClick={() => void invoke('maxima-build', () => window.launcher.buildMaxima(), 'Maxima was built and activated.')}>{status?.building ? <LoaderCircle className="spin" /> : <Hammer />}{status?.available ? 'Update Maxima' : 'Build Maxima'}</button> : null}
      <button className="secondary" disabled={working} onClick={() => void invoke('maxima-check', () => window.launcher.checkMaximaUpdate())}>{busy === 'maxima-check' ? <LoaderCircle className="spin" /> : <RefreshCw />}Check for updates</button>
      {ready && !status?.signedIn ? <button className="primary" disabled={working} onClick={() => void invoke('maxima-signin', () => window.launcher.signInMaxima(), 'Signed in to EA.')}>{status?.signingIn ? <LoaderCircle className="spin" /> : <LogIn />}Sign in to EA</button> : null}
      {ready && status?.signedIn ? <button className="secondary" disabled={working} onClick={() => void invoke('maxima-library', async () => setLibrary(await window.launcher.getEALibrary()))}>{busy === 'maxima-library' ? <LoaderCircle className="spin" /> : <Library />}Show EA library</button> : null}
      {status?.signedIn ? <button className="secondary" disabled={working} title="Removes the EA token stored by Maxima. Installed games are kept." onClick={() => void invoke('maxima-signout', () => window.launcher.signOutMaxima(), 'Signed out of EA.')}>Sign out</button> : null}
    </div>
    {library ? <ul className="maxima-library">{library.map(title => <li key={title.offerId}><strong>{title.name}</strong><small className="mono">{title.offerId}</small><span>{title.installed ? 'Installed' : 'Not installed'}</span></li>)}</ul> : null}
    {status?.building || logs.length > 0 ? <pre className="build-log">{logs.join('\n') || 'Waiting for output…'}</pre> : null}
  </section>;
}
