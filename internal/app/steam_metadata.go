package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"vault/internal/apperr"
	"vault/internal/domain"
)

// steamMetadataTimeout 是单批商店元数据请求的超时。
//
// 代理/加速器链路偶尔会慢到 15 秒以上；超时后前端只能拿到 AppID。这里留出更宽裕的
// 时间，并在失败时重试一次。
const steamMetadataTimeout = 25 * time.Second

// OwnedGames 返回账号已同步的全部游戏，并附带商店名称/封面/平台。
//
// 这是"账号库"列表的数据源：用户不需要按名称搜索，直接看到账号拥有的全部游戏。
// 元数据按 AppID 缓存在 steam_apps，首次加载会按批请求 Steam 商店。
func (s *SteamAccountService) OwnedGames(ctx context.Context, accountID, lang, cc string, refresh bool) ([]SteamStoreGame, error) {
	if _, err := s.Get(ctx, accountID); err != nil {
		return nil, err
	}
	apps, err := s.Store.ListSteamAccountApps(ctx, accountID)
	if err != nil {
		return nil, err
	}
	if len(apps) == 0 {
		return []SteamStoreGame{}, nil
	}
	ids := make([]int64, 0, len(apps))
	for _, app := range apps {
		if app.AppID > 0 {
			ids = append(ids, app.AppID)
		}
	}
	if len(ids) == 0 {
		return []SteamStoreGame{}, nil
	}
	if strings.TrimSpace(lang) == "" {
		lang = "schinese"
	}
	if strings.TrimSpace(cc) == "" {
		cc = "CN"
	}

	meta, err := s.Store.GetSteamApps(ctx, ids)
	if err != nil {
		return nil, err
	}
	missing := make([]int64, 0)
	for _, id := range ids {
		m, ok := meta[id]
		if refresh || !ok || strings.TrimSpace(m.Name) == "" || steamAppNameIsPlaceholder(m.Name) || m.AppType < 0 {
			missing = append(missing, id)
		}
	}
	// 首次加载可能面对上千个许可 AppID；分页解析元数据，单次请求最多解析一批，
	// 后续刷新会继续补齐，避免一个 HTTP 请求拖到浏览器超时。
	const maxResolvePerRequest = 900
	if len(missing) > maxResolvePerRequest {
		missing = missing[:maxResolvePerRequest]
	}
	if len(missing) > 0 {
		resolved, resolveErr := s.resolveAppMetadata(ctx, missing, lang, cc)
		// 即使部分批次失败，也要使用已经拿到的元数据，不能整批退回 AppID。
		for _, m := range resolved {
			meta[m.AppID] = m
		}
		if resolveErr != nil && s.Log != nil {
			s.Log.Warn("解析 Steam 商店元数据失败，使用缓存/AppID 回退", "error", resolveErr, "count", len(missing))
		}
	}

	out := make([]SteamStoreGame, 0, len(ids))
	for _, id := range ids {
		m, ok := meta[id]
		if !ok {
			continue
		}
		// 只展示游戏本体：DLC(4)、Demo(1)、软件(6)、工具/内置应用(-1 或解析失败)全部过滤。
		if m.AppType != 0 {
			continue
		}
		name := strings.TrimSpace(m.Name)
		if name == "" || steamAppNameIsPlaceholder(name) {
			continue
		}
		platforms := splitPlatforms(m.Platforms)
		image := m.Image
		if image == "" {
			image = fmt.Sprintf("https://cdn.cloudflare.steamstatic.com/steam/apps/%d/header.jpg", id)
		}
		out = append(out, SteamStoreGame{
			AppID: id, Name: name, Image: image, Platforms: platforms, Owned: true,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	return out, nil
}

// steamAppNameIsPlaceholder 判断名称是否只是 "App <id>" 这种客户端回退值。
//
// 客户端扫描不到 appinfo 时会用 "App 123" 占位；这种名字不能当成已解析元数据，
// 否则服务端永远不会再去商店补齐真实名称/封面。
func steamAppNameIsPlaceholder(name string) bool {
	name = strings.TrimSpace(name)
	if !strings.HasPrefix(name, "App ") {
		return false
	}
	_, err := strconv.ParseInt(strings.TrimSpace(strings.TrimPrefix(name, "App ")), 10, 64)
	return err == nil
}

func splitPlatforms(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		// 不能返回 nil：JSON 会编码成 null，前端按数组使用时会直接抛异常。
		return []string{}
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

type storeBrowseResponse struct {
	Response struct {
		StoreItems []struct {
			AppID   int64  `json:"appid"`
			Success int    `json:"success"`
			Visible bool   `json:"visible"`
			Type    int    `json:"type"`
			Name    string `json:"name"`
			Assets  struct {
				AssetURLFormat string `json:"asset_url_format"`
				Header         string `json:"header"`
				SmallCapsule   string `json:"small_capsule"`
			} `json:"assets"`
			Platforms struct {
				Windows      bool `json:"windows"`
				SteamosLinux bool `json:"steamos_linux"`
			} `json:"platforms"`
		} `json:"store_items"`
	} `json:"response"`
}

func (s *SteamAccountService) resolveAppMetadata(ctx context.Context, appIDs []int64, lang, cc string) ([]domain.SteamAppMeta, error) {
	const batchSize = 50
	out := make([]domain.SteamAppMeta, 0, len(appIDs))
	var firstErr error
	for start := 0; start < len(appIDs); start += batchSize {
		end := start + batchSize
		if end > len(appIDs) {
			end = len(appIDs)
		}
		batch, err := s.resolveAppMetadataBatch(ctx, appIDs[start:end], lang, cc)
		if err != nil {
			// 代理/网络偶发抖动很常见：重试一次再放弃。
			select {
			case <-ctx.Done():
				firstErr = ctx.Err()
			case <-time.After(500 * time.Millisecond):
				batch, err = s.resolveAppMetadataBatch(ctx, appIDs[start:end], lang, cc)
				if err != nil {
					// 网络/代理持续故障时继续打后面十几个批次只会拖长请求；
					// 已解析的部分会落缓存，下次刷新再补齐剩余 AppID。
					firstErr = err
				}
			}
		}
		if firstErr != nil {
			break
		}
		out = append(out, batch...)
	}
	if len(out) > 0 {
		if err := s.Store.UpsertSteamApps(ctx, out); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return out, firstErr
}

func (s *SteamAccountService) resolveAppMetadataBatch(ctx context.Context, appIDs []int64, lang, cc string) ([]domain.SteamAppMeta, error) {
	ids := make([]map[string]int64, 0, len(appIDs))
	for _, id := range appIDs {
		ids = append(ids, map[string]int64{"appid": id})
	}
	input := map[string]any{
		"ids": ids,
		"context": map[string]any{
			"language": lang, "country_code": cc, "steam_realm": 1,
		},
		"data_request": map[string]any{
			"include_assets": true, "include_platforms": true,
		},
	}
	raw, err := json.Marshal(input)
	if err != nil {
		return nil, err
	}
	u := "https://api.steampowered.com/IStoreBrowseService/GetItems/v1/?input_json=" + url.QueryEscape(string(raw))
	reqCtx, cancel := context.WithTimeout(ctx, steamMetadataTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, u, nil)
	if err != nil {
		return nil, apperr.New("steam.metadata_failed", 500).WithCause(err)
	}
	req.Header.Set("User-Agent", "Vault-Steam-Metadata")
	resp, err := s.steamHTTPClient(steamMetadataTimeout).Do(req)
	if err != nil {
		// 剥掉 *url.Error 里的完整 URL：一个批次包含 50 个 AppID，整条 URL 会把日志刷屏。
		var urlErr *url.Error
		if errors.As(err, &urlErr) {
			err = urlErr.Err
		}
		return nil, apperr.New("steam.metadata_failed", 502).WithCause(err)
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, apperr.New("steam.metadata_failed", 502).WithArg("status", resp.StatusCode)
	}
	var payload storeBrowseResponse
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, apperr.New("steam.metadata_failed", 502).WithCause(err)
	}
	now := time.Now().UnixMilli()
	out := make([]domain.SteamAppMeta, 0, len(payload.Response.StoreItems))
	for _, item := range payload.Response.StoreItems {
		if item.Success == 0 || item.AppID <= 0 || strings.TrimSpace(item.Name) == "" {
			continue
		}
		platforms := make([]string, 0, 2)
		if item.Platforms.Windows {
			platforms = append(platforms, "windows")
		}
		if item.Platforms.SteamosLinux {
			platforms = append(platforms, "linux")
		}
		image := ""
		if item.Assets.AssetURLFormat != "" {
			filename := item.Assets.Header
			if filename == "" {
				filename = item.Assets.SmallCapsule
			}
			if filename != "" {
				image = "https://shared.akamai.steamstatic.com/store_item_assets/" +
					strings.ReplaceAll(item.Assets.AssetURLFormat, "${FILENAME}", filename)
			}
		}
		if image == "" {
			image = fmt.Sprintf("https://cdn.cloudflare.steamstatic.com/steam/apps/%d/header.jpg", item.AppID)
		}
		out = append(out, domain.SteamAppMeta{
			AppID: item.AppID, Name: item.Name, Image: image,
			Platforms: strings.Join(platforms, ","), AppType: item.Type, UpdatedAt: now,
		})
	}
	return out, nil
}
