package app

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"vault/internal/domain"
	"vault/internal/platform"
)

// fakeUnmountVol 记录 UnmountVolume 的调用，并可指定失败。
type fakeUnmountVol struct {
	platform.VolumeBackend
	refs []string
	err  error
}

func (f *fakeUnmountVol) UnmountVolume(_ context.Context, ref, _ string) error {
	f.refs = append(f.refs, ref)
	return f.err
}

// TestDeleteReleasesLocalMountBeforeRemoving 锁定"本地还挂着的库也能删掉"。
//
// 现场：上传在收尾的"卸载母盘"之前失败（目录树校验不过），回滚把库置成 error，
// 但盘一直 attach 着。disks.mounted 是**服务端本地挂载**，不是客户端在用（那由租约判定），
// 旧实现却拿它拒绝删除，于是这条库既挂不了（状态不是 active）又删不掉。
//
// 现在的语义：删除前尽力卸载；卸载失败（句柄没释放）也不挡删除 —— 记录先删，
// 回收任务重试删除会失败并留痕，比让用户永远卡在一条异常库上要好。
func TestDeleteReleasesLocalMountBeforeRemoving(t *testing.T) {
	cases := []struct {
		name       string
		state      domain.RepoState
		unmountErr error
	}{
		{name: "error 残骸", state: domain.RepoStateError},
		{name: "importing 残骸", state: domain.RepoStateImporting},
		{name: "active 正常库", state: domain.RepoStateActive},
		{name: "卸载失败也照样删", state: domain.RepoStateError, unmountErr: errors.New("device busy")},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ctx := context.Background()
			st := openAppTestStore(t)
			repoID, _ := seedRepoWithOwner(t, st)
			if err := st.SetRepositoryState(ctx, repoID, c.state); err != nil {
				t.Fatalf("置存储库状态失败：%v", err)
			}
			disk := &domain.Disk{
				RepoID: repoID, Kind: domain.DiskKindParent,
				VHDXPath: filepath.Join("D:", "vault", "base.vhdx"), SizeBytes: 1 << 20,
				State: domain.DiskStateReady, Mounted: true,
			}
			if err := st.CreateDisk(ctx, disk); err != nil {
				t.Fatalf("建磁盘记录失败：%v", err)
			}

			vol := &fakeUnmountVol{err: c.unmountErr}
			svc := &RepoService{Deps: Deps{Store: st, Log: testLogger(), Vol: vol}}
			if err := svc.Delete(ctx, repoID); err != nil {
				t.Fatalf("删除应成功，实际失败：%v", err)
			}
			if len(vol.refs) != 1 || vol.refs[0] != disk.VHDXPath {
				t.Fatalf("应先卸载本地挂载，实际调用 %v", vol.refs)
			}
			if _, err := st.GetRepository(ctx, repoID); !isNotFound(err) {
				t.Fatalf("存储库记录应已删除，实际 err=%v", err)
			}
			if disks, err := st.ListDisksByRepo(ctx, repoID); err != nil || len(disks) != 0 {
				t.Fatalf("磁盘记录应已删除，实际 %d 条 err=%v", len(disks), err)
			}
		})
	}
}
