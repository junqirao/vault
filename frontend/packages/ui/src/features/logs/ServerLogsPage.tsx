import { Tabs } from 'antd'

import { PageShell } from '../../components/PageShell'
import { useI18n } from '../../i18n'
import { AuditList } from '../audit/AuditList'
import { LogViewer } from './LogViewer'

/**
 * 管理端「服务日志」：把原来的服务端日志与审计日志合并成同一页，用 Tab 区分。
 *
 * 两者都是"出问题后按时间回看系统发生了什么"，分在侧边栏两个入口只会让人来回切；
 * 合并后共用页面标题与留白，Tab 内各自保留原有的筛选与分页模块。
 */
export function ServerLogsPage(): JSX.Element {
  const { t } = useI18n()

  return (
    <PageShell title={t('page.serverLogs.title')} subtitle={t('page.serverLogs.subtitle')}>
      <Tabs
        defaultActiveKey="server"
        items={[
          {
            key: 'server',
            label: t('page.serverLogs.tab.server'),
            children: <LogViewer source="server" embedded />
          },
          {
            key: 'audit',
            label: t('page.serverLogs.tab.audit'),
            children: <AuditList embedded />
          }
        ]}
      />
    </PageShell>
  )
}
