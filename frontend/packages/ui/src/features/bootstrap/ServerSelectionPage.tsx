import { Button, Card, Space, Tag, Typography } from 'antd'
import { ArrowRightOutlined, PlusOutlined } from '@ant-design/icons'

import type { ServerEntry } from '../../hooks/useServerConfig'
import { useI18n } from '../../i18n'
import { fontSize, palette, spacing } from '../../tokens/palette'

export interface ServerSelectionPageProps {
  servers: ServerEntry[]
  onSelect: (key: string) => void
  onAdd: () => void
}

/**
 * 启动时的服务端选择页。
 *
 * 什么时候出现：所有已保存服务端都关闭了"自动连接"（或用户手动清空了活动服务端）。
 * 此时不替用户做选择，而是列出全部服务端，让用户点一台进登录/主界面。
 * "自动连接"开关在设置页管理；这里只负责本次选择。
 */
export function ServerSelectionPage({ servers, onSelect, onAdd }: ServerSelectionPageProps): JSX.Element {
  const { t } = useI18n()
  return (
    <div
      style={{
        minHeight: '100vh',
        display: 'flex',
        alignItems: 'center',
        justifyContent: 'center',
        padding: spacing.lg,
        background: palette.bgPage
      }}
    >
      <Card style={{ width: 640, maxWidth: '100%' }}>
        <Typography.Title level={4} style={{ marginTop: 0, marginBottom: spacing.xxs }}>
          {t('server.select.title')}
        </Typography.Title>
        <Typography.Text type="secondary" style={{ display: 'block', fontSize: fontSize.sm }}>
          {t('server.select.desc')}
        </Typography.Text>

        <Space direction="vertical" size={spacing.sm} style={{ width: '100%', marginTop: spacing.md }}>
          {servers.map((server) => {
            const auto = server.autoConnect !== false
            return (
              <Button
                key={server.key}
                block
                type="default"
                onClick={() => onSelect(server.key)}
                style={{ height: 'auto', padding: `${spacing.sm}px ${spacing.md}px`, textAlign: 'start' }}
              >
                <div style={{ display: 'flex', alignItems: 'center', gap: spacing.sm, width: '100%' }}>
                  <div style={{ minWidth: 0, flex: 1 }}>
                    <Typography.Text strong ellipsis style={{ display: 'block' }}>
                      {server.serverName || server.baseUrl}
                    </Typography.Text>
                    <Typography.Text
                      type="secondary"
                      ellipsis
                      style={{ display: 'block', fontSize: fontSize.xs }}
                    >
                      {server.baseUrl}
                    </Typography.Text>
                  </div>
                  <Tag color={auto ? 'green' : undefined} style={{ marginInlineEnd: 0 }}>
                    {auto ? t('server.autoConnect.on') : t('server.autoConnect.off')}
                  </Tag>
                  <ArrowRightOutlined style={{ color: palette.textTertiary }} />
                </div>
              </Button>
            )
          })}
          {servers.length === 0 ? (
            <Typography.Text type="secondary">{t('server.select.empty')}</Typography.Text>
          ) : null}
        </Space>

        <Button type="dashed" block icon={<PlusOutlined />} onClick={onAdd} style={{ marginTop: spacing.md }}>
          {t('server.select.add')}
        </Button>
      </Card>
    </div>
  )
}
