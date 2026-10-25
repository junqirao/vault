package app

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"vault/internal/domain"
	"vault/internal/platform"
)

// fakeVolBackend 只实现挂载/卸载；其余方法由嵌入的接口兜底。
//
// 与 fakeDiskBackend 同样的理由：本测试只关心"盘还在不在、能不能挂上"，
// 补齐十几个空实现只会让真正要断言的三行逻辑淹没在样板里。
type fakeVolBackend struct {
	platform.VolumeBackend
	root     string
	mountErr error
	calls    int
}

func (f *fakeVolBackend) MountVolume(context.Context, string, string) (string, error) {
	f.calls++
	if f.mountErr != nil {
		return "", f.mountErr
	}
	return f.root, nil
}

func (f *fakeVolBackend) UnmountVolume(context.Context, string, string) error { return nil }

// newMountTestService 造一条"母盘已登记、但进程内没有任何挂载记录"的上传会话。
// mountRoot 是会话里记着的上一次挂载根，diskExists 表示盘文件现在还在不在。
func newMountTestService(t *testing.T, diskExists bool, mountRoot string) (*UploadService, *domain.Upload, *fakeVolBackend) {
	t.Helper()
	ctx := context.Background()
	st := openAppTestStore(t)
	user := &domain.User{Username: "mount-tester", Role: domain.RoleUser, Enabled: true}
	if err := st.CreateUser(ctx, user); err != nil {
		t.Fatalf("建用户失败：%v", err)
	}
	repoID := seedUploadRepo(t, st, user.ID, "Mount Target", domain.RepoStateCreating)
	disk := &domain.Disk{
		RepoID:    repoID,
		Kind:      domain.DiskKindParent,
		VHDXPath:  filepath.Join(t.TempDir(), "base.vhdx"),
		SizeBytes: 1 << 20,
		State:     domain.DiskStateReady,
	}
	if err := st.CreateDisk(ctx, disk); err != nil {
		t.Fatalf("建磁盘记录失败：%v", err)
	}
	u := &domain.Upload{
		UserID:     user.ID,
		RepoID:     repoID,
		RepoName:   "Mount Target",
		Mode:       domain.UploadModeCopy,
		TotalFiles: 1,
		DiskID:     disk.ID,
		MountRoot:  mountRoot,
		State:      domain.UploadStateOpen,
	}
	if err := st.CreateUpload(ctx, u); err != nil {
		t.Fatalf("建上传会话失败：%v", err)
	}
	vol := &fakeVolBackend{root: t.TempDir()}
	svc := &UploadService{Deps: Deps{
		Store: st,
		Log:   testLogger(),
		Disk:  fakeDiskBackend{exists: diskExists},
		Vol:   vol,
	}}
	return svc, u, vol
}

// TestEnsureUploadMountRemountsAfterUnmount 锁住真实工单的失败链：
//
// 分块记录齐全时 agent 会直接 POST /complete，整个请求里没有一次 PutChunk，
// 于是母盘从未被重新挂载；complete 拿 DB 里的旧挂载根去补建空文件，
// `mkdir "G:\\"` 报 "The system cannot find the path specified"，
// 表现就是"重启服务端后重试同名库必定失败"。
//
// 修复后这条路必须先重新 Attach 母盘，并把新盘符写回会话（盘符可能已经变了）。
func TestEnsureUploadMountRemountsAfterUnmount(t *testing.T) {
	ctx := context.Background()
	// 旧挂载根故意指向一个不存在的目录，模拟 Windows 已经把盘符回收。
	svc, u, vol := newMountTestService(t, true, filepath.Join(t.TempDir(), "gone-drive"))

	root, err := svc.ensureUploadMount(ctx, u)
	if err != nil {
		t.Fatalf("盘还在时必须能重新挂载：%v", err)
	}
	if vol.calls != 1 {
		t.Fatalf("应当重新 Attach 一次，实际调用 %d 次", vol.calls)
	}
	if root != vol.root {
		t.Fatalf("挂载根 = %q，期望 %q", root, vol.root)
	}
	if u.MountRoot != vol.root {
		t.Fatalf("会话对象未更新挂载根：%q", u.MountRoot)
	}
	fresh, err := svc.Store.GetUpload(ctx, u.ID)
	if err != nil {
		t.Fatalf("回查会话失败：%v", err)
	}
	if fresh.MountRoot != vol.root {
		t.Fatalf("新挂载根未写回会话：%q", fresh.MountRoot)
	}
}

// TestEnsureUploadMountRefusesStaleRootWhenDiskIsGone 锁住"盘没了"这一支：
//
// Attach 失败且盘文件也不在时，**绝不能**退回去用 DB 里的旧挂载根 —— 那个路径很可能
// 已经作为盘符分给了别的卷（Windows 盘符是全局资源），沿用等于把用户数据写进别人的盘。
func TestEnsureUploadMountRefusesStaleRootWhenDiskIsGone(t *testing.T) {
	ctx := context.Background()
	// 旧挂载根存在（现在属于别的卷），盘文件不存在。
	occupied := t.TempDir()
	svc, u, vol := newMountTestService(t, false, occupied)
	vol.mountErr = errors.New("attach: the file does not exist")

	if _, err := svc.ensureUploadMount(ctx, u); err == nil {
		t.Fatal("盘文件已不存在时不得沿用旧挂载根")
	}
	if u.MountRoot != occupied {
		t.Fatalf("失败时不应改写挂载根，实际 %q", u.MountRoot)
	}
}

// TestEnsureUploadMountKeepsRootWhenAttachIsRejected 锁住兼容分支：
//
// 进程重启后 Windows 仍可能保持着 Attach，再 Attach 会报 sharing violation；
// 此时盘文件还在、盘还是那块盘、盘符不会变，必须沿用旧根而不是把上传判失败。
func TestEnsureUploadMountKeepsRootWhenAttachIsRejected(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	svc, u, vol := newMountTestService(t, true, root)
	vol.mountErr = errors.New("attach: the process cannot access the file")

	got, err := svc.ensureUploadMount(ctx, u)
	if err != nil {
		t.Fatalf("盘仍被 Windows 占用时应沿用旧根：%v", err)
	}
	if got != root {
		t.Fatalf("挂载根 = %q，期望沿用 %q", got, root)
	}
	if u.MountRoot != root {
		t.Fatalf("沿用旧根时不应改写会话：%q", u.MountRoot)
	}
}
