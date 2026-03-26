import { useCallback, useEffect, useState } from 'react'
import { useTranslation } from 'react-i18next'
import { PieChart, Pie, Cell, ResponsiveContainer, Tooltip } from 'recharts'
import Modal from './Modal'
import { api } from '../api'
import type { AccountRow, AccountUsageDetail } from '../types'
import { getErrorMessage } from '../utils/error'

const COLORS = ['#7c3aed', '#3b82f6', '#10b981', '#f59e0b', '#ef4444', '#ec4899', '#8b5cf6', '#06b6d4', '#84cc16', '#f97316']

function formatTokenCount(n: number): string {
  if (n >= 1_000_000_000) return (n / 1_000_000_000).toFixed(1) + 'B'
  if (n >= 1_000_000) return (n / 1_000_000).toFixed(1) + 'M'
  if (n >= 1_000) return (n / 1_000).toFixed(1) + 'K'
  return String(n)
}

interface Props {
  account: AccountRow
  onClose: () => void
}

export default function AccountUsageModal({ account, onClose }: Props) {
  const { t } = useTranslation()
  const [data, setData] = useState<AccountUsageDetail | null>(null)
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<string | null>(null)

  const load = useCallback(async () => {
    setLoading(true)
    setError(null)
    try {
      const result = await api.getAccountUsage(account.id)
      setData(result)
    } catch (err) {
      setError(getErrorMessage(err))
    } finally {
      setLoading(false)
    }
  }, [account.id])

  useEffect(() => { void load() }, [load])

  const title = t('accounts.usageDetailTitle') + ' — ' + (account.email || account.name || `#${account.id}`)

  return (
    <Modal show title={title} onClose={onClose} contentClassName="sm:max-w-[560px]">
      {loading ? (
        <div className="flex items-center justify-center py-12 text-muted-foreground text-sm">{t('common.loading')}</div>
      ) : error ? (
        <div className="py-8 text-center text-sm text-red-500">{error}</div>
      ) : !data || data.total_requests === 0 ? (
        <div className="py-12 text-center text-sm text-muted-foreground">{t('accounts.noUsageData')}</div>
      ) : (
        <div className="space-y-5">
          {/* Token 统计卡片 */}
          <div className="grid grid-cols-3 gap-2.5">
            <StatCard label={t('accounts.totalRequests')} value={data.total_requests.toLocaleString()} />
            <StatCard label={t('accounts.totalTokens')} value={formatTokenCount(data.total_tokens)} />
            <StatCard label={t('accounts.inputTokens')} value={formatTokenCount(data.input_tokens)} />
            <StatCard label={t('accounts.outputTokens')} value={formatTokenCount(data.output_tokens)} />
            <StatCard label={t('accounts.reasoningTokens')} value={formatTokenCount(data.reasoning_tokens)} />
            <StatCard label={t('accounts.cachedTokens')} value={formatTokenCount(data.cached_tokens)} />
          </div>

          {/* 模型分布饼图 */}
          <div>
            <h4 className="text-sm font-semibold mb-3">{t('accounts.modelDistribution')}</h4>
            <div className="flex items-center gap-4">
              <div className="w-[180px] h-[180px] shrink-0">
                <ResponsiveContainer width="100%" height="100%">
                  <PieChart>
                    <Pie
                      data={data.models}
                      dataKey="requests"
                      nameKey="model"
                      cx="50%"
                      cy="50%"
                      innerRadius={40}
                      outerRadius={75}
                      paddingAngle={2}
                      strokeWidth={0}
                    >
                      {data.models.map((_, i) => (
                        <Cell key={i} fill={COLORS[i % COLORS.length]} />
                      ))}
                    </Pie>
                    <Tooltip
                      formatter={(value: number, name: string) => [`${value} 次`, name]}
                      contentStyle={{ fontSize: 12, borderRadius: 8, border: '1px solid hsl(var(--border))' }}
                    />
                  </PieChart>
                </ResponsiveContainer>
              </div>
              {/* 图例 */}
              <div className="flex-1 space-y-1.5 overflow-hidden">
                {data.models.map((m, i) => (
                  <div key={m.model} className="flex items-center gap-2 text-[12px]">
                    <span className="size-2.5 rounded-full shrink-0" style={{ background: COLORS[i % COLORS.length] }} />
                    <span className="truncate text-foreground font-medium">{m.model}</span>
                    <span className="ml-auto shrink-0 text-muted-foreground">{m.requests}</span>
                  </div>
                ))}
              </div>
            </div>
          </div>
        </div>
      )}
    </Modal>
  )
}

function StatCard({ label, value }: { label: string; value: string }) {
  return (
    <div className="rounded-xl border border-border px-3 py-2.5 text-center">
      <div className="text-[18px] font-bold text-foreground">{value}</div>
      <div className="text-[11px] text-muted-foreground mt-0.5">{label}</div>
    </div>
  )
}
