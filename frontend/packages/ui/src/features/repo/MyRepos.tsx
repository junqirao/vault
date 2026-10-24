import { RepoList } from './RepoList'

export interface MyReposProps {
  onCreate: () => void
  onOpen: (repoId: string) => void
  /** 打开「上传任务」页：本机正在上传的库混在列表里（状态标签为「上传中」），点它进这个页面。 */
  onOpenUpload?: (uploadId: string) => void
  /** 首页「存储」卡片：整卡进入只读存储页。 */
  onOpenStorages?: () => void
}

/** 用户功能：我的存储库（仅本人拥有或可访问的库，由服务端过滤）。 */
export function MyRepos({ onCreate, onOpen, onOpenUpload, onOpenStorages }: MyReposProps): JSX.Element {
  return (
    <RepoList
      isSuperAdmin={false}
      onCreate={onCreate}
      onOpen={onOpen}
      onOpenUpload={onOpenUpload}
      onOpenStorages={onOpenStorages}
    />
  )
}
