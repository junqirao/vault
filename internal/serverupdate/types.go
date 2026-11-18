package serverupdate

import "time"

// State 是服务端自更新的状态机。
type State string

const (
	StateIdle        State = "idle"
	StateChecking    State = "checking"
	StateAvailable   State = "available"
	StateDownloading State = "downloading"
	StateDownloaded  State = "downloaded"
	StateApplying    State = "applying"
	StateRestarting  State = "restarting"
	StateFailed      State = "failed"
)

// CurrentInfo 是当前运行版本。
type CurrentInfo struct {
	// ReleaseVersion 对外发布版本（RELEASE.json），为空表示当前构建未注入。
	ReleaseVersion string `json:"release_version"`
	// ReleaseChannel stable / beta / rc / alpha。
	ReleaseChannel string `json:"release_channel"`
	// InternalVersion 内部构建号（VERSION）。
	InternalVersion string `json:"internal_version"`
	// Platform / Arch 当前平台（用于选资产）。
	Platform string `json:"platform"`
	Arch     string `json:"arch"`
}

// ReleaseAsset 是 GitHub Release 中的服务端资产。
type ReleaseAsset struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
	// Digest 形如 sha256:<hex>；GitHub 较新的 release asset 会返回。
	Digest             string `json:"digest,omitempty"`
	BrowserDownloadURL string `json:"browser_download_url"`
}

// ReleaseInfo 是候选版本信息。
type ReleaseInfo struct {
	Version   string `json:"version"`
	Channel   string `json:"channel"`
	Tag       string `json:"tag"`
	Notes     string `json:"notes,omitempty"`
	HTMLURL   string `json:"html_url,omitempty"`
	Published int64  `json:"published_at,omitempty"`

	AssetName string `json:"asset_name"`
	AssetSize int64  `json:"asset_size"`
	SHA256    string `json:"sha256,omitempty"`
	AssetURL  string `json:"-"`
}

// Progress 是下载进度。
type Progress struct {
	ReceivedBytes int64 `json:"received_bytes"`
	TotalBytes    int64 `json:"total_bytes"`
	// Percent 0~100；总大小未知时为 -1。
	Percent int `json:"percent"`
	// SpeedBytesPerSec 是最近一段时间的平均速度（尽力而为）。
	SpeedBytesPerSec int64 `json:"speed_bytes_per_sec"`
}

// UpdateError 是对外暴露的稳定错误。
type UpdateError struct {
	Code    string         `json:"code"`
	Message string         `json:"message,omitempty"`
	Args    map[string]any `json:"args,omitempty"`
}

// LastResult 是上一次替换/重启的结果（helper 写盘，服务端启动后读取）。
type LastResult struct {
	OK          bool   `json:"ok"`
	FromVersion string `json:"from_version,omitempty"`
	ToVersion   string `json:"to_version,omitempty"`
	Error       string `json:"error,omitempty"`
	FinishedAt  int64  `json:"finished_at,omitempty"`
}

// Status 是 GET /v1/system/server-update 的响应。
type Status struct {
	Current  CurrentInfo  `json:"current"`
	State    State        `json:"state"`
	Latest   *ReleaseInfo `json:"latest,omitempty"`
	Progress *Progress    `json:"progress,omitempty"`
	Error    *UpdateError `json:"error,omitempty"`
	// LastResult 是上一次更新结果；成功后可保留供界面展示。
	LastResult *LastResult `json:"last_result,omitempty"`

	CheckedAt int64 `json:"checked_at,omitempty"`

	// RestartMode / SystemdUnit 供界面提示本次会如何重启。
	RestartMode string `json:"restart_mode"`
	SystemdUnit string `json:"systemd_unit,omitempty"`
}

// LastResultLifetime 是 last-result.json 的保留时间；过期后不再展示，避免永久残留。
const LastResultLifetime = 7 * 24 * time.Hour
