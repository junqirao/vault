import { useEffect, useMemo, useRef, useState } from 'react'
import { Card, Space, Tooltip, Typography } from 'antd'
import { QuestionCircleOutlined } from '@ant-design/icons'
import dayjs from 'dayjs'
import * as echarts from 'echarts/core'
import { LineChart } from 'echarts/charts'
import { GridComponent, LegendComponent, TooltipComponent } from 'echarts/components'
import { CanvasRenderer } from 'echarts/renderers'
import type { EChartsOption } from 'echarts'

import { agentApi } from '../../api/agentClient'
import type { AgentEventStream } from '../../api/agentClient'
import type { AgentPerformanceHistory, AgentPerformancePoint } from '../../api/agentTypes'
import { useAgent } from '../../hooks/useAgent'
import { useI18n } from '../../i18n'
import { fontSize, palette, spacing } from '../../tokens/palette'
import { formatBytes } from '../../utils/format'

// 只注册用到的图表/组件，沿用 CacheMetricsDialog 的按需引入。
echarts.use([LineChart, GridComponent, LegendComponent, TooltipComponent, CanvasRenderer])

/** ECharts 的薄封装：负责 init/resize/dispose，option 变化时整份替换。 */
function PerfChart({ option, height }: { option: EChartsOption; height: number }): JSX.Element {
  const hostRef = useRef<HTMLDivElement | null>(null)
  const chartRef = useRef<ReturnType<typeof echarts.init> | null>(null)

  useEffect(() => {
    if (!hostRef.current) return
    const chart = echarts.init(hostRef.current)
    chartRef.current = chart
    const onResize = (): void => chart.resize()
    window.addEventListener('resize', onResize)
    return () => {
      window.removeEventListener('resize', onResize)
      chart.dispose()
      chartRef.current = null
    }
  }, [])

  useEffect(() => {
    chartRef.current?.setOption(option, true)
  }, [option])

  return <div ref={hostRef} style={{ width: '100%', height }} />
}

interface Series {
  name: string
  color: string
  data: number[]
  /**
   * 同色系的次要指标（吞吐 vs IOPS）：用更淡的线 + 更浅的填充区分，
   * 而不是虚线 —— 虚线在浅色卡片上显得毛躁。
   */
  soft?: boolean
  axis?: number
}

interface TooltipParam {
  axisValue?: string
  seriesName?: string
  value?: number
  color?: string
}

const fmtIops = (v: number): string => (v >= 100 ? v.toFixed(0) : v.toFixed(1))
const fmtSpeed = (v: number): string => `${formatBytes(v)}/s`
const fmtPct = (v: number): string => `${(v * 100).toFixed(1)}%`

/** 没有挂载/采样时展示的零值点（计数器本身为 0，不用"暂无数据"占位）。 */
function zeroPoint(t: number): AgentPerformancePoint {
  return {
    t,
    read_iops: 0,
    write_iops: 0,
    read_bytes_per_sec: 0,
    write_bytes_per_sec: 0,
    hit_rate: 0,
    l1_used_bytes: 0,
    l2_used_bytes: 0,
    cache_used_bytes: 0
  }
}

/** 给十六进制色加透明度：曲线下的填充要"淡色 + 微微透明"。 */
function withAlpha(hex: string, alpha: number): string {
  const value = hex.replace('#', '')
  const r = parseInt(value.slice(0, 2), 16)
  const g = parseInt(value.slice(2, 4), 16)
  const b = parseInt(value.slice(4, 6), 16)
  return `rgba(${r}, ${g}, ${b}, ${alpha})`
}

function baseOption(times: string[], series: Series[], tooltip: (params: unknown) => string): EChartsOption {
  return {
    animation: false,
    grid: { left: 60, right: 64, top: 34, bottom: 24 },
    legend: {
      top: 0,
      itemWidth: 14,
      itemHeight: 6,
      textStyle: { fontSize: 11, color: palette.textSecondary }
    },
    tooltip: { trigger: 'axis', formatter: tooltip },
    xAxis: {
      type: 'category',
      data: times,
      boundaryGap: false,
      axisTick: { show: false },
      axisLine: { lineStyle: { color: palette.neutralFill } },
      axisLabel: { fontSize: 10, color: palette.textTertiary, hideOverlap: true }
    },
    series: series.map((s) => ({
      name: s.name,
      type: 'line',
      yAxisIndex: s.axis ?? 0,
      data: s.data,
      showSymbol: false,
      connectNulls: false,
      // 柔和曲线 + 淡色半透明实心填充；同色系次要指标（soft）整体更淡一档。
      smooth: 0.4,
      lineStyle: { width: s.soft ? 1.3 : 1.8, color: withAlpha(s.color, s.soft ? 0.55 : 1), type: 'solid' },
      itemStyle: { color: s.color },
      areaStyle: { color: withAlpha(s.color, s.soft ? 0.07 : 0.12) }
    }))
  }
}

function tooltipLines(title: string, list: TooltipParam[], format: (name: string, value: number) => string): string {
  return [title, ...list.map((item) => `${item.seriesName ?? ''}: ${format(item.seriesName ?? '', item.value ?? 0)}`)]
    .filter((line) => line !== '')
    .join('<br/>')
}

function chartTitle(params: unknown): string {
  return Array.isArray(params) && params.length > 0
    ? String((params[0] as TooltipParam).axisValue ?? '')
    : ''
}

/**
 * 我的存储库首页的「性能」模块。
 *
 * 数据来自 GET /agent/performance/events：页面挂载时才建立 SSE，代理每 5 秒采样并在 Go 侧
 * 算好 IOPS / 吞吐 / 缓存占用 / 命中率；这里只画两张图，不再堆文字指标。
 */
export function PerformanceMonitor(): JSX.Element {
  const { t } = useI18n()
  const agent = useAgent()
  const [data, setData] = useState<AgentPerformanceHistory | null>(null)

  // 只在页面挂载时建立性能 SSE；卸载/切页立即断开，不在全局常驻。
  useEffect(() => {
    if (!agent.available) {
      setData(null)
      return
    }
    let stopped = false
    let stream: AgentEventStream | null = null
    let retryTimer: ReturnType<typeof setTimeout> | null = null

    const scheduleReconnect = (): void => {
      if (stopped || retryTimer) return
      retryTimer = setTimeout(() => {
        retryTimer = null
        connect()
      }, 3000)
    }

    const connect = (): void => {
      if (stopped) return
      try {
        stream = agentApi.subscribePerformance()
      } catch {
        scheduleReconnect()
        return
      }
      const onPerformance = (event: Event): void => {
        const payload = (event as MessageEvent).data
        if (typeof payload !== 'string') return
        try {
          const parsed = JSON.parse(payload) as AgentPerformanceHistory
          if (!stopped) setData(parsed)
        } catch {
          // 坏帧忽略，保留上一份缓存
        }
      }
      stream.addEventListener('performance', onPerformance)
      stream.onerror = () => {
        stream?.close()
        stream = null
        scheduleReconnect()
      }
    }

    connect()
    return () => {
      stopped = true
      if (retryTimer) clearTimeout(retryTimer)
      stream?.close()
    }
  }, [agent.available])

  const points = useMemo<AgentPerformancePoint[]>(() => data?.points ?? [], [data?.points])
  // 没有挂载/采样时也照常画：用两个零值点撑起坐标轴，所有读数显示 0。
  const chartPoints = useMemo<AgentPerformancePoint[]>(() => {
    if (points.length >= 2) return points
    const now = Math.floor(Date.now() / 1000)
    if (points.length === 1) return [zeroPoint(now - 5), { ...points[0], t: now }]
    return [zeroPoint(now - 5), zeroPoint(now)]
  }, [points])
  const times = useMemo(() => chartPoints.map((p) => dayjs(p.t * 1000).format('HH:mm:ss')), [chartPoints])

  const readIops = t('repo.performance.readIops')
  const writeIops = t('repo.performance.writeIops')
  const readSpeed = t('repo.performance.readSpeed')
  const writeSpeed = t('repo.performance.writeSpeed')
  const hitRate = t('repo.performance.hitRate')

  // 磁盘性能：IOPS + 吞吐共用一张图（左轴 IOPS、右轴字节/秒），颜色区分读/写、线型区分指标。
  const diskOption = useMemo<EChartsOption>(() => {
    const option = baseOption(
      times,
      [
        { name: readIops, color: palette.accent, data: chartPoints.map((p) => p.read_iops), axis: 0 },
        { name: writeIops, color: palette.warning, data: chartPoints.map((p) => p.write_iops), axis: 0 },
        {
          name: readSpeed,
          color: palette.accent,
          data: chartPoints.map((p) => p.read_bytes_per_sec),
          soft: true,
          axis: 1
        },
        {
          name: writeSpeed,
          color: palette.warning,
          data: chartPoints.map((p) => p.write_bytes_per_sec),
          soft: true,
          axis: 1
        }
      ],
      (params) => {
        const title = chartTitle(params)
        const list = Array.isArray(params) ? (params as TooltipParam[]) : []
        return tooltipLines(title, list, (name, value) =>
          name === readIops || name === writeIops ? fmtIops(value) : fmtSpeed(value)
        )
      }
    )
    return {
      ...option,
      yAxis: [
        {
          type: 'value',
          min: 0,
          axisLabel: { fontSize: 10, color: palette.textTertiary, formatter: (v: number) => fmtIops(v) },
          splitLine: { lineStyle: { color: palette.neutralFill } }
        },
        {
          type: 'value',
          min: 0,
          axisLabel: { fontSize: 10, color: palette.textTertiary, formatter: (v: number) => formatBytes(v) },
          splitLine: { show: false }
        }
      ]
    }
  }, [times, chartPoints, readIops, writeIops, readSpeed, writeSpeed])

  // 缓存性能：左轴缓存占用（L1/L2），右轴命中率。
  const cacheOption = useMemo<EChartsOption>(() => {
    const option = baseOption(
      times,
      [
        { name: 'L1', color: palette.accent, data: chartPoints.map((p) => p.l1_used_bytes), axis: 0 },
        { name: 'L2', color: palette.warning, data: chartPoints.map((p) => p.l2_used_bytes), axis: 0 },
        { name: hitRate, color: palette.success, data: chartPoints.map((p) => p.hit_rate), axis: 1 }
      ],
      (params) => {
        const title = chartTitle(params)
        const list = Array.isArray(params) ? (params as TooltipParam[]) : []
        return tooltipLines(title, list, (name, value) => (name === hitRate ? fmtPct(value) : formatBytes(value)))
      }
    )
    return {
      ...option,
      yAxis: [
        {
          type: 'value',
          min: 0,
          axisLabel: { fontSize: 10, color: palette.textTertiary, formatter: (v: number) => formatBytes(v) },
          splitLine: { lineStyle: { color: palette.neutralFill } }
        },
        {
          type: 'value',
          min: 0,
          max: 1,
          axisLabel: { fontSize: 10, color: palette.textTertiary, formatter: (v: number) => `${Math.round(v * 100)}%` },
          splitLine: { show: false }
        }
      ]
    }
  }, [times, chartPoints, hitRate])

  return (
    <Card
      title={
        <Space size={spacing.xs}>
          <span>{t('repo.performance.title')}</span>
          <Tooltip title={t('repo.performance.proxyHint')} placement="right">
            <QuestionCircleOutlined style={{ color: palette.textTertiary, cursor: 'help' }} />
          </Tooltip>
        </Space>
      }
      extra={
        <Typography.Text type="secondary" style={{ fontSize: fontSize.xs }}>
          {t('repo.performance.window')}
        </Typography.Text>
      }
      styles={{ body: { padding: spacing.md, flex: 1, minHeight: 0 } }}
      style={{ width: '100%', height: '100%', display: 'flex', flexDirection: 'column' }}
    >
      <div
        style={{
          display: 'grid',
          gridTemplateColumns: 'repeat(auto-fit, minmax(300px, 1fr))',
          gap: spacing.md,
          height: '100%'
        }}
      >
        <div style={{ minWidth: 0 }}>
          <Typography.Text strong style={{ display: 'block', marginBottom: spacing.xs, fontSize: fontSize.sm }}>
            {t('repo.performance.disk')}
          </Typography.Text>
          <PerfChart option={diskOption} height={190} />
        </div>
        <div style={{ minWidth: 0 }}>
          <Typography.Text strong style={{ display: 'block', marginBottom: spacing.xs, fontSize: fontSize.sm }}>
            {t('repo.performance.cache')}
          </Typography.Text>
          <PerfChart option={cacheOption} height={190} />
        </div>
      </div>
    </Card>
  )
}
