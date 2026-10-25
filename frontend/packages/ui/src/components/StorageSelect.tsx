import { useMemo, type CSSProperties, type ReactNode } from 'react'
import { Select } from 'antd'

import type { StorageDTO } from '../api/types'
import { useI18n } from '../i18n'
import { fontSize, palette, spacing } from '../tokens/palette'
import { formatBytes } from '../utils/format'
import { storageUsage } from '../utils/storage'
import { StorageOption } from './StorageOption'

export interface StorageSelectProps {
  /** 可选项（调用方负责过滤启用/挂载状态，以及排除当前存储）。 */
  storages: StorageDTO[]
  value?: string
  onChange?: (value: string | undefined) => void
  placeholder?: string
  allowClear?: boolean
  loading?: boolean
  disabled?: boolean
  style?: CSSProperties
  /** Form.Item 注入的 id（用于 label 关联）。 */
  id?: string
}

/**
 * 统一的"选择存储"下拉。
 *
 * 建库与迁移共用同一套交互：展开项显示名称 / 路径 / 容量进度条（StorageOption），
 * 收起态显示名称 + 可用空间。调用方只传存储列表与受控值，不再各写一遍 optionRender。
 */
export function StorageSelect({
  storages,
  value,
  onChange,
  placeholder,
  allowClear,
  loading,
  disabled,
  style,
  id
}: StorageSelectProps): JSX.Element {
  const { t } = useI18n()
  const storageById = useMemo(() => new Map(storages.map((storage) => [storage.id, storage])), [storages])

  return (
    <Select<string>
      id={id}
      value={value}
      onChange={(next) => onChange?.(next)}
      allowClear={allowClear}
      loading={loading}
      disabled={disabled}
      placeholder={placeholder}
      style={style}
      options={storages.map((storage) => ({ value: storage.id, label: storage.name }))}
      // 展开项：名称 / 路径 / 容量进度条三排（容量条上的百分比与配色见 StorageOption）。
      optionRender={(option) => {
        const storage = storageById.get(String(option.value))
        if (!storage) return option.label
        const { usedBytes, totalBytes } = storageUsage(storage)
        return (
          <StorageOption
            name={storage.name}
            path={storage.path}
            usedBytes={usedBytes}
            totalBytes={totalBytes}
          />
        )
      }}
      // 收起态保持一行；选中项已被删除/停用时退回原始 id，避免显示成空框。
      labelRender={({ value: selectedValue }) => renderSelected(selectedValue, storageById, t)}
    />
  )
}

function renderSelected(
  value: unknown,
  storageById: Map<string, StorageDTO>,
  t: (key: string, args?: Record<string, unknown>) => string
): ReactNode {
  if (value === undefined || value === null || value === '') return null
  const storage = storageById.get(String(value))
  if (!storage) return String(value)
  return (
    <span>
      {storage.name}
      <span
        style={{
          marginInlineStart: spacing.xs,
          color: palette.textTertiary,
          fontSize: fontSize.xs
        }}
      >
        {`${t('storage.available')} ${formatBytes(storage.free_bytes)}`}
      </span>
    </span>
  )
}
