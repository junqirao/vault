package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"time"

	"vault/internal/apperr"
)

// SteamStoreGame 是 Steam 商店搜索返回的一个游戏条目。
type SteamStoreGame struct {
	AppID     int64    `json:"app_id"`
	Name      string   `json:"name"`
	Image     string   `json:"image"`
	Platforms []string `json:"platforms"`
	Owned     bool     `json:"owned"`
}

type steamStoreSearchResponse struct {
	Items []struct {
		Type      string `json:"type"`
		Name      string `json:"name"`
		ID        int64  `json:"id"`
		Image     string `json:"tiny_image"`
		Platforms struct {
			Windows bool `json:"windows"`
			Mac     bool `json:"mac"`
			Linux   bool `json:"linux"`
		} `json:"platforms"`
	} `json:"items"`
}

// SearchSteamStore 从服务端代理 Steam 商店搜索。
//
// 浏览器能访问 Steam 不代表服务端能访问；这里复用 steam.proxy / 系统代理 / 环境变量。
func (s *SteamAccountService) SearchSteamStore(ctx context.Context, query, lang, cc, accountID string) ([]SteamStoreGame, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, apperr.InvalidParam("q")
	}
	if len([]rune(query)) > 80 {
		query = string([]rune(query)[:80])
	}
	if strings.TrimSpace(lang) == "" {
		lang = "schinese"
	}
	if strings.TrimSpace(cc) == "" {
		cc = "CN"
	}
	u, err := url.Parse("https://store.steampowered.com/api/storesearch/")
	if err != nil {
		return nil, apperr.New("steam.store_search_failed", 502).WithCause(err)
	}
	q := u.Query()
	q.Set("term", query)
	q.Set("l", lang)
	q.Set("cc", cc)
	u.RawQuery = q.Encode()

	reqCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, apperr.New("steam.store_search_failed", 500).WithCause(err)
	}
	req.Header.Set("User-Agent", "Vault-Steam-Store-Search")
	req.Header.Set("Accept", "application/json")
	resp, err := s.steamHTTPClient(15 * time.Second).Do(req)
	if err != nil {
		return nil, apperr.New("steam.store_search_failed", 502).WithCause(err)
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, apperr.New("steam.store_search_failed", 502).WithArg("status", resp.StatusCode)
	}
	var payload steamStoreSearchResponse
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, apperr.New("steam.store_search_failed", 502).WithCause(err)
	}
	out := make([]SteamStoreGame, 0, len(payload.Items))
	owned := map[int64]bool{}
	if strings.TrimSpace(accountID) != "" {
		if apps, err := s.Store.ListSteamAccountApps(ctx, accountID); err == nil {
			for _, app := range apps {
				owned[app.AppID] = true
			}
		}
	}
	for _, item := range payload.Items {
		if item.Type != "app" || item.ID <= 0 || strings.TrimSpace(item.Name) == "" {
			continue
		}
		platforms := make([]string, 0, 3)
		if item.Platforms.Windows {
			platforms = append(platforms, "windows")
		}
		if item.Platforms.Mac {
			platforms = append(platforms, "macos")
		}
		if item.Platforms.Linux {
			platforms = append(platforms, "linux")
		}
		out = append(out, SteamStoreGame{
			AppID: item.ID, Name: item.Name, Image: item.Image, Platforms: platforms,
			Owned: owned[item.ID],
		})
		if len(out) >= 40 {
			break
		}
	}
	return out, nil
}
