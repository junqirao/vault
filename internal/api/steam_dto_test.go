package api

import (
	"encoding/json"
	"strings"
	"testing"

	"vault/internal/app"
	"vault/internal/domain"
)

// TestSteamAccountDTONeverLeaksSecrets 锁定账号 DTO 的安全边界：
// 凭据、长期 token、Guard secret 只允许以“是否已设置”的布尔值出现。
func TestSteamAccountDTONeverLeaksSecrets(t *testing.T) {
	a := &domain.SteamAccount{
		ID:                "acc-1",
		Username:          "content-01",
		DisplayName:       "内容账号",
		Status:            domain.SteamAccountStatusActive,
		CredentialCipher:  []byte("credential-cipher-secret"),
		CredentialKind:    domain.SteamCredentialPassword,
		AuthTokenCipher:   []byte("refresh-token-secret"),
		GuardSecretCipher: []byte("totp-secret"),
		GuardType:         domain.SteamGuardTOTP,
		MaxConcurrent:     1,
		SessionState:      domain.SteamSessionValid,
	}
	raw, err := json.Marshal(toSteamAccountDTO(a))
	if err != nil {
		t.Fatalf("序列化 DTO 失败: %v", err)
	}
	body := string(raw)
	for _, forbidden := range []string{
		"credential-cipher-secret", "refresh-token-secret", "totp-secret",
		"credential_cipher", "auth_token_cipher", "guard_secret_cipher",
	} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("DTO 泄漏敏感字段 %q: %s", forbidden, body)
		}
	}
	if !strings.Contains(body, `"has_credential":true`) || !strings.Contains(body, `"has_guard_secret":true`) {
		t.Fatalf("DTO 应只暴露“是否已设置”: %s", body)
	}
}

// TestGameCatalogDTOHidesManagementFields 锁定用户游戏目录的暴露面：
// 只给挂载所需的游戏/库信息，不包含 Steam 账号、管理备注或客户端私有配置。
func TestGameCatalogDTOHidesManagementFields(t *testing.T) {
	group := &domain.RepoGroup{ID: "g1", Name: "单机游戏", Kind: domain.RepoGroupSteam}
	repoID := "repo-1"
	game := &domain.Game{
		ID: "game-1", GroupID: group.ID, RepoID: &repoID, AppID: 730, AppName: "CS2",
		BuildID: "100", Status: domain.GameStatusReady,
	}
	repo := &domain.Repository{
		ID: repoID, Name: "CS2", Mode: domain.RepoModeShared, OwnerID: "admin",
		State: domain.RepoStateActive,
		Meta:  domain.RepoMeta{Note: "管理备注", ClientConfig: map[string]any{"secret": "x"}},
	}
	items := []app.GameCatalogItem{{Group: group, Game: game, Repo: repo}}
	raw, err := json.Marshal(toGameCatalogDTOs(items))
	if err != nil {
		t.Fatalf("序列化目录失败: %v", err)
	}
	body := string(raw)
	for _, forbidden := range []string{"管理备注", "client_config", "steam_account_id", "install_dir"} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("游戏目录泄漏字段 %q: %s", forbidden, body)
		}
	}
}
