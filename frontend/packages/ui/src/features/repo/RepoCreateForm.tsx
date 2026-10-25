import { useEffect, useMemo, useRef, useState } from 'react'
import { Button, Checkbox, Col, Form, Input, InputNumber, Progress, Row, Segmented, Select, Space, Typography } from 'antd'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'

import { ErrorNotice } from '../../components/ErrorNotice'
import { PageShell } from '../../components/PageShell'
import { SectionCard } from '../../components/SectionCard'
import { StorageSelect } from '../../components/StorageSelect'
import { agentApi } from '../../api/agentClient'
import type { StartUploadInput, UploadState, UploadStatus } from '../../api/agentTypes'
import { ApiError } from '../../api/errors'
import { useApi } from '../../api/provider'
import type { CreateRepoRequest, RepoDTO, RepoMode } from '../../api/types'
import { useAgent } from '../../hooks/useAgent'
import { useServerConfig } from '../../hooks/useServerConfig'
import { useI18n } from '../../i18n'
import { CAPACITY_UNIT, capacityNormalize, capacityValueProps } from '../../utils/format'
import { spacing } from '../../tokens/palette'
import { RepoGroupSelect } from './RepoGroupSelect'
import { SourceDirPicker, type SourceDirValue } from './SourceDirPicker'
import {
  UploadBuildStage,
  UploadCurrentFile,
  UploadMeta,
  UploadNotice,
  uploadPercent,
  uploadStageText,
  useUploadPrepare
} from './uploadView'

export interface RepoCreateFormProps {
  isSuperAdmin: boolean
  onCreated: (repo: RepoDTO) => void
  onCancel: () => void
  /** 本地目录上传完成后打开存储库（缺省时只提示成功并刷新列表）。 */
  onOpenRepo?: (repoId: string) => void
}

/** 表单值：源目录由 SourceDirPicker 提供结构化取值。 */
type RepoFormValues = Omit<CreateRepoRequest, 'source_dir'> & {
  source_dir?: SourceDirValue
  /** 是否在建库时预创建差异盘与 iSCSI 目标（仅共享库有意义）。 */
  preallocate?: boolean
}

/** 上传进行中的状态（可取消）。 */
const ACTIVE_UPLOAD_STATES: readonly UploadStatus[] = ['scanning', 'creating', 'uploading', 'completing']

/** 创建存储库表单页（超级管理员可指定所有者）。 */
export function RepoCreateForm({
  isSuperAdmin,
  onCreated,
  onCancel,
  onOpenRepo
}: RepoCreateFormProps): JSX.Element {
  const api = useApi()
  const { t } = useI18n()
  // 新建的库留在**当前活动**服务端上：本地目录上传必须带上归属台。
  const { active } = useServerConfig()
  const queryClient = useQueryClient()
  const [form] = Form.useForm<RepoFormValues>()
  // 独享库没有差异盘（整块盘直接给用户），服务端会把共享数量静默归零：
  // 与其让用户填一个不起作用的数字，不如当场置灰并说明原因。
  //
  // 用 onValuesChange 而不是 Form.useWatch：上传进行中表单会被进度面板替换掉，
  // 此时 useWatch 会去读一个"没有挂在任何 Form 上"的实例，开发模式会刷告警。
  const [exclusive, setExclusive] = useState(false)
  // 「预分配」勾选状态：勾上才把数量传给服务端；取消勾选等于共享数量 0
  // （差异盘在分配时派生、iSCSI 目标在挂载时现场下发）。默认不勾选，由用户显式开启。
  const [preallocate, setPreallocate] = useState(false)
  const [error, setError] = useState<unknown>(null)
  const [sourceDirValid, setSourceDirValid] = useState(true)
  const [upload, setUpload] = useState<UploadState | null>(null)
  const { uploads, refreshUploads } = useAgent()
  // 上一次**由源目录自动写入**的库名，用来判断"这个名字还是不是我填的"（见 autoFillName）。
  const autoNameRef = useRef('')

  const usersQuery = useQuery({
    queryKey: ['users', 'options'],
    queryFn: () => api.listUsers({ limit: 200 }),
    enabled: isSuperAdmin
  })

  const storagesQuery = useQuery({
    queryKey: ['storages', 'list'],
    queryFn: () => api.listStorages()
  })

  // 可选存储：只列启用的（与后端 CreateRepo 的校验一致）。
  const storages = useMemo(
    () => (storagesQuery.data?.items ?? []).filter((storage) => storage.enabled),
    [storagesQuery.data]
  )

  const mutation = useMutation({
    mutationFn: (values: CreateRepoRequest) => api.createRepo(values),
    onSuccess: (response) => {
      // 新建库立刻占用用户配额（「我的存储库」顶部的总账），列表页的 ['me'] 必须一起失效，
      // 否则用户建完库回到列表，看到的还是建库前的"已用"。
      void queryClient.invalidateQueries({ queryKey: ['me'] })
      onCreated(response.repo)
    },
    onError: (err) => setError(err)
  })

  const uploadMutation = useMutation({
    mutationFn: (input: StartUploadInput) => agentApi.startUpload(input),
    onSuccess: (response) => {
      setUpload(response.upload)
      void refreshUploads().catch(() => undefined)
    },
    onError: (err) => setError(err)
  })

  const cancelMutation = useMutation({
    mutationFn: (uploadId: string) => agentApi.cancelUpload(uploadId),
    onSuccess: () => {
      void refreshUploads().catch(() => undefined)
    },
    onError: (err) => setError(err)
  })

  // 页面挂载时恢复进行中的上传（用户中途离开又回来）。
  useEffect(() => {
    void refreshUploads().catch(() => undefined)
  }, [refreshUploads])

  // 会话尚未创建时 upload_id 为空（扫描/建会话阶段）：按 local_dir 跟踪同一任务，
  // 避免真实 upload_id 到达后回退到那条空 id 的陈旧占位条目（见 useAgent 的 upsertUpload）。
  const tracked = upload
    ? (upload.upload_id === ''
        ? uploads.find((item) => item.local_dir === upload.local_dir)
        : uploads.find((item) => item.upload_id === upload.upload_id)) ?? upload
    : null

  useEffect(() => {
    if (tracked) return
    const active = uploads.find((item) => ACTIVE_UPLOAD_STATES.includes(item.state))
    if (active) setUpload(active)
  }, [uploads, tracked])

  // 上传完成：刷新存储库列表（repo_id 缺失时这是唯一的成功反馈之一）。
  //
  // ['me'] 也要刷：上传建库是配额真正落账的时刻（库一建出来，那块盘就按标称容量计入已用），
  // 只刷列表会让顶部的"已用 / 总共可用"长期停在建库前的数字。
  useEffect(() => {
    if (tracked?.state !== 'done') return
    void queryClient.invalidateQueries({ queryKey: ['repos'] })
    void queryClient.invalidateQueries({ queryKey: ['me'] })
  }, [tracked?.state, queryClient])

  /**
   * 选了源目录就顺手把库名填上（真实反馈："从文件创建库会自动填写库名为目录名"）。
   *
   * 为什么该自动：拿一个目录建库的人，十有八九就是想把那个目录做成同名库，名字几乎总是
   * 最后一段目录名。还要他先选目录、再回头敲一遍同样的几个字，纯属白费 —— 而且顺序反过来
   * （先起名、后选目录）同样常见，所以两种顺序都得能自动补上。
   *
   * 为什么只在"名字是空的 / 还停在我上次自动填的那个值"时才写：用户手改过的名字是他的，
   * 换个目录就覆盖等于把他刚打的字吃掉。他一旦改过，这个名字就归他，此后换目录不再动它。
   */
  const autoFillName = (changed: Partial<RepoFormValues>): void => {
    if (!('source_dir' in changed)) return
    const picked = repoNameFromPath(changed.source_dir?.path ?? '')
    if (!picked) return
    const current = form.getFieldValue('name') as string | undefined
    if (current && current !== autoNameRef.current) return
    autoNameRef.current = picked
    form.setFieldValue('name', picked)
  }

  const submit = async (): Promise<void> => {
    const values = await form.validateFields()
    setError(null)
    const source = values.source_dir
    if (source?.kind === 'local' && source.path) {
      uploadMutation.mutate({
        local_dir: source.path,
        repo_name: values.name,
        repo_mode: values.mode,
        storage_id: values.storage_id || undefined,
        // 不传 quota_bytes：库没有"配额"，建库只需要**容量**（容量即它占用的配额）。
        //
        // 但预分配数量要传：它随上传会话的清单一起存下来，服务端在上传结束建库时写进库配置 ——
        // 不传的话这条路径建出来的库会丢掉用户勾选的预分配（服务端建库那条路径是直接建库、
        // 参数当场生效，不存在这个问题）。共享数量曾经就是这么丢的：无论填几，建出来的库
        // 都是配置默认值 50（真实反馈："设置共享数量大于0时会变成50"）。
        max_diff_disks: preallocateCount(values),
        preallocate: values.mode === 'shared' && values.preallocate !== false,
        // 分组同样要传：上传建库走的是另一条链路（agent → 上传会话 → 异步建库），
        // 漏传的话本机目录建出来的库分组恒为空（真实反馈："选了分组但是没有生效"）。
        repo_group: values.group,
        source_mode: 'copy',
        // 存储 ID 只对**它所属的那台**服务端有意义：多服务端下必须说清建到哪台。
        server_key: active?.key,
        server_url: active?.baseUrl
      })
      return
    }
    mutation.mutate(buildCreatePayload(values, source))
  }

  return (
    <PageShell
      title={t('repo.create.title')}
      extra={
        tracked ? (
          <Button onClick={onCancel}>{t('common.back')}</Button>
        ) : (
          <Space>
            <Button onClick={onCancel}>{t('common.cancel')}</Button>
            <Button
              type="primary"
              loading={mutation.isPending || uploadMutation.isPending}
              disabled={!sourceDirValid}
              onClick={() => void submit()}
            >
              {t('common.create')}
            </Button>
          </Space>
        )
      }
    >
      {error ? <ErrorNotice error={error} /> : null}
      {tracked ? (
        <SectionCard>
          <UploadPanel
            upload={tracked}
            canceling={cancelMutation.isPending}
            onCancel={() => cancelMutation.mutate(tracked.upload_id)}
            onOpenRepo={
              onOpenRepo && tracked.repo_id ? () => onOpenRepo(tracked.repo_id as string) : undefined
            }
          />
        </SectionCard>
      ) : (
        <Form
          form={form}
          layout="vertical"
          initialValues={{ mode: 'shared', max_diff_disks: 1, preallocate: false }}
          onValuesChange={(changed, values) => {
            setExclusive(values.mode === 'exclusive')
            setPreallocate(values.preallocate !== false)
            autoFillName(changed)
          }}
        >
          {/* 分三段，按用户真正的思考顺序排：先想清楚"建个什么库"（名字 / 类型 / 归属），
              再决定"放在哪、多大"（存储 / 容量 / 预分配），最后才是"要不要导入数据"。
              平铺成一片两列网格时，这三件事的边界全靠字段名猜 —— 分卡之后一眼能看出
              "哪些是必须填的、哪些是可选的、哪一块会影响提交路径"。 */}
          <SectionCard title={t('repo.create.section.basic')}>
            <Row gutter={16}>
              <Col span={12}>
                <Form.Item name="name" label={t('field.name')} rules={[{ required: true }]}>
                  <Input maxLength={64} />
                </Form.Item>
              </Col>
              <Col span={12}>
                <Form.Item
                  name="mode"
                  label={t('field.mode')}
                  rules={[{ required: true }]}
                  // 两种模式的差别不是"一个词"，而是一整句话：写在控件下方，比另加提示框省事。
                  extra={t(exclusive ? 'repo.create.modeHint.exclusive' : 'repo.create.modeHint.shared')}
                >
                  <Segmented<RepoMode>
                    block
                    options={[
                      { value: 'shared', label: t('repo.mode.shared') },
                      { value: 'exclusive', label: t('repo.mode.exclusive') }
                    ]}
                  />
                </Form.Item>
              </Col>
              <Col span={12}>
                <Form.Item name="group" label={t('field.group')}>
                  <RepoGroupSelect />
                </Form.Item>
              </Col>
              {isSuperAdmin ? (
                <Col span={12}>
                  <Form.Item name="owner_id" label={t('field.owner')}>
                    <Select
                      allowClear
                      loading={usersQuery.isLoading}
                      options={(usersQuery.data?.items ?? []).map((user) => ({
                        value: user.id,
                        label: `${user.username} (${user.role})`
                      }))}
                    />
                  </Form.Item>
                </Col>
              ) : null}
            </Row>
          </SectionCard>

          <SectionCard title={t('repo.create.section.storage')}>
            <Row gutter={16}>
              <Col span={12}>
                <Form.Item
                  name="storage_id"
                  label={t('field.storage')}
                  extra={storagesQuery.error ? t('storage.loadFailed') : undefined}
                >
                  <StorageSelect
                    allowClear
                    loading={storagesQuery.isLoading}
                    placeholder={t('storage.auto')}
                    storages={storages}
                  />
                </Form.Item>
              </Col>
              <Col span={12}>
                <Form.Item
                  name="size_bytes"
                  label={t('field.size')}
                  // 不需要这个提示了
                  // extra={t('repo.create.capacityHint')}
                  getValueProps={capacityValueProps}
                  normalize={capacityNormalize}
                >
                  <InputNumber min={0} addonAfter={CAPACITY_UNIT} style={{ width: '100%' }} />
                </Form.Item>
              </Col>
              {/*
                这里**不问配额**：库没有"配额"这个概念 —— 这里填的是**容量**（VHDX 标称容量），
                而这个容量就是它占用的配额（从创建者的配额里扣除）。配额本身是**用户级**的策略值，
                由管理员在用户表单里设置；库这边只需要给出容量。
              */}
              {/*
                「预分配」= 服务端的"共享数量 / max_diff_disks > 0"：勾选后建库时同时
                预创建差异盘与 iSCSI 目标（池位），等待分配；不勾选则分配时才派生差异盘、
                挂载时现场下发目标。独享库没有差异盘，整块盘直接交给用户，因此不显示。
                勾选框与它的数量是一条信息：勾上后数量紧跟其下，不再单独占一格。
              */}
              {!exclusive ? (
                <Col span={24}>
                  <Form.Item
                    name="preallocate"
                    valuePropName="checked"
                    extra={t('repo.create.preallocateHint')}
                    style={{ marginBottom: preallocate ? spacing.sm : 0 }}
                  >
                    <Checkbox>{t('field.preallocate')}</Checkbox>
                  </Form.Item>
                  {preallocate ? (
                    <Row gutter={16}>
                      <Col span={12}>
                        <Form.Item
                          name="max_diff_disks"
                          label={t('field.preallocateCount')}
                          rules={[{ required: true }]}
                          extra={t('repo.create.preallocateCountHint')}
                          style={{ marginBottom: 0 }}
                        >
                          <InputNumber min={1} style={{ width: '100%' }} />
                        </Form.Item>
                      </Col>
                    </Row>
                  ) : null}
                </Col>
              ) : null}
            </Row>
          </SectionCard>

          <SectionCard title={t('repo.create.section.source')}>
            <Form.Item
              name="source_dir"
              label={t('field.sourceDirOptional')}
              extra={t('repo.create.sourceHint')}
              style={{ marginBottom: 0 }}
            >
              <SourceDirPicker onValidityChange={setSourceDirValid} />
            </Form.Item>
          </SectionCard>
        </Form>
      )}
    </PageShell>
  )
}

/** 库名长度上限：与名称输入框的 maxLength 一致（服务端只要求非空，长度在这里把关）。 */
const REPO_NAME_MAX_LEN = 64

/**
 * 从源目录路径取库名：最后一段目录名，例如 `D:\data\photos` 与 `/srv/data/photos` 都得 `photos`。
 *
 * 同时兼容 Windows 反斜杠与 POSIX 斜杠（服务端目录与本地目录都可能出现在这一个输入里），
 * 并忽略尾随分隔符 —— 路径常是从地址栏或资源管理器里抄来的，尾巴上带不带 `\` 不由我们决定。
 * 取不出名字时返回空串（根路径 `D:\`、`/`、以及 `.` / `..` 这种没有名字的段），调用方据此不填。
 */
function repoNameFromPath(path: string): string {
  const parts = path.split(/[\\/]/).filter((part) => part !== '')
  const last = (parts[parts.length - 1] ?? '').trim()
  if (last === '.' || last === '..') return ''
  return last.length > REPO_NAME_MAX_LEN ? last.slice(0, REPO_NAME_MAX_LEN) : last
}

/** 组装服务端建库请求：仅服务端目录作为 source_dir 传给 /v1/repos。 */
function buildCreatePayload(values: RepoFormValues, source: SourceDirValue | undefined): CreateRepoRequest {
  const payload: CreateRepoRequest = {
    name: values.name,
    mode: values.mode,
    owner_id: values.owner_id,
    storage_id: values.storage_id,
    size_bytes: values.size_bytes,
    max_diff_disks: preallocateCount(values),
    preallocate: values.mode === 'shared' && values.preallocate !== false,
    group: values.group
  }
  if (source?.kind === 'server' && source.path) payload.source_dir = source.path
  return payload
}

/**
 * 计算要传给服务端的 max_diff_disks / 共享数量。
 *
 * 0 表示"不预分配"：独享库没有差异盘；共享库没勾选预分配时也传 0，服务端就只在
 * 分配时派生差异盘、挂载时现场下发目标。勾选但数量留空按 1 处理，避免"勾了却没生效"。
 */
function preallocateCount(values: RepoFormValues): number {
  if (values.mode !== 'shared' || values.preallocate === false) return 0
  const count = Number(values.max_diff_disks ?? 1)
  if (!Number.isFinite(count) || count <= 0) return 1
  return Math.trunc(count)
}

/** 本地目录上传进度面板（替代表单区域，不跳转页面）。 */
function UploadPanel({
  upload,
  canceling,
  onCancel,
  onOpenRepo
}: {
  upload: UploadState
  canceling: boolean
  onCancel: () => void
  onOpenRepo?: () => void
}): JSX.Element {
  const { t } = useI18n()
  const active = ACTIVE_UPLOAD_STATES.includes(upload.state)
  // 字节传完后的建库阶段（池位派生 / 目标下发）：百分比由服务端任务给出，
  // 阶段明细由库的 prepare 快照给出。
  const prepare = useUploadPrepare(upload)

  return (
    <Space direction="vertical" size={spacing.sm} style={{ width: '100%' }}>
      <Typography.Title level={5} style={{ margin: 0 }}>
        {t('upload.title')}
      </Typography.Title>
      <Typography.Text strong>{upload.repo_name}</Typography.Text>
      <Typography.Text type="secondary" style={{ wordBreak: 'break-all' }}>
        {upload.local_dir}
      </Typography.Text>
      <Progress
        percent={uploadPercent(upload)}
        status={upload.state === 'failed' ? 'exception' : active ? 'active' : 'normal'}
      />
      <Space size={spacing.sm} wrap>
        <Typography.Text type="secondary">{uploadStageText(t, upload.state)}</Typography.Text>
        {active ? (
          <Button type="link" size="small" danger loading={canceling} onClick={onCancel}>
            {t('upload.cancel')}
          </Button>
        ) : null}
      </Space>
      {/* 与「上传任务」页共用同一份元信息（已传/总量 · 速度 · 预计剩余 · 文件 · 分块）：
          新建页是"刚发起的任务"、任务页是"回头查看同一件事"，两处口径必须一致。 */}
      <UploadMeta upload={upload} />
      <UploadBuildStage upload={upload} prepare={prepare} />
      <UploadCurrentFile upload={upload} style={{ display: 'block' }} />
      <UploadNotice upload={upload} style={{ display: 'block' }} />
      {upload.state === 'done' ? (
        <Space size={spacing.sm} wrap>
          <Typography.Text type="success">{t('upload.done')}</Typography.Text>
          {onOpenRepo ? (
            <Button type="link" size="small" onClick={onOpenRepo}>
              {t('upload.openRepo')}
            </Button>
          ) : null}
        </Space>
      ) : null}
      {upload.state === 'canceled' ? (
        <Typography.Text type="secondary">{t('disk.copy.canceled')}</Typography.Text>
      ) : null}
      {upload.state === 'failed' ? (
        <ErrorNotice error={new ApiError({ kind: 'business', code: upload.error || 'system.internal' })} />
      ) : null}
    </Space>
  )
}
