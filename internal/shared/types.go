// Package shared mirrors src/shared/types.ts. JSON field names and optionality
// match the TypeScript contract so the React renderer stays unchanged.
package shared

type Page string

type BackendCapabilities struct {
	Commands                []string `json:"commands"`
	Account                 bool     `json:"account"`
	Profile                 bool     `json:"profile,omitempty"`
	GameActivity            bool     `json:"gameActivity,omitempty"`
	Achievements            bool     `json:"achievements,omitempty"`
	Library                 bool     `json:"library,omitempty"`
	Login                   bool     `json:"login"`
	ExternalAuth            bool     `json:"externalAuth,omitempty"`
	Logout                  bool     `json:"logout"`
	Download                bool     `json:"download"`
	Streaming               bool     `json:"streaming"`
	StreamingProgressJSON   bool     `json:"streamingProgressJson,omitempty"`
	StreamingControls       bool     `json:"streamingControls,omitempty"`
	StreamingBandwidthLimit bool     `json:"streamingBandwidthLimit,omitempty"`
	PackageSelection        bool     `json:"packageSelection,omitempty"`
	ContentPackageSelection bool     `json:"contentPackageSelection,omitempty"`
	Run                     bool     `json:"run"`
	Version                 string   `json:"version,omitempty"`
}

type GamePassSubscription struct {
	Status     string   `json:"status"` // active | none | unknown
	ProductIDs []string `json:"productIds"`
	CheckedAt  string   `json:"checkedAt"`
}

type AuthStatus struct {
	Running       bool                  `json:"running"`
	Stored        *bool                 `json:"stored"`
	Username      string                `json:"username,omitempty"`
	Gamertag      string                `json:"gamertag,omitempty"`
	AvatarDataURL string                `json:"avatarDataUrl,omitempty"`
	Subscription  *GamePassSubscription `json:"subscription,omitempty"`
	LastResult    string                `json:"lastResult,omitempty"` // success | failed
	LastAction    string                `json:"lastAction,omitempty"` // login | logout
	Note          string                `json:"note"`
}

type BackendPrerequisites struct {
	Git    bool `json:"git"`
	Cargo  bool `json:"cargo"`
	Rustc  bool `json:"rustc"`
	Protoc bool `json:"protoc"`
}

type BackendStatus struct {
	Available        bool                 `json:"available"`
	Source           string               `json:"source"` // external | managed | none
	BinaryPath       string               `json:"binaryPath,omitempty"`
	Commit           string               `json:"commit,omitempty"`
	UpstreamCommit   string               `json:"upstreamCommit,omitempty"`
	UpdateAvailable  bool                 `json:"updateAvailable"`
	Checking         bool                 `json:"checking"`
	Updating         bool                 `json:"updating"`
	ExternalRoot     string               `json:"externalRoot"`
	Capabilities     BackendCapabilities  `json:"capabilities"`
	Prerequisites    BackendPrerequisites `json:"prerequisites"`
	BlockReason      string               `json:"blockReason,omitempty"`
	LinuxRunPatch    string               `json:"linuxRunPatch,omitempty"`
	Version          string               `json:"version,omitempty"`
	Delivery         string               `json:"delivery,omitempty"` // release | local
	ReleaseAvailable bool                 `json:"releaseAvailable,omitempty"`
	ReleaseVersion   string               `json:"releaseVersion,omitempty"`
	Phase            string               `json:"phase,omitempty"`
}

type RuntimePrerequisites struct {
	GH            bool `json:"gh"`
	Authenticated bool `json:"authenticated"`
	Tar           bool `json:"tar"`
	SHA512Sum     bool `json:"sha512sum"`
}

type RuntimeStatus struct {
	Installed         bool                 `json:"installed"`
	Checking          bool                 `json:"checking"`
	Updating          bool                 `json:"updating"`
	RuntimeRoot       string               `json:"runtimeRoot"`
	Name              string               `json:"name,omitempty"`
	WinePath          string               `json:"winePath,omitempty"`
	Commit            string               `json:"commit,omitempty"`
	UpstreamCommit    string               `json:"upstreamCommit,omitempty"`
	ArtifactName      string               `json:"artifactName,omitempty"`
	ArtifactExpiresAt string               `json:"artifactExpiresAt,omitempty"`
	ArtifactAvailable bool                 `json:"artifactAvailable"`
	NexusAvailable    bool                 `json:"nexusAvailable,omitempty"`
	NexusName         string               `json:"nexusName,omitempty"`
	NexusVersion      string               `json:"nexusVersion,omitempty"`
	NexusChecking     bool                 `json:"nexusChecking,omitempty"`
	NexusError        string               `json:"nexusError,omitempty"`
	Prerequisites     RuntimePrerequisites `json:"prerequisites"`
	Phase             string               `json:"phase,omitempty"`
	Error             string               `json:"error,omitempty"`
}

type GameLaunchTools struct {
	DualSenseCompatibility *bool  `json:"dualSenseCompatibility,omitempty"`
	MangoHud               bool   `json:"mangoHud"`
	Gamescope              bool   `json:"gamescope"`
	GamescopeArgs          string `json:"gamescopeArgs"`
	LaunchOptions          string `json:"launchOptions"`
	// CPULimit caps the logical CPUs Wine reports; 0 means no limit. Older
	// games crash on hosts with many cores (Dead Space 2 divides by zero).
	CPULimit int `json:"cpuLimit,omitempty"`
}

type AntiCheat struct {
	Provider        string `json:"provider"` // Easy Anti-Cheat
	Executable      string `json:"executable,omitempty"`
	EncryptedTarget bool   `json:"encryptedTarget"`
}

type Game struct {
	ID                string           `json:"id"`
	Title             string           `json:"title"`
	Path              string           `json:"path"`
	StoreID           string           `json:"storeId,omitempty"`
	OfferProductID    string           `json:"offerProductId,omitempty"` // Requested Store edition, not a license or replacement for the base identity.
	Publisher         string           `json:"publisher,omitempty"`
	InstalledVersion  string           `json:"installedVersion,omitempty"`
	PackageName       string           `json:"packageName,omitempty"`
	Executable        string           `json:"executable,omitempty"`
	WinePath          string           `json:"winePath,omitempty"` // Legacy saved field; ignored for launch. The runtime is global.
	LaunchArgs        string           `json:"launchArgs,omitempty"`
	LaunchTools       *GameLaunchTools `json:"launchTools,omitempty"`
	Locale            string           `json:"locale,omitempty"`
	CoverDataURL      string           `json:"coverDataUrl,omitempty"`
	CoverURL          string           `json:"coverUrl,omitempty"` // Store box art, preferred over the installation's small logo.
	HeroURL           string           `json:"heroUrl,omitempty"`  // Wide Store artwork for the game detail panel.
	InstallState      string           `json:"installState"`       // ready | incomplete | imported
	InstallMessage    string           `json:"installMessage,omitempty"`
	VCRedistEncrypted *bool            `json:"vcRedistEncrypted,omitempty"`
	AntiCheat         *AntiCheat       `json:"antiCheat,omitempty"`
	Provider          string           `json:"provider,omitempty"` // maxima: launched through Maxima, not Xodus
	OfferID           string           `json:"offerId,omitempty"`
	EASlug            string           `json:"eaSlug,omitempty"`
	AddedAt           string           `json:"addedAt"`
}

type GameLaunchOptions struct {
	CloudSaves  bool   `json:"cloudSaves"`
	CloudReason string `json:"cloudReason,omitempty"`
	VCInstall   bool   `json:"vcInstall"`
	VCReason    string `json:"vcReason,omitempty"`
}

type DownloadTask struct {
	ID                string   `json:"id"`
	Kind              string   `json:"kind,omitempty"` // install | update | content
	GameID            string   `json:"gameId,omitempty"`
	Title             string   `json:"title"`
	ProductID         string   `json:"productId"`
	SelectedContentID string   `json:"selectedContentId,omitempty"`
	SelectionMarket   string   `json:"selectionMarket,omitempty"`
	Destination       string   `json:"destination"`
	Provider          string   `json:"provider,omitempty"` // maxima: an EA game installed by Maxima
	OfferID           string   `json:"offerId,omitempty"`  // EA offer, with provider maxima
	Status            string   `json:"status"`             // queued | running | paused | complete | failed | cancelled
	CanPause          bool     `json:"canPause,omitempty"`
	ControlPending    bool     `json:"controlPending,omitempty"`
	Cancelling        bool     `json:"cancelling,omitempty"`
	Progress          *float64 `json:"progress"`
	DownloadedBytes   *float64 `json:"downloadedBytes,omitempty"`
	TotalBytes        *float64 `json:"totalBytes,omitempty"`
	TransferRate      string   `json:"transferRate,omitempty"`
	CoverURL          string   `json:"coverUrl,omitempty"`
	Phase             string   `json:"phase"`
	Log               []string `json:"log"`
	StartedAt         string   `json:"startedAt"`
	FinishedAt        string   `json:"finishedAt,omitempty"`
	ExitCode          *int     `json:"exitCode,omitempty"`
	Error             string   `json:"error,omitempty"`
}

type DownloadCancelOptions struct {
	DeleteFilesAllowed bool   `json:"deleteFilesAllowed"`
	Reason             string `json:"reason,omitempty"`
}

type GameOperation struct {
	ID         string   `json:"id"`
	GameID     string   `json:"gameId"`
	Title      string   `json:"title"`
	Kind       string   `json:"kind,omitempty"` // launch | prerequisite
	Status     string   `json:"status"`         // preparing | running | stopping | complete | failed
	Phase      string   `json:"phase"`
	Log        []string `json:"log"`
	LogPath    string   `json:"logPath,omitempty"`
	StartedAt  string   `json:"startedAt"`
	FinishedAt string   `json:"finishedAt,omitempty"`
	Error      string   `json:"error,omitempty"`
}

type Settings struct {
	InstallRoot       string   `json:"installRoot"`
	Market            string   `json:"market"`
	ParallelDownloads int      `json:"parallelDownloads"`
	DownloadLimitMiB  *float64 `json:"downloadLimitMiB,omitempty"`
	WinePath          string   `json:"winePath"`
	ExternalXodusRoot string   `json:"externalXodusRoot"`
	RuntimeRoot       string   `json:"runtimeRoot"`
	VerboseLogs       bool     `json:"verboseLogs"`
	FirstRunCompleted bool     `json:"firstRunCompleted,omitempty"`
}

type LauncherUpdate struct {
	CurrentVersion  string   `json:"currentVersion"`
	LatestVersion   string   `json:"latestVersion,omitempty"`
	ReleaseURL      string   `json:"releaseUrl,omitempty"`
	Checking        bool     `json:"checking"`
	CheckedAt       string   `json:"checkedAt,omitempty"`
	Updating        bool     `json:"updating,omitempty"`
	Progress        *float64 `json:"progress,omitempty"`
	Phase           string   `json:"phase,omitempty"`
	Error           string   `json:"error,omitempty"`
	CanInstall      bool     `json:"canInstall,omitempty"`
	RestartRequired bool     `json:"restartRequired,omitempty"`
	CanRollback     bool     `json:"canRollback,omitempty"`
}

type GameUpdate struct {
	Checking      bool   `json:"checking"`
	Available     *bool  `json:"available,omitempty"`
	LatestVersion string `json:"latestVersion,omitempty"`
	CheckedAt     string `json:"checkedAt,omitempty"`
	Error         string `json:"error,omitempty"`
}

type DiagnosticLog struct {
	Name      string `json:"name"`
	Content   string `json:"content"`
	Truncated bool   `json:"truncated"`
}

type DiagnosticReport struct {
	Name    string `json:"name"`
	Content string `json:"content"`
}

type KeyringStatus struct {
	CanCreate bool   `json:"canCreate,omitempty"`
	CanUnlock bool   `json:"canUnlock,omitempty"`
	State     string `json:"state"` // ready | locked | stopped | disabled | missing | needs-setup | unavailable
	GameMode  bool   `json:"gameMode"`
	CanStart  bool   `json:"canStart"`
	Message   string `json:"message"`
}

type AppSnapshot struct {
	Backend                   BackendStatus         `json:"backend"`
	Runtime                   RuntimeStatus         `json:"runtime"`
	Games                     []Game                `json:"games"`
	Downloads                 []DownloadTask        `json:"downloads"`
	Settings                  Settings              `json:"settings"`
	Auth                      AuthStatus            `json:"auth"`
	LauncherUpdate            *LauncherUpdate       `json:"launcherUpdate,omitempty"`
	GameOperation             *GameOperation        `json:"gameOperation,omitempty"`
	GameUpdates               map[string]GameUpdate `json:"gameUpdates,omitempty"`
	DownloadProgressSupported bool                  `json:"downloadProgressSupported,omitempty"`
	EAApp                     *EAAppStatus          `json:"eaApp,omitempty"`
	Maxima                    *MaximaStatus         `json:"maxima,omitempty"`
}

// MaximaStatus describes the Maxima build that installs and launches EA games.
type MaximaStatus struct {
	Available      bool   `json:"available"`
	Commit         string `json:"commit,omitempty"`         // active build
	PinnedCommit   string `json:"pinnedCommit"`             // build this launcher version expects
	UpstreamCommit string `json:"upstreamCommit,omitempty"` // latest Maxima commit, informational
	BuildRequired  bool   `json:"buildRequired"`
	Building       bool   `json:"building"`
	SignedIn       bool   `json:"signedIn"`
	SigningIn      bool   `json:"signingIn"`
	Account        string `json:"account,omitempty"`
	Phase          string `json:"phase,omitempty"`
	Error          string `json:"error,omitempty"`
	CanBuild       bool   `json:"canBuild"` // git and cargo are installed
}

// EATitle is a game in the signed-in EA account's library.
type EATitle struct {
	Slug      string `json:"slug"`
	Name      string `json:"name"`
	OfferID   string `json:"offerId"`
	Installed bool   `json:"installed"`
	// ExecutePath is the installed game executable, when installed.
	ExecutePath string `json:"executePath,omitempty"`
}

// EAAppStatus describes the EA app kept in its own Proton prefix for EA Play titles.
type EAAppStatus struct {
	Installed bool   `json:"installed"`
	Running   bool   `json:"running"`
	SignedIn  bool   `json:"signedIn"`
	Busy      bool   `json:"busy"`
	Proton    string `json:"proton,omitempty"` // display name of the selected Proton build
	Phase     string `json:"phase,omitempty"`
	Error     string `json:"error,omitempty"`
	LogPath   string `json:"logPath,omitempty"`
}

type InstallRequest struct {
	ProductID         string `json:"productId"`
	Title             string `json:"title"`
	Destination       string `json:"destination"`
	SelectedContentID string `json:"selectedContentId,omitempty"`
	SelectionMarket   string `json:"selectionMarket,omitempty"`
}

type InstallChoice struct {
	ProductID             string `json:"productId"`
	ContentID             string `json:"contentId"`
	Title                 string `json:"title"`
	Platform              string `json:"platform"`
	PackageFormat         string `json:"packageFormat"`
	Kind                  string `json:"kind"`
	Supported             bool   `json:"supported"`
	ParentProductID       string `json:"parentProductId,omitempty"`
	MainPackageFamilyName string `json:"mainPackageFamilyName,omitempty"`
}

type InstallPlan struct {
	Schema        int             `json:"schema"`
	RootProductID string          `json:"rootProductId"`
	Market        string          `json:"market"`
	Status        string          `json:"status"` // ready | choice | unavailable
	Choices       []InstallChoice `json:"choices"`
}

type LibraryAccess = string // owned | gamepass | other

type GameContentItem struct {
	ProductID     string        `json:"productId"`
	Title         string        `json:"title"`
	Access        LibraryAccess `json:"access,omitempty"`
	Status        string        `json:"status"` // installed | available | verify | unlicensed | unsupported | bundle | license-only
	DownloadBytes *float64      `json:"downloadBytes,omitempty"`
	Note          string        `json:"note"`
}

type GameContentPlan struct {
	GameID           string            `json:"gameId"`
	CheckedAt        string            `json:"checkedAt"`
	RuntimeSupported *bool             `json:"runtimeSupported,omitempty"`
	Items            []GameContentItem `json:"items"`
}

type CatalogProduct struct {
	ProductID        string   `json:"productId"`
	InstallProductID string   `json:"installProductId,omitempty"`
	Title            string   `json:"title"`
	Description      string   `json:"description"`
	Publisher        string   `json:"publisher,omitempty"`
	PosterURL        string   `json:"posterUrl,omitempty"`
	HeroURL          string   `json:"heroUrl,omitempty"`
	StoreURL         string   `json:"storeUrl,omitempty"`
	Categories       []string `json:"categories"`
	PurchasePrice    string   `json:"purchasePrice,omitempty"`
	IncludedWith     string   `json:"includedWith,omitempty"`
	ReleaseDate      string   `json:"releaseDate,omitempty"`
	AverageRating    *float64 `json:"averageRating,omitempty"`
	Installability   string   `json:"installability"` // supported | unsupported | unknown
	PackageFormat    string   `json:"packageFormat,omitempty"`
	SupportNote      string   `json:"supportNote"`
	Distributor      string   `json:"distributor,omitempty"` // ea-app: installed through the EA app, not Xodus
}

// GameStoreDetails is public catalog data only, never an ownership/installation decision.
type GameStoreDetails struct {
	ProductID   string   `json:"productId"`
	Title       string   `json:"title"`
	Description string   `json:"description"`
	Publisher   string   `json:"publisher,omitempty"`
	HeroURL     string   `json:"heroUrl,omitempty"`
	Categories  []string `json:"categories"`
	Screenshots []string `json:"screenshots"`
	ReleaseDate string   `json:"releaseDate,omitempty"`
}

type GameActivity struct {
	Status        string   `json:"status"` // available | unavailable | unsupported | signed-out | error
	MinutesPlayed *float64 `json:"minutesPlayed,omitempty"`
	LastPlayed    string   `json:"lastPlayed,omitempty"`
	Partial       bool     `json:"partial,omitempty"`
}

type GameAchievement struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	State       string   `json:"state"` // unlocked | in-progress | locked
	Secret      bool     `json:"secret"`
	Revoked     bool     `json:"revoked"`
	Gamerscore  *float64 `json:"gamerscore,omitempty"`
	IconURL     string   `json:"iconUrl,omitempty"`
	UnlockedAt  string   `json:"unlockedAt,omitempty"`
	Progress    *float64 `json:"progress,omitempty"`
}

type GameAchievements struct {
	Status  string            `json:"status"`
	Items   []GameAchievement `json:"items,omitempty"`
	Partial bool              `json:"partial,omitempty"`
}

type CatalogSearchResult struct {
	Query     string           `json:"query"`
	Market    string           `json:"market"`
	Products  []CatalogProduct `json:"products"`
	Cursor    string           `json:"cursor,omitempty"`
	HasMore   bool             `json:"hasMore"`
	FetchedAt string           `json:"fetchedAt"`
	Source    string           `json:"source"` // Microsoft Store
}

type LibraryProduct struct {
	CatalogProduct
	Access LibraryAccess `json:"access"`
}

type AccountLibraryPage struct {
	Kind                     string                   `json:"kind"` // all | owned | gamepass | other
	Products                 []LibraryProduct         `json:"products"`
	AccessByProductID        map[string]LibraryAccess `json:"accessByProductId"`
	UnclassifiedEntitlements *int                     `json:"unclassifiedEntitlements,omitempty"`
	ExcludedEntitlements     *int                     `json:"excludedEntitlements,omitempty"`
	Cursor                   string                   `json:"cursor,omitempty"`
	CheckedAt                string                   `json:"checkedAt"`
	SkippedEntitlements      *int                     `json:"skippedEntitlements,omitempty"`
}
