import { RepoList } from './RepoList'

export interface MyReposProps {
  onCreate: () => void
  onOpen: (repoId: string) => void
  /** 卡片「设置」图标：跳到该库详情的「设置」页签（不是弹窗）。 */
  onOpenSettings?: (repoId: string) => void
  /** 点击顶部「性能」模块进入性能详情页。 */
  onOpenPerformance?: () => void
  /** 打开「上传任务」页：本机正在上传的库混在列表里（状态标签为「上传中」），点它进这个页面。 */
  onOpenUpload?: (uploadId: string) => void
  /** 首页「存储」卡片标题后的箭头：进入只读存储页。 */
  onOpenStorages?: () => void
  /** 首页「我的配额」标题后的箭头（仅超级管理员可见）：进入用户管理页。 */
  onOpenUsers?: () => void
}

/** 用户功能：我的存储库（仅本人拥有或可访问的库，由服务端过滤）。 */
export function MyRepos({
  onCreate,
  onOpen,
  onOpenSettings,
  onOpenPerformance,
  onOpenUpload,
  onOpenStorages,
  onOpenUsers
}: MyReposProps): JSX.Element {
  return (
    <RepoList
      isSuperAdmin={false}
      onCreate={onCreate}
      onOpen={onOpen}
      onOpenSettings={onOpenSettings}
      onOpenPerformance={onOpenPerformance}
      onOpenUpload={onOpenUpload}
      onOpenStorages={onOpenStorages}
      onOpenUsers={onOpenUsers}
    />
  )
}
