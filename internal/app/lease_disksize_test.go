package app

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"vault/internal/domain"
)

// TestAuthoritativeDiskSizeCorrectsImportedFileSizeGuess 锁定"挂载前自愈历史错误容量"。
//
// 真实事故：导入已有磁盘时没有 sidecar，早期版本把**文件大小**写成标称容量
// （198870176638 连 512 都不整除），客户端按容量精确匹配永远找不到盘，
// 库永久挂载失败。VHDX 头部里的虚拟大小才是权威值：挂载前必须改用它，
// 并把数据库纠正过来，否则每次挂载都要重新踩一遍。
func TestAuthoritativeDiskSizeCorrectsImportedFileSizeGuess(t *testing.T) {
	ctx := context.Background()
	st := openAppTestStore(t)
	repoID, _ := seedRepoWithOwner(t, st)

	const virtualSize = int64(198870176638)
	path := writeFakeVHDX(t, filepath.Join(t.TempDir(), "disks"), "base.vhdx", 1<<20, virtualSize)

	disk := &domain.Disk{
		RepoID:    repoID,
		Kind:      domain.DiskKindStandalone,
		VHDXPath:  path,
		SizeBytes: 1 << 20, // 导入时按文件大小猜出来的错误值
		State:     domain.DiskStateReady,
	}
	if err := st.CreateDisk(ctx, disk); err != nil {
		t.Fatalf("建磁盘记录失败：%v", err)
	}

	svc := &LeaseService{Deps: Deps{
		Store: st,
		Log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	}}
	if got := svc.authoritativeDiskSize(ctx, disk); got != virtualSize {
		t.Fatalf("下发的容量应为 VHDX 头部值 %d，实际 %d", virtualSize, got)
	}

	reloaded, err := st.GetDisk(ctx, disk.ID)
	if err != nil {
		t.Fatalf("读回磁盘失败：%v", err)
	}
	if reloaded.SizeBytes != virtualSize {
		t.Fatalf("数据库容量应被纠正为 %d，实际 %d", virtualSize, reloaded.SizeBytes)
	}
}

// TestAuthoritativeDiskSizeKeepsRecordWhenUnreadable 锁定降级路径：
// 读不到 VHDX 头部（不是 VHDX / 文件损坏 / Linux 设备路径）时必须沿用记录值，
// 容量纠正绝不能反过来阻断挂载。
func TestAuthoritativeDiskSizeKeepsRecordWhenUnreadable(t *testing.T) {
	ctx := context.Background()
	st := openAppTestStore(t)
	repoID, _ := seedRepoWithOwner(t, st)

	path := filepath.Join(t.TempDir(), "fake.vhdx")
	if err := os.WriteFile(path, []byte("not a vhdx"), 0o600); err != nil {
		t.Fatalf("写文件失败：%v", err)
	}
	const recorded = int64(64 << 30)
	disk := &domain.Disk{
		RepoID:    repoID,
		Kind:      domain.DiskKindStandalone,
		VHDXPath:  path,
		SizeBytes: recorded,
		State:     domain.DiskStateReady,
	}
	if err := st.CreateDisk(ctx, disk); err != nil {
		t.Fatalf("建磁盘记录失败：%v", err)
	}

	svc := &LeaseService{Deps: Deps{
		Store: st,
		Log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	}}
	if got := svc.authoritativeDiskSize(ctx, disk); got != recorded {
		t.Fatalf("读不到头部时应沿用记录值 %d，实际 %d", recorded, got)
	}

	reloaded, err := st.GetDisk(ctx, disk.ID)
	if err != nil {
		t.Fatalf("读回磁盘失败：%v", err)
	}
	if reloaded.SizeBytes != recorded {
		t.Fatalf("记录不应被改动，期望 %d，实际 %d", recorded, reloaded.SizeBytes)
	}
}
