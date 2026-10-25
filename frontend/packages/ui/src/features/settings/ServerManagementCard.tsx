import { useState } from 'react'
import { Alert, Button, Form, Input, Modal, Space, Switch, Table, Tag, Typography, message } from 'antd'
import type { ColumnsType } from 'antd/es/table'
import { DeleteOutlined, EditOutlined, LinkOutlined, PlusOutlined } from '@ant-design/icons'

import { ConfirmDialog } from '../../components/ConfirmDialog'
import { SectionCard } from '../../components/SectionCard'
import { agentApi, trustServerCertificate } from '../../api/agentClient'
import type { ServerProbe } from '../../api/agentTypes'
import { translateError } from '../../api/errors'
import { useServerConfig, type ServerEntry } from '../../hooks/useServerConfig'
import { useI18n } from '../../i18n'
import { fontSize, spacing } from '../../tokens/palette'
import { DEFAULT_SERVER_URL, isValidHttpUrl, normalizeBaseUrl } from '../../utils/validation'

interface ServerFormValues {
  baseUrl: string
  name?: string
  autoConnect: boolean
}

/**
 * 设置页的「服务器」模块：管理多服务端注册表。
 *
 * 能力：新增 / 编辑 / 删除 / 测试连接 / 自动连接开关。
 * 自动连接只影响**启动时**是否主动连这台；关闭的服务器仍保留在列表里，
 * 用户可以从顶栏切换器或启动时的服务端选择页手动进入。
 */
export function ServerManagementCard(): JSX.Element {
  const { t } = useI18n()
  const { servers, activeKey, addServer, updateServer, remove } = useServerConfig()
  const [form] = Form.useForm<ServerFormValues>()
  const [open, setOpen] = useState(false)
  const [editKey, setEditKey] = useState<string | null>(null)
  const [probe, setProbe] = useState<ServerProbe | null>(null)
  const [testedUrl, setTestedUrl] = useState('')
  const [testing, setTesting] = useState(false)
  const [saving, setSaving] = useState(false)
  const [formError, setFormError] = useState<string | null>(null)
  const [rowTesting, setRowTesting] = useState<string | null>(null)
  const [deleteTarget, setDeleteTarget] = useState<ServerEntry | null>(null)

  const openAdd = (): void => {
    setEditKey(null)
    setProbe(null)
    setTestedUrl('')
    setFormError(null)
    form.setFieldsValue({ baseUrl: DEFAULT_SERVER_URL, name: '', autoConnect: true })
    setOpen(true)
  }

  const openEdit = (server: ServerEntry): void => {
    setEditKey(server.key)
    setProbe(null)
    setTestedUrl('')
    setFormError(null)
    form.setFieldsValue({
      baseUrl: server.baseUrl,
      name: server.serverName,
      autoConnect: server.autoConnect !== false
    })
    setOpen(true)
  }

  const testConnection = async (): Promise<void> => {
    let values: ServerFormValues
    try {
      values = (await form.validateFields(['baseUrl'])) as ServerFormValues
    } catch {
      return
    }
    const url = normalizeBaseUrl(values.baseUrl)
    setTesting(true)
    setFormError(null)
    setProbe(null)
    setTestedUrl('')
    try {
      const result = await agentApi.testServer(url)
      const effective = result.server_url || url
      setProbe(result)
      setTestedUrl(effective)
      // 探测返回的才是实际生效地址（可能从 http 升级为 https），回填输入框。
      if (effective !== values.baseUrl) form.setFieldValue('baseUrl', effective)
      if (!form.getFieldValue('name') && result.server_name) form.setFieldValue('name', result.server_name)
    } catch (error) {
      setFormError(translateError(error))
    } finally {
      setTesting(false)
    }
  }

  const save = async (): Promise<void> => {
    let values: ServerFormValues
    try {
      values = await form.validateFields()
    } catch {
      return
    }
    const effective = normalizeBaseUrl(values.baseUrl)
    const original = editKey ? servers.find((server) => server.key === editKey) : null
    const urlChanged = !original || normalizeBaseUrl(original.baseUrl) !== effective
    const probeMatches = Boolean(probe && normalizeBaseUrl(probe.server_url || testedUrl) === effective)
    if (urlChanged && !probeMatches) {
      // 新地址必须先测试成功，否则拿不到实例 ID / 证书指纹，存下去也连不上。
      setFormError(t('settings.servers.testRequired'))
      return
    }

    setSaving(true)
    setFormError(null)
    try {
      const certSha256 = probeMatches ? probe?.cert?.sha256 : original?.certSha256
      if (certSha256) await trustServerCertificate(effective, certSha256)
      const entry: ServerEntry = {
        key: (probeMatches ? probe?.server_instance_id : undefined) || original?.key || effective,
        baseUrl: effective,
        serverName:
          (values.name ?? '').trim() ||
          (probeMatches ? probe?.server_name : undefined) ||
          original?.serverName ||
          '',
        instanceId: (probeMatches ? probe?.server_instance_id : undefined) || original?.instanceId,
        certSha256,
        autoConnect: values.autoConnect
      }
      if (original) {
        updateServer(original.key, entry)
      } else {
        addServer(entry)
      }
      setOpen(false)
      message.success(t('settings.servers.saved'))
    } catch (error) {
      setFormError(translateError(error))
    } finally {
      setSaving(false)
    }
  }

  const testExisting = async (server: ServerEntry): Promise<void> => {
    setRowTesting(server.key)
    try {
      const result = await agentApi.testServer(server.baseUrl)
      const effective = result.server_url || server.baseUrl
      if (result.cert?.sha256) await trustServerCertificate(effective, result.cert.sha256)
      updateServer(server.key, {
        baseUrl: effective,
        serverName: result.server_name || server.serverName,
        instanceId: result.server_instance_id || server.instanceId,
        certSha256: result.cert?.sha256 || server.certSha256
      })
      message.success(t('settings.servers.testOk', { name: result.server_name || server.serverName || effective }))
    } catch (error) {
      message.error(translateError(error))
    } finally {
      setRowTesting(null)
    }
  }

  const columns: ColumnsType<ServerEntry> = [
    {
      title: t('settings.servers.name'),
      key: 'name',
      render: (_value, server) => (
        <Space direction="vertical" size={0}>
          <Space size={spacing.xs}>
            <Typography.Text strong>{server.serverName || server.baseUrl}</Typography.Text>
            {server.key === activeKey ? <Tag color="blue">{t('settings.servers.active')}</Tag> : null}
          </Space>
          <Typography.Text type="secondary" style={{ fontSize: fontSize.xs }}>
            {server.baseUrl}
          </Typography.Text>
        </Space>
      )
    },
    {
      title: t('settings.servers.autoConnect'),
      key: 'autoConnect',
      width: 150,
      render: (_value, server) => (
        <Space size={spacing.xs}>
          <Switch
            size="small"
            checked={server.autoConnect !== false}
            onChange={(checked) => updateServer(server.key, { autoConnect: checked })}
          />
          <Typography.Text type="secondary" style={{ fontSize: fontSize.xs }}>
            {server.autoConnect !== false ? t('server.autoConnect.on') : t('server.autoConnect.off')}
          </Typography.Text>
        </Space>
      )
    },
    {
      title: t('common.actions'),
      key: 'actions',
      width: 300,
      render: (_value, server) => (
        <Space size={0} style={{ whiteSpace: 'nowrap' }}>
          <Button
            type="link"
            size="small"
            icon={<LinkOutlined />}
            loading={rowTesting === server.key}
            onClick={() => void testExisting(server)}
          >
            {t('settings.servers.test')}
          </Button>
          <Button type="link" size="small" icon={<EditOutlined />} onClick={() => openEdit(server)}>
            {t('common.edit')}
          </Button>
          <Button
            type="link"
            size="small"
            danger
            icon={<DeleteOutlined />}
            onClick={() => setDeleteTarget(server)}
          >
            {t('common.delete')}
          </Button>
        </Space>
      )
    }
  ]

  return (
    <SectionCard
      title={t('settings.servers.title')}
      extra={
        <Button size="small" type="primary" icon={<PlusOutlined />} onClick={openAdd}>
          {t('settings.servers.add')}
        </Button>
      }
    >
      <Space direction="vertical" size={spacing.sm} style={{ width: '100%' }}>
        <Typography.Text type="secondary" style={{ fontSize: fontSize.sm }}>
          {t('settings.servers.hint')}
        </Typography.Text>
        {servers.length === 0 ? (
          <Typography.Text type="secondary">{t('settings.servers.empty')}</Typography.Text>
        ) : (
          <Table<ServerEntry>
            rowKey="key"
            size="small"
            columns={columns}
            dataSource={servers}
            pagination={false}
          />
        )}
      </Space>

      <Modal
        open={open}
        forceRender
        title={editKey ? t('settings.servers.edit') : t('settings.servers.add')}
        okText={t('common.save')}
        cancelText={t('common.cancel')}
        confirmLoading={saving}
        onOk={() => void save()}
        onCancel={() => setOpen(false)}
      >
        <Form form={form} layout="vertical">
          <Form.Item
            name="baseUrl"
            label={t('field.baseUrl')}
            rules={[
              { required: true, message: t('server.required') },
              {
                validator: (_rule, value: string) =>
                  !value || isValidHttpUrl(value)
                    ? Promise.resolve()
                    : Promise.reject(new Error(t('server.invalidUrl')))
              }
            ]}
          >
            <Input placeholder={t('server.baseUrlPlaceholder')} autoComplete="off" spellCheck={false} />
          </Form.Item>
          <Form.Item name="name" label={t('settings.servers.displayName')}>
            <Input placeholder={t('settings.servers.displayNamePlaceholder')} maxLength={64} />
          </Form.Item>
          <Form.Item
            name="autoConnect"
            label={t('settings.servers.autoConnect')}
            valuePropName="checked"
            extra={t('settings.servers.autoConnectHint')}
          >
            <Switch />
          </Form.Item>
        </Form>

        <Space style={{ marginBottom: spacing.sm }}>
          <Button loading={testing} onClick={() => void testConnection()}>
            {testing ? t('server.testing') : t('server.test')}
          </Button>
        </Space>

        {formError ? <Alert type="error" showIcon message={formError} style={{ marginBottom: spacing.sm }} /> : null}
        {probe ? (
          <Alert
            type="success"
            showIcon
            message={t('server.testSuccess')}
            description={
              <Space direction="vertical" size={0}>
                <span>{`${t('system.serverName')}: ${probe.server_name || '-'}`}</span>
                <span>{`${t('field.baseUrl')}: ${probe.server_url || testedUrl}`}</span>
                <span style={{ fontSize: fontSize.xs }}>
                  {`${t('server.certFingerprint')}: ${probe.cert?.sha256 || '-'}`}
                </span>
              </Space>
            }
          />
        ) : null}
      </Modal>

      <ConfirmDialog
        open={deleteTarget !== null}
        danger
        title={t('settings.servers.deleteConfirm')}
        content={deleteTarget ? deleteTarget.serverName || deleteTarget.baseUrl : undefined}
        onConfirm={() => {
          if (deleteTarget) {
            // 先从代理撤掉这台服务端的会话/自动挂载，再删本地条目；
            // 只删本地条目的话，代理侧仍会保留旧会话继续尝试连接。
            void agentApi.clearSession(deleteTarget.key).catch(() => undefined)
            remove(deleteTarget.key)
          }
          setDeleteTarget(null)
        }}
        onCancel={() => setDeleteTarget(null)}
      />
    </SectionCard>
  )
}
