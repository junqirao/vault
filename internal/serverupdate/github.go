package serverupdate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"runtime"
	"strings"
	"time"

	"vault/internal/releasever"
)

const (
	// releaseRepo 是服务端自更新的固定发布源。这里刻意不提供配置项：
	// 更新源只允许是项目官方 Release，避免 token/代理/仓库配置被误改或泄露。
	releaseRepo     = "junqirao/vault"
	githubAPIBase   = "https://api.github.com"
	githubUserAgent = "Vault-Server-Update"
	maxReleaseBody  = 8 << 20
)

type githubRelease struct {
	TagName     string        `json:"tag_name"`
	Name        string        `json:"name"`
	Draft       bool          `json:"draft"`
	Prerelease  bool          `json:"prerelease"`
	Body        string        `json:"body"`
	HTMLURL     string        `json:"html_url"`
	PublishedAt time.Time     `json:"published_at"`
	Assets      []githubAsset `json:"assets"`
}

type githubAsset struct {
	Name               string `json:"name"`
	Size               int64  `json:"size"`
	Digest             string `json:"digest"`
	BrowserDownloadURL string `json:"browser_download_url"`
}

type githubError struct {
	Message string `json:"message"`
}

// checkOptions 是一次 GitHub 查询的参数。
type checkOptions struct {
	Current     releasever.Version
	CurrentChan string
	GOOS        string
	GOARCH      string
}

// checkResult 是选中的候选版本与新 ETag。
type checkResult struct {
	Info *ReleaseInfo
	ETag string
}

// githubCheck 查询 GitHub Release 并选出适合当前平台的最高版本。
//
// 使用 /releases 列表而不是 /releases/latest：后者不返回预发布版本，
// 而本项目的 beta / rc 都是 prerelease。
func githubCheck(ctx context.Context, opt checkOptions) (*checkResult, *UpdateError) {
	apiURL := fmt.Sprintf("%s/repos/%s/releases?per_page=100", githubAPIBase, releaseRepo)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		return nil, wrapError("server_update.github_unreachable", err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", githubUserAgent)

	client := githubHTTPClient()
	resp, err := client.Do(req)
	if err != nil {
		return nil, wrapError("server_update.github_unreachable", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests {
		// GitHub 用 403/429 表示限流；把 reset 时间带回去，界面可以提示多久后再试。
		args := map[string]any{}
		if reset := strings.TrimSpace(resp.Header.Get("X-RateLimit-Reset")); reset != "" {
			args["reset"] = reset
		}
		return nil, &UpdateError{Code: "server_update.github_rate_limited", Args: args}
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
		var ghErr githubError
		_ = json.Unmarshal(body, &ghErr)
		return nil, &UpdateError{
			Code:    "server_update.github_http_error",
			Message: strings.TrimSpace(ghErr.Message),
			Args:    map[string]any{"status": resp.StatusCode},
		}
	}

	var releases []githubRelease
	dec := json.NewDecoder(io.LimitReader(resp.Body, maxReleaseBody))
	if err := dec.Decode(&releases); err != nil {
		return nil, wrapError("server_update.github_bad_response", err)
	}

	info, reason := selectRelease(releases, opt.Current, opt.CurrentChan, opt.GOOS, opt.GOARCH)
	if info == nil && reason != "" {
		return nil, &UpdateError{Code: "server_update." + reason}
	}
	return &checkResult{Info: info, ETag: strings.TrimSpace(resp.Header.Get("ETag"))}, nil
}

// githubReleaseByTag 按 tag 重新查询单个 Release，用于资产下载地址过期后的刷新。
func githubReleaseByTag(ctx context.Context, tag string) (*ReleaseInfo, *UpdateError) {
	tag = strings.TrimSpace(tag)
	if tag == "" {
		return nil, &UpdateError{Code: "server_update.not_ready"}
	}
	apiURL := fmt.Sprintf("%s/repos/%s/releases/tags/%s", githubAPIBase, releaseRepo, url.PathEscape(tag))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		return nil, wrapError("server_update.github_unreachable", err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", githubUserAgent)
	client := githubHTTPClient()
	resp, err := client.Do(req)
	if err != nil {
		return nil, wrapError("server_update.github_unreachable", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
		return nil, &UpdateError{Code: "server_update.github_http_error", Message: strings.TrimSpace(string(body)), Args: map[string]any{"status": resp.StatusCode}}
	}
	var rel githubRelease
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxReleaseBody)).Decode(&rel); err != nil {
		return nil, wrapError("server_update.github_bad_response", err)
	}
	v, err := releasever.Parse(rel.TagName)
	if err != nil {
		return nil, wrapError("server_update.github_bad_response", err)
	}
	asset, sha := findServerAsset(rel.Assets, runtime.GOOS, runtime.GOARCH, v.String())
	if asset == nil {
		return nil, &UpdateError{Code: "server_update.no_platform_asset"}
	}
	channel := v.PreChannel()
	if channel == "" {
		channel = "stable"
	}
	return &ReleaseInfo{
		Version:   v.String(),
		Channel:   channel,
		Tag:       strings.TrimSpace(rel.TagName),
		Notes:     strings.TrimSpace(rel.Body),
		HTMLURL:   strings.TrimSpace(rel.HTMLURL),
		Published: rel.PublishedAt.UnixMilli(),
		AssetName: asset.Name,
		AssetSize: asset.Size,
		SHA256:    sha,
		AssetURL:  asset.BrowserDownloadURL,
	}, nil
}

// githubSHA256ForAsset 在 asset digest 缺失时从同一 Release 的 SHA256SUMS.txt 里找摘要。
func githubSHA256ForAsset(ctx context.Context, tag, assetName string) (string, *UpdateError) {
	tag = strings.TrimSpace(tag)
	if tag == "" {
		return "", &UpdateError{Code: "server_update.digest_unavailable"}
	}
	apiURL := fmt.Sprintf("%s/repos/%s/releases/tags/%s", githubAPIBase, releaseRepo, url.PathEscape(tag))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		return "", wrapError("server_update.github_unreachable", err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", githubUserAgent)
	client := githubHTTPClient()
	resp, err := client.Do(req)
	if err != nil {
		return "", wrapError("server_update.github_unreachable", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", &UpdateError{Code: "server_update.digest_unavailable", Args: map[string]any{"status": resp.StatusCode}}
	}
	var rel githubRelease
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxReleaseBody)).Decode(&rel); err != nil {
		return "", wrapError("server_update.github_bad_response", err)
	}
	var sumsURL string
	for _, asset := range rel.Assets {
		if strings.EqualFold(asset.Name, "SHA256SUMS.txt") {
			sumsURL = asset.BrowserDownloadURL
			break
		}
	}
	if sumsURL == "" {
		return "", &UpdateError{Code: "server_update.digest_unavailable"}
	}
	sumReq, err := http.NewRequestWithContext(ctx, http.MethodGet, sumsURL, nil)
	if err != nil {
		return "", wrapError("server_update.github_unreachable", err)
	}
	sumReq.Header.Set("User-Agent", githubUserAgent)
	sumResp, err := client.Do(sumReq)
	if err != nil {
		return "", wrapError("server_update.github_unreachable", err)
	}
	defer func() { _ = sumResp.Body.Close() }()
	if sumResp.StatusCode != http.StatusOK {
		return "", &UpdateError{Code: "server_update.digest_unavailable", Args: map[string]any{"status": sumResp.StatusCode}}
	}
	data, err := io.ReadAll(io.LimitReader(sumResp.Body, 1<<20))
	if err != nil {
		return "", wrapError("server_update.github_unreachable", err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) < 2 {
			continue
		}
		name := strings.TrimPrefix(fields[len(fields)-1], "*")
		if strings.EqualFold(name, assetName) {
			sha := strings.ToLower(fields[0])
			if _, err := hex.DecodeString(sha); err == nil && len(sha) == sha256.Size*2 {
				return sha, nil
			}
		}
	}
	return "", &UpdateError{Code: "server_update.digest_unavailable"}
}

// selectRelease 从 release 列表中选择当前通道允许的最高版本。
//
// 纯函数，便于单元测试：不联网、不做 I/O。
func selectRelease(releases []githubRelease, current releasever.Version, currentChannel, goos, goarch string) (*ReleaseInfo, string) {
	currentChannel = normalizeChannel(currentChannel)
	curRank := channelRank(currentChannel)

	var best *ReleaseInfo
	var bestVersion releasever.Version
	missingAsset := false
	for i := range releases {
		rel := &releases[i]
		if rel.Draft {
			continue
		}
		v, err := releasever.Parse(rel.TagName)
		if err != nil {
			continue
		}
		channel := v.PreChannel()
		if channel == "" {
			channel = "stable"
		}
		// 防御：GitHub 标记为 prerelease 但 tag 没有后缀的异常数据不当作正式版。
		if rel.Prerelease && channel == "stable" {
			channel = "beta"
		}
		// 当前 stable 只接受 stable；beta 接受 beta/rc/stable，以此类推。
		if channelRank(channel) < curRank {
			continue
		}
		if releasever.Compare(v, current) <= 0 {
			continue
		}
		asset, sha := findServerAsset(rel.Assets, goos, goarch, v.String())
		if asset == nil {
			// 有更高版本、但当前平台没有资产：不要静默当作“已是最新”。
			missingAsset = true
			continue
		}
		if best != nil && releasever.Compare(v, bestVersion) <= 0 {
			continue
		}
		bestVersion = v
		best = &ReleaseInfo{
			Version:   v.String(),
			Channel:   channel,
			Tag:       strings.TrimSpace(rel.TagName),
			Notes:     strings.TrimSpace(rel.Body),
			HTMLURL:   strings.TrimSpace(rel.HTMLURL),
			Published: rel.PublishedAt.UnixMilli(),
			AssetName: asset.Name,
			AssetSize: asset.Size,
			SHA256:    sha,
			AssetURL:  asset.BrowserDownloadURL,
		}
	}
	if best != nil {
		return best, ""
	}
	if missingAsset {
		return nil, "no_platform_asset"
	}
	return nil, ""
}

func normalizeChannel(ch string) string {
	switch strings.ToLower(strings.TrimSpace(ch)) {
	case "stable", "":
		return "stable"
	case "alpha", "beta", "rc":
		return strings.ToLower(strings.TrimSpace(ch))
	default:
		return "stable"
	}
}

func channelRank(ch string) int {
	switch normalizeChannel(ch) {
	case "alpha":
		return 0
	case "beta":
		return 1
	case "rc":
		return 2
	default:
		return 3
	}
}

// findServerAsset 按平台与版本选取服务端资产。
//
// 要求文件名必须包含本次 tag 的版本，避免仓库把旧包挂到新 tag 上导致“更新后版本仍是旧的”。
func findServerAsset(assets []githubAsset, goos, goarch, version string) (*githubAsset, string) {
	suffix := serverAssetSuffix(goos, goarch)
	if suffix == "" {
		return nil, ""
	}
	version = strings.ToLower(strings.TrimSpace(version))
	for i := range assets {
		name := assets[i].Name
		lower := strings.ToLower(name)
		if !strings.HasPrefix(lower, "vault-server-") || !strings.HasSuffix(lower, suffix) {
			continue
		}
		if version != "" && !strings.Contains(lower, "-"+version+"-") {
			continue
		}
		sha := digestHex(assets[i].Digest)
		return &assets[i], sha
	}
	return nil, ""
}

func serverAssetSuffix(goos, goarch string) string {
	switch strings.ToLower(goos) {
	case "windows":
		if strings.ToLower(goarch) == "amd64" {
			return "-win64.zip"
		}
	case "linux":
		if strings.ToLower(goarch) == "amd64" {
			return "-linux-amd64.tar.gz"
		}
	}
	return ""
}

// digestHex 把 GitHub 的 `sha256:<hex>` 转成纯小写 hex；格式不对返回空串。
func digestHex(digest string) string {
	d := strings.TrimSpace(digest)
	if d == "" {
		return ""
	}
	if i := strings.IndexByte(d, ':'); i >= 0 {
		d = d[i+1:]
	}
	d = strings.ToLower(strings.TrimSpace(d))
	if len(d) != sha256.Size*2 {
		return ""
	}
	if _, err := hex.DecodeString(d); err != nil {
		return ""
	}
	return d
}

// githubHTTPClient 使用标准环境代理（HTTP_PROXY/HTTPS_PROXY），不提供自定义代理配置。
func githubHTTPClient() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	return &http.Client{
		Transport: transport,
		Timeout:   45 * time.Second,
	}
}

func wrapError(code string, err error) *UpdateError {
	if err == nil {
		return &UpdateError{Code: code}
	}
	return &UpdateError{Code: code, Message: err.Error()}
}
