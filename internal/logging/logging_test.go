package logging

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestReadDayAndPathForDay 锁定日志模块的核心：PathForDay("") 指向当天文件、ReadDay 只回
// 末尾且不漏最后几条。
func TestReadDayAndPathForDay(t *testing.T) {
	dir := t.TempDir()
	l, err := New(Options{Level: "debug", Dir: dir, FileName: "Vault-Agent", KeepDays: 7})
	if err != nil {
		t.Fatalf("构造 Logger 失败：%v", err)
	}
	defer l.Close()

	l.Info("line one", "k", "v1")
	l.Info("line two", "k", "v2")
	l.Warn("line three")

	today := time.Now().Format("2006-01-02")
	path := l.PathForDay("")
	if !strings.HasPrefix(path, filepath.Join(dir, "Vault-Agent-")) || !strings.HasSuffix(path, ".log") {
		t.Fatalf("PathForDay 形态不对：%s", path)
	}
	if path != l.PathForDay(today) {
		t.Fatalf("PathForDay(\"\") 应等于当天文件：%s vs %s", path, l.PathForDay(today))
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("日志文件不存在：%v", err)
	}

	data, err := l.ReadDay("", 1<<20)
	if err != nil {
		t.Fatalf("ReadDay 失败：%v", err)
	}
	text := string(data)
	for _, want := range []string{"line one", "line two", "line three"} {
		if !strings.Contains(text, want) {
			t.Fatalf("日志尾部缺少 %q：%s", want, text)
		}
	}

	// 小 tail：只回末尾，且最后一条必须完整落在里面。
	small, err := l.ReadDay("", 200)
	if err != nil {
		t.Fatalf("ReadDay(200) 失败：%v", err)
	}
	if len(small) > 200 {
		t.Fatalf("ReadDay(200) 返回 %d 字节，超过上限", len(small))
	}
	if !strings.Contains(string(small), "line three") {
		t.Fatalf("ReadDay(200) 应包含最后一条：%s", string(small))
	}

	// 当天文件已存在：Days 必须列出它。
	if days := l.Days(); len(days) != 1 || days[0] != today {
		t.Fatalf("Days 应为 [%s]，实际 %v", today, days)
	}
}

// TestDaysAndReadDayHistorical 历史日志按天查询：手工造出昨天的文件后，Days 列出两天、
// ReadDay 能读到昨天那份。
func TestDaysAndReadDayHistorical(t *testing.T) {
	dir := t.TempDir()
	l, err := New(Options{Level: "debug", Dir: dir, FileName: "Vault-Agent", KeepDays: 7})
	if err != nil {
		t.Fatalf("构造 Logger 失败：%v", err)
	}
	defer l.Close()

	yesterday := time.Now().AddDate(0, 0, -1).Format("2006-01-02")
	old := filepath.Join(dir, "Vault-Agent-"+yesterday+".log")
	if err := os.WriteFile(old, []byte("{\"level\":\"INFO\",\"msg\":\"old line\"}\n"), 0o644); err != nil {
		t.Fatalf("写入历史日志失败：%v", err)
	}
	// 干扰项：命名不合法（不是日期）的文件不能出现在可查日期里。
	if err := os.WriteFile(filepath.Join(dir, "Vault-Agent-notaday.log"), []byte("x\n"), 0o644); err != nil {
		t.Fatalf("写入干扰文件失败：%v", err)
	}

	days := l.Days()
	want := []string{yesterday, time.Now().Format("2006-01-02")}
	if len(days) != len(want) || days[0] != want[0] || days[1] != want[1] {
		t.Fatalf("Days 应为 %v，实际 %v", want, days)
	}

	data, err := l.ReadDay(yesterday, 1<<20)
	if err != nil {
		t.Fatalf("ReadDay(昨天) 失败：%v", err)
	}
	if !strings.Contains(string(data), "old line") {
		t.Fatalf("昨天日志内容不对：%s", string(data))
	}

	// 非法日期必须被拒，且不能把它当路径拼出去。
	if path := l.PathForDay("../evil"); path != "" {
		t.Fatalf("非法日期的 PathForDay 应为空，实际 %q", path)
	}
	if _, err := l.ReadDay("../evil", 1024); err == nil {
		t.Fatal("非法日期的 ReadDay 应报错")
	}
}

// TestReadDayWithoutDaily 未启用文件日志时，PathForDay 为空、ReadDay 报 not exist、Days 为空。
func TestReadDayWithoutDaily(t *testing.T) {
	l, err := New(Options{AlsoConsole: true})
	if err != nil {
		t.Fatalf("构造 Logger 失败：%v", err)
	}
	defer l.Close()
	if path := l.PathForDay(""); path != "" {
		t.Fatalf("未启用文件日志时 PathForDay 应为空，实际 %q", path)
	}
	if days := l.Days(); len(days) != 0 {
		t.Fatalf("未启用文件日志时 Days 应为空，实际 %v", days)
	}
	if _, err := l.ReadDay("", 1024); err == nil {
		t.Fatal("未启用文件日志时 ReadDay 应报错")
	}
}

// TestLogEventCatalogIsWellFormed 防止日志事件表出现重复 ID / 重复旧文案。
// 重复会让某个事件被另一个事件覆盖，线上表现为"消息翻错"或直接消失。
func TestLogEventCatalogIsWellFormed(t *testing.T) {
	ids := map[string]bool{}
	sources := map[string]string{}
	for _, ev := range logEvents {
		if ev.ID == "" || ev.English == "" {
			t.Fatalf("日志事件缺少 ID 或英文模板：%+v", ev)
		}
		if ids[ev.ID] {
			t.Fatalf("日志事件 ID 重复：%s", ev.ID)
		}
		ids[ev.ID] = true
		for _, match := range ev.Match {
			if match == "" {
				t.Fatalf("事件 %s 含空匹配文案", ev.ID)
			}
			if prev, ok := sources[match]; ok && prev != ev.ID {
				t.Fatalf("旧文案同时匹配两个事件：%q → %s / %s", match, prev, ev.ID)
			}
			sources[match] = ev.ID
		}
	}
}

// TestLocalizeHandler 钉住日志收敛行为：
//   - 收录消息输出英文模板 + msg_key；
//   - 未收录 Info 默认不落盘；
//   - 未收录 Warn/Error 收敛为兜底事件（不把无法翻译的原文写进普通日志）。
func TestLocalizeHandler(t *testing.T) {
	var buf bytes.Buffer
	inner := slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})
	logger := slog.New(newLocalizeHandler(inner, false))

	logger.With("type", "repo_prepare").Error("任务失败且已达最大尝试次数", "job_id", "j1")
	logger.Info("一条未映射的日志内容")
	logger.Warn("一条未映射的警告内容")
	logger.Error("一条未映射的错误内容")

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 3 {
		t.Fatalf("应只落盘 3 条（任务失败 + 兜底警告 + 兜底错误），实际 %d 条：\n%s", len(lines), buf.String())
	}

	type entry struct {
		Level  string `json:"level"`
		Msg    string `json:"msg"`
		MsgKey string `json:"msg_key"`
		Detail string `json:"detail"`
		Type   string `json:"type"`
	}
	parse := func(line string) entry {
		var e entry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("日志行不是合法 JSON：%v\n%s", err, line)
		}
		return e
	}

	first := parse(lines[0])
	if first.Msg != "Job failed: repo_prepare" || first.MsgKey != "job.failed" {
		t.Fatalf("任务失败日志应输出英文模板与 msg_key，实际 msg=%q msg_key=%q", first.Msg, first.MsgKey)
	}

	warn := parse(lines[1])
	if warn.Msg != "Unexpected warning" || warn.MsgKey != "system.warning" {
		t.Fatalf("未收录警告应收敛为 system.warning，实际 %+v", warn)
	}
	if warn.Detail != "unmapped log event" {
		t.Fatalf("未收录警告应附带英文 detail，实际 %q", warn.Detail)
	}

	failure := parse(lines[2])
	if failure.Msg != "Unexpected error" || failure.MsgKey != "system.error" {
		t.Fatalf("未收录错误应收敛为 system.error，实际 %+v", failure)
	}
	if failure.Detail != "unmapped log event" {
		t.Fatalf("未收录错误应附带英文 detail，实际 %q", failure.Detail)
	}
}

// TestLocalizeHandlerKeepsUnknownAtDebug 明确 debug 是排查逃生口：未收录消息原样保留。
func TestLocalizeHandlerKeepsUnknownAtDebug(t *testing.T) {
	var buf bytes.Buffer
	inner := slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	logger := slog.New(newLocalizeHandler(inner, true))
	logger.Debug("底层调试日志", "k", "v")
	if !strings.Contains(buf.String(), "底层调试日志") {
		t.Fatalf("debug 级别应保留未收录日志：%s", buf.String())
	}
}

// TestFallbackDetailIsEnglish 钉住 detail 的英文兜底口径：
// 优先底层错误码，其次字段摘要；不把原中文文案写进 detail。
func TestFallbackDetailIsEnglish(t *testing.T) {
	if got := fallbackDetail(map[string]string{"error.code": "agent.mount_failed"}, ""); got != "agent.mount_failed" {
		t.Fatalf("应优先使用稳定错误码，实际 %q", got)
	}
	if got := fallbackDetail(map[string]string{"repo_id": "r1", "stage": "mount"}, ""); got != "repo_id=r1 stage=mount" {
		t.Fatalf("无错误码时应给出英文 key=value 摘要，实际 %q", got)
	}
	if got := fallbackDetail(nil, ""); got != "unmapped log event" {
		t.Fatalf("无字段时应给出英文兜底说明，实际 %q", got)
	}
	if got := fallbackDetail(nil, "cache: l2 read failed"); got != "cache: l2 read failed" {
		t.Fatalf("原本就是英文的底层日志应原样作为 detail，实际 %q", got)
	}
	if got := fallbackDetail(nil, "写入本地状态失败"); got != "failed to write local state" {
		t.Fatalf("中文日志应转成可读英文 detail，实际 %q", got)
	}
	if got := englishErrorDetail(map[string]string{"error.cause": "access denied", "error.code": "agent.mount_failed"}); got != "access denied" {
		t.Fatalf("英文 cause 应优先，实际 %q", got)
	}
	if got := englishErrorDetail(map[string]string{"error.cause": "拒绝访问", "error.code": "agent.mount_failed"}); got != "agent.mount_failed" {
		t.Fatalf("非英文 cause 应退回英文错误码，实际 %q", got)
	}
}
