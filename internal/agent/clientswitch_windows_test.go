package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestValidateClientSwitchOptions 锁定切换器的入参校验。
//
// 切换器是"旧客户端已经退出之后唯一还在的东西"：它拿到错误的 target 就等于用户
// 既没有旧界面也没有新界面。所以参数必须在启动任何进程之前就被拒绝。
func TestValidateClientSwitchOptions(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "Vault.exe")
	if err := os.WriteFile(exe, []byte("stub"), 0o644); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name    string
		opt     ClientSwitchOptions
		wantErr bool
	}{
		{name: "合法", opt: ClientSwitchOptions{TargetExe: exe, Timeout: time.Second}},
		{name: "空 target", opt: ClientSwitchOptions{Timeout: time.Second}, wantErr: true},
		{name: "相对路径", opt: ClientSwitchOptions{TargetExe: "Vault.exe", Timeout: time.Second}, wantErr: true},
		{name: "不存在", opt: ClientSwitchOptions{TargetExe: filepath.Join(dir, "nope.exe"), Timeout: time.Second}, wantErr: true},
		{name: "指向目录", opt: ClientSwitchOptions{TargetExe: dir, Timeout: time.Second}, wantErr: true},
		{name: "超时为 0", opt: ClientSwitchOptions{TargetExe: exe}, wantErr: true},
		{name: "回退路径是相对路径", opt: ClientSwitchOptions{TargetExe: exe, FallbackExe: "old.exe", Timeout: time.Second}, wantErr: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := validateClientSwitchOptions(c.opt)
			if c.wantErr && err == nil {
				t.Fatalf("期望报错，实际通过")
			}
			if !c.wantErr && err != nil {
				t.Fatalf("期望通过，实际报错: %v", err)
			}
		})
	}
}

// TestRunClientSwitchTimesOutWhenClientNeverExits 锁定"旧客户端不退出就绝不启动新进程"。
//
// 这里用测试进程自己的 PID 当"旧客户端"（它当然不会在 200ms 内退出）：切换器必须
// 超时放弃，而不是在旧进程还在的时候启动第二个客户端（那会抢单实例锁，回到闪屏）。
func TestRunClientSwitchTimesOutWhenClientNeverExits(t *testing.T) {
	t.Setenv("ProgramData", t.TempDir())
	target, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	err = RunClientSwitch(context.Background(), ClientSwitchOptions{
		ClientPID: os.Getpid(),
		TargetExe: target,
		Timeout:   200 * time.Millisecond,
	}, nil)
	if err == nil {
		t.Fatal("期望超时错误，实际成功启动了目标进程")
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("超时控制失效，耗时 %s", elapsed)
	}
}

// TestClientSwitchLogPath 锁定日志落在 %ProgramData%\Vault\update 下。
func TestClientSwitchLogPath(t *testing.T) {
	pd := t.TempDir()
	t.Setenv("ProgramData", pd)
	got := clientSwitchLogPath(`C:\Vault\Vault.exe`)
	want := filepath.Join(pd, "Vault", updateDirName, clientSwitchLogName)
	if !strings.EqualFold(got, want) {
		t.Fatalf("日志路径 = %q，期望 %q", got, want)
	}
	if !strings.Contains(filepath.ToSlash(got), "/update/") {
		t.Fatalf("日志路径应位于 update 目录下: %q", got)
	}
}
