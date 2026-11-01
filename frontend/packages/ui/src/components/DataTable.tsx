import type { ReactNode } from 'react'
import { Table } from 'antd'
import type { TableColumnsType, TableProps } from 'antd'

import { useLayoutMode } from './PageShell'

export interface DataTableProps<T> {
  columns: TableColumnsType<T>
  rows: T[]
  rowKey: (row: T) => string
  loading?: boolean
  empty?: ReactNode
  pagination?: TableProps<T>['pagination']
  size?: TableProps<T>['size']
  scroll?: TableProps<T>['scroll']
  /** 行展开内容（如日志的完整结构化字段）。 */
  expandable?: TableProps<T>['expandable']
}

/** 表格封装：统一分页默认值、行键与空态文案；管理端外壳下默认更紧凑。 */
export function DataTable<T extends object>({
  columns,
  rows,
  rowKey,
  loading,
  empty,
  pagination,
  size,
  scroll,
  expandable
}: DataTableProps<T>): JSX.Element {
  const defaultSize: TableProps<T>['size'] = useLayoutMode() === 'admin' ? 'small' : 'middle'
  return (
    <Table<T>
      rowKey={(row) => rowKey(row)}
      columns={columns}
      dataSource={rows}
      loading={loading}
      size={size ?? defaultSize}
      scroll={scroll}
      expandable={expandable}
      pagination={
        pagination === undefined
          ? {
              // 列表默认每页 10 条，并允许用户切换每页条数（10/20/50/100）。
              // 不隐藏单页分页器：条数选择入口必须在任何数据量下都存在。
              pageSize: 10,
              pageSizeOptions: [10, 20, 50, 100],
              showSizeChanger: true,
              hideOnSinglePage: false
            }
          : pagination
      }
      locale={{ emptyText: empty }}
    />
  )
}
