package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSteamLibraryRoot(t *testing.T) {
	cases := map[string]string{
		`F:\Game\SteamLibrary`:                   `F:\Game\SteamLibrary`,
		`F:\Game\SteamLibrary\steamapps`:         `F:\Game\SteamLibrary`,
		`F:\Game\SteamLibrary\steamapps\common`:  `F:\Game\SteamLibrary`,
		`F:\Game\SteamLibrary\steamapps\common\`: `F:\Game\SteamLibrary`,
	}
	for input, want := range cases {
		if got := steamLibraryRoot(input); got != want {
			t.Fatalf("steamLibraryRoot(%q) = %q，期望 %q", input, got, want)
		}
	}
}

func TestWriteSteamAppManifest(t *testing.T) {
	library := t.TempDir()
	mountPath := filepath.Join(library, "steamapps", "common", "My Game")
	spec := &MountSpec{
		SteamAppID:    730,
		SteamAppName:  "Counter-Strike 2",
		SteamBuildID:  "12345",
		DiskSizeBytes: 123456789,
	}
	path, err := writeSteamAppManifest(spec, library, mountPath)
	if err != nil {
		t.Fatalf("写入 appmanifest 失败: %v", err)
	}
	want := filepath.Join(library, "steamapps", "appmanifest_730.acf")
	if path != want {
		t.Fatalf("manifest 路径 = %q，期望 %q", path, want)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取 appmanifest 失败: %v", err)
	}
	text := string(raw)
	for _, needle := range []string{`"appid"	"730"`, `"installdir"	"My Game"`, `"buildid"	"12345"`} {
		if !strings.Contains(text, needle) {
			t.Fatalf("appmanifest 缺少 %q:\n%s", needle, text)
		}
	}
}
