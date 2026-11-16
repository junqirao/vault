import { useEffect, useMemo, useState } from 'react'
import type { ReactNode } from 'react'
import { Button, Card, Form, Input, InputNumber, Modal, Skeleton, Space, Switch, Tag, Typography } from 'antd'
import { DatabaseOutlined, FolderOutlined } from '@ant-design/icons'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'

import { ConfirmDialog } from '../../components/ConfirmDialog'
import { EmptyState } from '../../components/EmptyState'
import { ErrorNotice } from '../../components/ErrorNotice'
import { MediaTypeTag } from '../../components/MediaTypeTag'
import { PageShell } from '../../components/PageShell'
import { UsageBar } from '../../components/UsageBar'
import { useApi } from '../../api/provider'
import type { StorageDTO } from '../../api/types'
import { useI18n } from '../../i18n'
import { fontSize, palette, spacing } from '../../tokens/palette'
import { formatBytes } from '../../utils/format'
import { StoragePoolsPanel } from './StoragePoolsPanel'
import { storageUsage } from './storageUsageMetrics'

const BYTES_PER_GB = 1024 ** 3

/** 编辑表单值：只有这三个字段可改（挂载点对卷承载的存储是锁定项）。 */
interface StorageEditValues {
  name: string
  enabled: boolean
  mountPoint?: string
}

/** 按卷去重后的容量汇总：同一卷上的多条存储只计一条容量。 */
interface VolumeSummary {
  key: string
  volumeName: string
  mediaType: StorageDTO['media_type']
  freeBytes: number
  totalBytes: number
  /** 本应用在这块盘上真实占用的字节（卷承载的存储按整卷已用算，目录存储按目录内磁盘汇总）。 */
  appUsedBytes: number
  count: number
}

/** 是否由底层卷承载（thin / lv）；目录模式（Windows、或历史存储）为 false。 */
function isVolumeBacked(storage: StorageDTO): boolean {
  return storage.kind === 'thin' || storage.kind === 'lv'
}

/**
 * 行尾的用量块：上方「已用 / 总量」，下方同一条 UsageBar。
 *
 * 与存储库列表行同一套排版：信息在左、用量与操作在右，多条存储的条能上下对齐着扫。
 */
function UsageMeter({
  used,
  total,
  percent,
  otherPercent,
  color,
  legend
}: {
  used: number
  total: number
  percent: number
  /** 非本应用占用的灰色段（见 UsageBar）。 */
  otherPercent?: number
  color?: string
  /** 条下方的图例（蓝 = 本应用 / 灰 = 其他占用）。 */
  legend?: JSX.Element
}): JSX.Element {
  return (
    <div style={{ flex: '0 0 auto', width: 276, minWidth: 0 }}>
      <div style={{ display: 'flex', justifyContent: 'flex-end', alignItems: 'baseline', gap: spacing.xs }}>
        <Typography.Text style={{ fontSize: fontSize.sm, fontVariantNumeric: 'tabular-nums' }}>
          {`${formatBytes(used)} / ${formatBytes(total)}`}
        </Typography.Text>
      </div>
      <UsageBar percent={percent} otherPercent={otherPercent} color={color} />
      {legend ?? null}
    </div>
  )
}

/** 行首的方块图标：与存储库卡片的图标同一尺寸/底色。 */
function RowIcon({ children }: { children: JSX.Element }): JSX.Element {
  return (
    <div
      style={{
        width: 32,
        height: 32,
        borderRadius: 8,
        background: palette.neutralFill,
        display: 'flex',
        alignItems: 'center',
        justifyContent: 'center',
        flexShrink: 0
      }}
    >
      {children}
    </div>
  )
}

/** 分区标题：与「存储使用情况」页同一套字号与留白（标题 + 右侧计数）。 */
function SectionHeading({ title, extra }: { title: string; extra?: JSX.Element }): JSX.Element {
  return (
    <div
      style={{
        display: 'flex',
        alignItems: 'baseline',
        justifyContent: 'space-between',
        gap: spacing.sm,
        marginBottom: spacing.sm
      }}
    >
      <Typography.Title level={5} style={{ margin: 0 }}>
        {title}
      </Typography.Title>
      {extra ?? null}
    </div>
  )
}

/** 图例小圆点：颜色 + 文案（蓝 = 本应用，灰 = 其他占用）。 */
function LegendDot({ color, children }: { color: string; children: ReactNode }): JSX.Element {
  return (
    <span style={{ display: 'inline-flex', alignItems: 'center', gap: 4 }}>
      <span style={{ width: 8, height: 8, borderRadius: 2, background: color }} />
      {children}
    </span>
  )
}

/** 按卷汇总的一行：左边卷名与存储数，右边整卷用量条（蓝 = 本应用，灰 = 其他占用）。 */
function VolumeUsageRow({ volume }: { volume: VolumeSummary }): JSX.Element {
  const { t } = useI18n()
  const used = Math.max(0, volume.totalBytes - volume.freeBytes)
  const appUsed = Math.min(used, Math.max(0, volume.appUsedBytes))
  const otherUsed = Math.max(0, used - appUsed)
  const percentOf = (bytes: number): number =>
    volume.totalBytes > 0 ? (bytes / volume.totalBytes) * 100 : 0
  const otherPercent = percentOf(otherUsed)
  // 不足 1%（四舍五入后）的灰色段不进图例/条：浮点误差会让"其他占用 0 B"冒出来。
  const showOther = Math.round(otherPercent) > 0
  return (
    <Card styles={{ body: { padding: `${spacing.sm}px ${spacing.md}px` } }}>
      <div style={{ display: 'flex', alignItems: 'center', gap: spacing.md, flexWrap: 'wrap' }}>
        <RowIcon>
          <DatabaseOutlined style={{ color: palette.accent, fontSize: 16 }} />
        </RowIcon>
        <div style={{ flex: '1 1 240px', minWidth: 0 }}>
          <div style={{ display: 'flex', alignItems: 'center', gap: spacing.xs, minWidth: 0 }}>
            <Typography.Text strong ellipsis={{ tooltip: volume.volumeName }} style={{ fontSize: fontSize.lg }}>
              {volume.volumeName}
            </Typography.Text>
            {volume.mediaType && volume.mediaType !== 'unknown' ? (
              <MediaTypeTag mediaType={volume.mediaType} />
            ) : null}
          </div>
          <Typography.Text
            type="secondary"
            ellipsis
            style={{ display: 'block', fontSize: fontSize.xs, marginTop: spacing.xxs }}
          >
            {t('storage.volumeStorages', { count: volume.count })} · {t('storage.usage.remaining')}{' '}
            {formatBytes(volume.freeBytes)}
          </Typography.Text>
        </div>
        <UsageMeter
          used={used}
          total={volume.totalBytes}
          percent={percentOf(appUsed)}
          otherPercent={showOther ? otherPercent : 0}
          color={palette.accent}
          legend={
            showOther ? (
              <div
                style={{
                  display: 'flex',
                  justifyContent: 'flex-end',
                  gap: spacing.sm,
                  marginTop: spacing.xxs,
                  fontSize: fontSize.xs,
                  color: palette.textSecondary
                }}
              >
                <LegendDot color={palette.accent}>
                  {t('storage.usage.appUsed')} {formatBytes(appUsed)}
                </LegendDot>
                <LegendDot color={palette.textTertiary}>
                  {t('storage.usage.otherUsed')} {formatBytes(otherUsed)}
                </LegendDot>
              </div>
            ) : undefined
          }
        />
      </div>
    </Card>
  )
}

/** 单块存储的一行：左边名称/标签/路径，右边用量条与管理动作。 */
function StorageRow({
  storage,
  kindLabel,
  toggling,
  mounting,
  onToggle,
  onEdit,
  onMount,
  onUnmount,
  onResize,
  onRemove
}: {
  storage: StorageDTO
  kindLabel: (kind: StorageDTO['kind']) => string
  toggling: boolean
  mounting: boolean
  onToggle: (checked: boolean) => void
  onEdit: () => void
  onMount: () => void
  onUnmount: () => void
  onResize: () => void
  onRemove: () => void
}): JSX.Element {
  const { t } = useI18n()
  const usage = storageUsage(storage)
  const meta = [storage.path, storage.file_system, `${t('storage.diskCount')} ${storage.disk_count}`]
    .filter(Boolean)
    .join(' · ')
  return (
    <Card styles={{ body: { padding: `${spacing.sm}px ${spacing.md}px` } }}>
      <div style={{ display: 'flex', alignItems: 'center', gap: spacing.md, flexWrap: 'wrap' }}>
        <RowIcon>
          <FolderOutlined style={{ color: palette.accent, fontSize: 16 }} />
        </RowIcon>

        <div style={{ flex: '1 1 260px', minWidth: 0 }}>
          <div style={{ display: 'flex', alignItems: 'center', gap: spacing.xs, flexWrap: 'wrap', minWidth: 0 }}>
            <Typography.Text
              strong
              ellipsis={{ tooltip: storage.name }}
              style={{ minWidth: 0, fontSize: fontSize.lg }}
            >
              {storage.name}
            </Typography.Text>
            <Tag style={{ marginInlineEnd: 0 }}>{kindLabel(storage.kind)}</Tag>
            {storage.media_type && storage.media_type !== 'unknown' ? (
              <MediaTypeTag mediaType={storage.media_type} />
            ) : null}
            {!storage.enabled ? <Tag style={{ marginInlineEnd: 0 }}>{t('storage.usage.disabled')}</Tag> : null}
            {isVolumeBacked(storage) ? (
              storage.mounted ? (
                <Tag color="green" style={{ marginInlineEnd: 0 }}>
                  {t('storage.mounted')}
                </Tag>
              ) : (
                <Tag color="red" style={{ marginInlineEnd: 0 }}>
                  {t('storage.notMounted')}
                </Tag>
              )
            ) : null}
          </div>
          <Typography.Text
            type="secondary"
            ellipsis={{ tooltip: meta }}
            style={{ display: 'block', fontSize: fontSize.xs, marginTop: spacing.xxs }}
          >
            {meta}
          </Typography.Text>
        </div>

        <UsageMeter used={usage.used} total={usage.total} percent={usage.percent} />

        {/* 操作全部收在右侧：启停 / 编辑 / 挂载切换 / 扩容 / 移除。 */}
        <div style={{ display: 'flex', alignItems: 'center', gap: spacing.xxs, flexWrap: 'wrap', justifyContent: 'flex-end' }}>
          <Switch
            size="small"
            checked={storage.enabled}
            loading={toggling}
            onChange={onToggle}
          />
          <Button type="link" size="small" onClick={onEdit}>
            {t('common.edit')}
          </Button>
          {isVolumeBacked(storage) ? (
            storage.mounted ? (
              <Button type="link" size="small" onClick={onUnmount}>
                {t('storage.unmount')}
              </Button>
            ) : (
              <Button type="link" size="small" loading={mounting} onClick={onMount}>
                {t('storage.mount')}
              </Button>
            )
          ) : null}
          {storage.kind === 'thin' ? (
            <Button type="link" size="small" onClick={onResize}>
              {t('storage.resize')}
            </Button>
          ) : null}
          <Button type="link" size="small" danger onClick={onRemove}>
            {t('storage.remove')}
          </Button>
        </div>
      </div>
    </Card>
  )
}

/**
 * 存储管理（管理员）：列表 / 编辑 / 启停 / 挂载 / 卸载 / 扩容 / 移除记录。
 *
 * 版式与「存储库」列表一致：一行一个存储，信息在左、用量条与管理动作在右；
 * 上方先给「按卷汇总」——同一块物理卷上的多个存储只算一份容量，避免把卷容量重复累加。
 *
 * 创建不在本页：逻辑链是「选/建存储池 → 设定存储 → 存储库」，内联建池还要带块设备表格，
 * 弹窗装不下也看不清，所以独立成创建页（StorageCreateForm），保存后跳回本页。
 */
export function StorageList({ onCreate }: { onCreate: () => void }): JSX.Element {
  const api = useApi()
  const { t } = useI18n()
  const queryClient = useQueryClient()
  const [form] = Form.useForm<StorageEditValues>()
  const [resizeForm] = Form.useForm<{ sizeGB: number }>()
  const [error, setError] = useState<unknown>(null)
  const [formOpen, setFormOpen] = useState(false)
  const [editing, setEditing] = useState<StorageDTO | null>(null)
  const [pendingDelete, setPendingDelete] = useState<StorageDTO | null>(null)
  const [pendingUnmount, setPendingUnmount] = useState<StorageDTO | null>(null)
  const [resizeTarget, setResizeTarget] = useState<StorageDTO | null>(null)
  // 「存储池管理」面板：受控弹窗而不是新路由——管理池是列表页的顺手操作，开路由会把列表状态丢掉。
  const [poolsOpen, setPoolsOpen] = useState(false)

  const storagesQuery = useQuery({ queryKey: ['storages', 'list'], queryFn: () => api.listStorages() })
  const storages = storagesQuery.data?.items ?? []

  // 能力探测：lvm=true（Linux）才有卷组/存储池这回事，否则连入口都不该出现。
  // 与 StorageCreateForm 共用 ['system-info'] 缓存，不会多发一次请求。
  const infoQuery = useQuery({ queryKey: ['system-info'], queryFn: () => api.systemInfo() })
  const supportsVolumes = Boolean(infoQuery.data?.capabilities.lvm)

  const invalidate = (): void => {
    void queryClient.invalidateQueries({ queryKey: ['storages'] })
  }

  useEffect(() => {
    if (!formOpen || !editing) return
    setError(null)
    form.setFieldsValue({ name: editing.name, mountPoint: editing.path, enabled: editing.enabled })
  }, [editing, form, formOpen])

  const saveMutation = useMutation({
    mutationFn: async (values: StorageEditValues) => {
      // 弹窗只在编辑态打开，editing 不该为空；用窄化而不是断言，避免类型逃逸。
      if (!editing) throw new Error('storage not selected')
      const patch: { name: string; enabled: boolean; path?: string } = {
        name: values.name,
        enabled: values.enabled
      }
      // 绑定底层卷的存储其"挂载点即路径"，改路径会让卷与目录脱钩，后端会拒绝，这里直接不发。
      const mountPoint = (values.mountPoint ?? '').trim()
      if (mountPoint && mountPoint !== editing.path && !isVolumeBacked(editing)) {
        patch.path = mountPoint
      }
      return api.updateStorage(editing.id, patch)
    },
    onSuccess: () => {
      setError(null)
      setFormOpen(false)
      invalidate()
    },
    onError: (err) => setError(err)
  })

  const toggleMutation = useMutation({
    mutationFn: ({ id, enabled }: { id: string; enabled: boolean }) => api.updateStorage(id, { enabled }),
    onSuccess: () => {
      setError(null)
      invalidate()
    },
    onError: (err) => setError(err)
  })

  const deleteMutation = useMutation({
    mutationFn: (id: string) => api.deleteStorage(id),
    onSuccess: () => {
      setError(null)
      setPendingDelete(null)
      invalidate()
    },
    onError: (err) => setError(err)
  })

  const mountMutation = useMutation({
    mutationFn: (id: string) => api.mountStorage(id),
    onSuccess: () => {
      setError(null)
      invalidate()
    },
    onError: (err) => setError(err)
  })

  // 卸载保留确认框（失败时留在框内展示原因，如"有进行中的上传"）。
  const unmountMutation = useMutation({
    mutationFn: (id: string) => api.unmountStorage(id),
    onSuccess: () => {
      setError(null)
      setPendingUnmount(null)
      invalidate()
    },
    onError: (err) => setError(err)
  })

  const resizeMutation = useMutation({
    mutationFn: ({ id, sizeBytes }: { id: string; sizeBytes: number }) => api.resizeStorage(id, sizeBytes),
    onSuccess: () => {
      setError(null)
      setResizeTarget(null)
      invalidate()
    },
    onError: (err) => setError(err)
  })

  const volumeSummaries = useMemo<VolumeSummary[]>(() => {
    const map = new Map<string, VolumeSummary>()
    for (const storage of storages) {
      const key = storage.volume_name || storage.path
      // 卷承载的存储：整卷都是本应用的，已用 = 卷已用；目录存储：目录内磁盘汇总才是本应用占用。
      const appBytes = isVolumeBacked(storage)
        ? Math.max(0, storage.total_bytes - storage.free_bytes)
        : storage.used_bytes
      const existing = map.get(key)
      if (existing) {
        existing.count += 1
        existing.appUsedBytes += appBytes
      } else {
        map.set(key, {
          key,
          volumeName: storage.volume_name || '-',
          mediaType: storage.media_type,
          freeBytes: storage.free_bytes,
          totalBytes: storage.total_bytes,
          appUsedBytes: appBytes,
          count: 1
        })
      }
    }
    return [...map.values()]
  }, [storages])

  const kindLabel = (kind: StorageDTO['kind']): string => {
    if (kind === 'thin') return t('storage.kind.thin')
    if (kind === 'lv') return t('storage.kind.lv')
    return t('storage.kind.dir')
  }

  const openResize = (storage: StorageDTO): void => {
    setError(null)
    setResizeTarget(storage)
    resizeForm.setFieldsValue({ sizeGB: Math.round((storage.size_bytes / BYTES_PER_GB) * 100) / 100 })
  }

  return (
    <PageShell
      title={t('storage.title')}
      subtitle={t('page.storages.subtitle')}
      extra={
        <>
          {supportsVolumes ? (
            <Button onClick={() => setPoolsOpen(true)}>{t('storage.pools.manage')}</Button>
          ) : null}
          <Button onClick={() => void storagesQuery.refetch()}>{t('common.refresh')}</Button>
          <Button type="primary" onClick={onCreate}>
            {t('storage.create')}
          </Button>
        </>
      }
    >
      {error ? <ErrorNotice error={error} /> : null}
      {storagesQuery.error ? <ErrorNotice error={storagesQuery.error} /> : null}

      <Space direction="vertical" size={spacing.lg} style={{ width: '100%' }}>
        {volumeSummaries.length > 0 ? (
          <div>
            <SectionHeading title={t('storage.volumeSummary')} />
            <Space direction="vertical" size={spacing.sm} style={{ width: '100%' }}>
              {volumeSummaries.map((volume) => (
                <VolumeUsageRow key={volume.key} volume={volume} />
              ))}
            </Space>
          </div>
        ) : null}

        <div>
          <SectionHeading
            title={t('storage.list.title')}
            extra={
              <Typography.Text type="secondary" style={{ fontSize: fontSize.xs }}>
                {t('common.total', { total: storages.length })}
              </Typography.Text>
            }
          />
          {storagesQuery.isLoading ? (
            <Skeleton active paragraph={{ rows: 4 }} />
          ) : storages.length === 0 ? (
            <EmptyState title={t('storage.empty')} />
          ) : (
            <Space direction="vertical" size={spacing.sm} style={{ width: '100%' }}>
              {storages.map((storage) => (
                <StorageRow
                  key={storage.id}
                  storage={storage}
                  kindLabel={kindLabel}
                  toggling={toggleMutation.isPending && toggleMutation.variables?.id === storage.id}
                  mounting={mountMutation.isPending && mountMutation.variables === storage.id}
                  onToggle={(checked) => {
                    setError(null)
                    toggleMutation.mutate({ id: storage.id, enabled: checked })
                  }}
                  onEdit={() => {
                    setEditing(storage)
                    setFormOpen(true)
                  }}
                  onMount={() => {
                    setError(null)
                    mountMutation.mutate(storage.id)
                  }}
                  onUnmount={() => {
                    setError(null)
                    setPendingUnmount(storage)
                  }}
                  onResize={() => openResize(storage)}
                  onRemove={() => {
                    setError(null)
                    setPendingDelete(storage)
                  }}
                />
              ))}
            </Space>
          )}
        </div>
      </Space>

      <Modal
        open={formOpen}
        title={t('storage.edit')}
        width={560}
        centered
        okText={t('common.save')}
        cancelText={t('common.cancel')}
        confirmLoading={saveMutation.isPending}
        onOk={() => {
          void form.validateFields().then((values) => {
            setError(null)
            saveMutation.mutate(values)
          })
        }}
        onCancel={() => setFormOpen(false)}
        destroyOnClose
        maskClosable={false}
      >
        {error ? <ErrorNotice error={error} /> : null}
        <Form form={form} layout="vertical">
          <Form.Item name="name" label={t('field.name')} rules={[{ required: true }]}>
            <Input maxLength={64} />
          </Form.Item>
          <Form.Item name="enabled" label={t('common.status')} valuePropName="checked">
            <Switch />
          </Form.Item>
          {editing ? (
            <Form.Item
              name="mountPoint"
              label={t('field.mountPoint')}
              extra={isVolumeBacked(editing) ? t('storage.pathLocked') : undefined}
            >
              <Input spellCheck={false} disabled={isVolumeBacked(editing)} />
            </Form.Item>
          ) : null}
        </Form>
      </Modal>

      <Modal
        open={resizeTarget !== null}
        title={t('storage.resize')}
        width={480}
        centered
        okText={t('common.save')}
        cancelText={t('common.cancel')}
        confirmLoading={resizeMutation.isPending}
        onOk={() => {
          void resizeForm.validateFields().then((values) => {
            if (!resizeTarget) return
            setError(null)
            resizeMutation.mutate({ id: resizeTarget.id, sizeBytes: Math.round(values.sizeGB * BYTES_PER_GB) })
          })
        }}
        onCancel={() => setResizeTarget(null)}
        destroyOnClose
        maskClosable={false}
      >
        {error ? <ErrorNotice error={error} /> : null}
        <Form form={resizeForm} layout="vertical">
          <Form.Item name="sizeGB" label={t('storage.sizeGb')} rules={[{ required: true }]}>
            <InputNumber min={0.1} precision={2} style={{ width: '100%' }} addonAfter="GB" />
          </Form.Item>
        </Form>
      </Modal>

      <ConfirmDialog
        open={pendingUnmount !== null}
        title={t('storage.unmount')}
        content={
          <>
            <div>{t('storage.unmountConfirm')}</div>
            {error ? (
              <div style={{ marginTop: spacing.sm }}>
                <ErrorNotice error={error} />
              </div>
            ) : null}
          </>
        }
        loading={unmountMutation.isPending}
        onConfirm={() => {
          if (!pendingUnmount) return
          setError(null)
          unmountMutation.mutate(pendingUnmount.id)
        }}
        onCancel={() => setPendingUnmount(null)}
      />

      <ConfirmDialog
        open={pendingDelete !== null}
        danger
        title={t('storage.remove')}
        content={
          <>
            <div>{t('storage.removeConfirm')}</div>
            {error ? (
              <div style={{ marginTop: spacing.sm }}>
                <ErrorNotice error={error} />
              </div>
            ) : null}
          </>
        }
        loading={deleteMutation.isPending}
        onConfirm={() => {
          if (!pendingDelete) return
          setError(null)
          deleteMutation.mutate(pendingDelete.id)
        }}
        onCancel={() => setPendingDelete(null)}
      />

      <StoragePoolsPanel open={poolsOpen} onClose={() => setPoolsOpen(false)} />
    </PageShell>
  )
}
