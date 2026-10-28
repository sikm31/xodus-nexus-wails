import { Call, Events } from '@wailsio/runtime';
import type { LauncherApi } from '../shared/types';

// window.launcher over Wails bindings. The Go service mirrors the original
// Electron IPC handlers, so the renderer itself stays unchanged.
const service = 'xodus-nexus-wails/internal/launcher.Launcher.';

// Bound methods require every declared argument; undefined becomes null.
function call<T>(method: string, ...args: unknown[]): Promise<T> {
  return Call.ByName(service + method, ...args.map(arg => arg === undefined ? null : arg)) as Promise<T>;
}

function on<T>(name: string, callback: (value: T) => void): () => void {
  return Events.On(name, event => callback(event.data as T));
}

export async function installLauncher(): Promise<void> {
  const demoTour = await call<boolean>('IsDemoTour');
  const api: LauncherApi = {
    getSnapshot: () => call('GetSnapshot'),
    refreshBackend: () => call('RefreshBackend'),
    checkBackendUpdate: () => call('CheckBackendUpdate'),
    updateBackend: () => call('UpdateBackend'),
    rebuildBackend: () => call('RebuildBackend'),
    refreshRuntime: () => call('RefreshRuntime'),
    checkRuntimeUpdate: () => call('CheckRuntimeUpdate'),
    checkNexusRuntime: () => call('CheckNexusRuntime'),
    installRuntime: () => call('InstallRuntime'),
    installNexusRuntime: () => call('InstallNexusRuntime'),
    rollbackRuntimePreview: () => call('RollbackRuntimePreview'),
    refreshAccount: () => call('RefreshAccount'),
    checkKeyring: () => call('CheckKeyring'),
    startKeyring: () => call('StartKeyring'),
    createKeyringCollection: () => call('CreateKeyringCollection'),
    onKeyringStatus: callback => on('keyring-state', callback),
    checkLauncherUpdate: () => call('CheckLauncherUpdate'),
    installLauncherUpdate: () => call('InstallLauncherUpdate'),
    restartLauncher: () => call('RestartLauncher'),
    rollbackLauncherUpdate: () => call('RollbackLauncherUpdate'),
    checkGameUpdates: id => call('CheckGameUpdates', id),
    readDiagnosticLog: id => call('ReadDiagnosticLog', id),
    copyDiagnosticLog: id => call('CopyDiagnosticLog', id),
    revealDiagnosticLog: id => call('RevealDiagnosticLog', id),
    readBackendDiagnosticLog: () => call('ReadBackendDiagnosticLog'),
    createDiagnosticReport: gameId => call('CreateDiagnosticReport', gameId),
    saveDiagnosticReport: content => call('SaveDiagnosticReport', content),
    login: () => call('Login'),
    logout: () => call('Logout'),
    searchCatalog: (query, cursor) => call('SearchCatalog', query, cursor),
    getFeaturedCatalog: () => call('GetFeaturedCatalog'),
    getAccountLibrary: (kind, cursor, refresh) => call('GetAccountLibrary', kind, cursor, refresh),
    startInstall: request => call('StartInstall', request),
    getInstallPlan: productId => call('GetInstallPlan', productId),
    getGameContent: id => call('GetGameContent', id),
    installGameContent: (id, productId) => call('InstallGameContent', id, productId),
    updateInstalledGame: id => call('UpdateInstalledGame', id),
    getDownloadCancelOptions: id => call('GetDownloadCancelOptions', id),
    cancelDownload: (id, mode) => call('CancelDownload', id, mode),
    pauseDownload: id => call('PauseDownload', id),
    resumeDownload: id => call('ResumeDownload', id),
    removeDownload: id => call('RemoveDownload', id),
    clearFinishedDownloads: () => call('ClearFinishedDownloads'),
    importGame: path => call('ImportGame', path),
    updateGame: (id, patch) => call('UpdateGame', id, patch),
    getGameLocales: id => call('GetGameLocales', id),
    removeGame: (id, mode) => call('RemoveGame', id, mode),
    getGameLaunchOptions: id => call('GetGameLaunchOptions', id),
    getGameDetails: async id => (await call<Awaited<ReturnType<LauncherApi['getGameDetails']>> | null>('GetGameDetails', id)) ?? undefined,
    getGameActivity: (id, refresh) => call('GetGameActivity', id, refresh),
    getGameAchievements: (id, refresh) => call('GetGameAchievements', id, refresh),
    launchGame: id => call('LaunchGame', id),
    createGameShortcut: (id, destination) => call('CreateGameShortcut', id, destination),
    addToSteam: id => call('AddToSteam', id),
    installGameVcRuntime: id => call('InstallGameVcRuntime', id),
    cancelGame: () => call('CancelGame'),
    updateSettings: patch => call('UpdateSettings', patch),
    chooseDirectory: defaultPath => call('ChooseDirectory', defaultPath),
    chooseFile: defaultPath => call('ChooseFile', defaultPath),
    openPath: target => call('OpenPath', target),
    installEAApp: () => call('InstallEAApp'),
    launchEAApp: () => call('LaunchEAApp'),
    stopEAApp: () => call('StopEAApp'),
    signOutEAApp: () => call('SignOutEAApp'),
    openEAAppFor: title => call('OpenEAAppFor', title),
    buildMaxima: () => call('BuildMaxima'),
    checkMaximaUpdate: () => call('CheckMaximaUpdate'),
    signInMaxima: () => call('SignInMaxima'),
    signOutMaxima: () => call('SignOutMaxima'),
    getEALibrary: () => call('GetEALibrary'),
    startEAInstall: request => call('StartEAInstall', request),
    openEAAccountLinking: () => call('OpenEAAccountLinking'),
    onSnapshot: callback => on('snapshot', callback),
    onOperationLog: callback => on('operation-log', callback),
    isDemoTour: () => demoTour,
  };
  Object.defineProperty(window, 'launcher', { value: Object.freeze(api), configurable: false, writable: false });
}
