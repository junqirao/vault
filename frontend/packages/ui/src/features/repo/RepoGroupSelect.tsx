import { useMemo } from 'react'
import { AutoComplete, Input } from 'antd'
import { useQuery } from '@tanstack/react-query'

import { useApi } from '../../api/provider'
import { useAuth } from '../../hooks/useAuth'
import { useI18n } from '../../i18n'

export interface RepoGroupSelectProps {
  value?: string
  onChange?: (value: string) => void
  disabled?: boolean
}

/**
 * 存储库分组选择：可从**已有分组**里选，也可以直接输入新分组名。
 *
 * 分组当前只是 `repo.meta.group` 这个自由文本字段，服务端没有独立的分组表；
 * 因此选项直接从现有存储库列表里去重得出，输入新值就等于创建一个新分组。
 */
export function RepoGroupSelect({ value, onChange, disabled }: RepoGroupSelectProps): JSX.Element {
  const api = useApi()
  const { t } = useI18n()
  const { isSuperAdmin } = useAuth()
  const reposQuery = useQuery({
    queryKey: ['repos', 'groups'],
    queryFn: () => api.listRepos({ limit: 200 }),
    staleTime: 60_000
  })
  // 服务端分组实体：管理员能看到尚未挂任何存储库的新分组（例如刚创建的 Steam 组）。
  // 普通用户没有该接口权限，继续从自己可见的存储库里推导组名即可。
  const groupsQuery = useQuery({
    queryKey: ['repo-groups', 'for-select'],
    queryFn: () => api.listRepoGroups(),
    staleTime: 60_000,
    retry: false,
    enabled: isSuperAdmin
  })

  const options = useMemo(() => {
    const seen = new Set<string>()
    for (const group of groupsQuery.data?.items ?? []) {
      const name = (group.name ?? '').trim()
      if (name) seen.add(name)
    }
    for (const repo of reposQuery.data?.items ?? []) {
      const group = (repo.group ?? '').trim()
      if (group) seen.add(group)
    }
    return Array.from(seen)
      .sort((a, b) => a.localeCompare(b))
      .map((group) => ({ value: group, label: group }))
  }, [groupsQuery.data, reposQuery.data])

  return (
    <AutoComplete
      value={value}
      onChange={(next) => onChange?.(next ?? '')}
      options={options}
      disabled={disabled}
      placeholder={t('repo.group.placeholder')}
      // 允许自由输入新分组名；已有的直接点选。
      filterOption={(input, option) =>
        String(option?.value ?? '')
          .toLowerCase()
          .includes(input.toLowerCase())
      }
    >
      <Input maxLength={64} />
    </AutoComplete>
  )
}
