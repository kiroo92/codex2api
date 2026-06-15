import { useMemo, useState } from 'react'
import { useTranslation } from 'react-i18next'
import { ArrowLeft, Mail, Send } from 'lucide-react'
import PageHeader from './PageHeader'
import { Button } from '@/components/ui/button'
import { api } from '../api'
import type { AccountRow, InviteResult } from '../types'
import { getErrorMessage } from '../utils/error'
import { useToast } from '../hooks/useToast'

interface Props {
  accounts: AccountRow[]
  onClose: () => void
}

// CodexInviteView 是账号管理页内的「Codex 邀请」视图，入口与回收站一致。
// 选择一个 Codex 账号，填入邮箱后通过该账号凭证发送推荐邀请。
export default function CodexInviteView({ accounts, onClose }: Props) {
  const { t } = useTranslation()
  const { showToast } = useToast()

  // 仅可用 Codex OAuth 账号发送邀请（中转/AT-only 账号没有可用于 referral 的凭证）。
  const codexAccounts = useMemo(
    () => accounts.filter((a) => !a.openai_responses_api && !a.at_only),
    [accounts],
  )

  const [accountId, setAccountId] = useState<number | null>(codexAccounts[0]?.id ?? null)
  const [emailsText, setEmailsText] = useState('')
  const [sending, setSending] = useState(false)
  const [result, setResult] = useState<InviteResult | null>(null)
  const [error, setError] = useState<string | null>(null)

  const handleSend = async () => {
    if (accountId == null) {
      setError(t('invite.noAccountSelected'))
      return
    }
    if (emailsText.trim() === '') {
      setError(t('invite.noEmails'))
      return
    }
    setSending(true)
    setError(null)
    setResult(null)
    try {
      const res = await api.sendInvite(accountId, { emails_text: emailsText })
      setResult(res.result)
      if (res.ok) {
        showToast(t('invite.sendSuccess'), 'success')
      } else {
        showToast(t('invite.sendUpstreamFailed', { code: res.result.status_code }), 'error')
      }
    } catch (err) {
      setError(getErrorMessage(err))
      showToast(t('invite.sendFailed', { error: getErrorMessage(err) }), 'error')
    } finally {
      setSending(false)
    }
  }

  return (
    <div>
      <PageHeader
        title={t('invite.title')}
        description={t('invite.description')}
        actions={
          <div className="flex flex-wrap items-center justify-end gap-1.5">
            <Button variant="outline" onClick={onClose} className="max-sm:w-full">
              <ArrowLeft className="size-3.5" />
              {t('invite.back')}
            </Button>
          </div>
        }
      />

      <div className="mx-auto mt-4 max-w-2xl space-y-5">
        <div className="rounded-2xl border bg-card p-5">
          <label className="mb-1.5 block text-sm font-medium">{t('invite.accountLabel')}</label>
          {codexAccounts.length === 0 ? (
            <p className="text-sm text-muted-foreground">{t('invite.noCodexAccounts')}</p>
          ) : (
            <select
              value={accountId ?? ''}
              onChange={(e) => setAccountId(Number(e.target.value))}
              className="h-9 w-full rounded-lg border bg-background px-3 text-sm"
            >
              {codexAccounts.map((a) => (
                <option key={a.id} value={a.id}>
                  {a.email || a.name || `#${a.id}`}
                  {a.plan_type ? ` · ${a.plan_type}` : ''}
                </option>
              ))}
            </select>
          )}

          <label className="mb-1.5 mt-4 block text-sm font-medium">{t('invite.emailsLabel')}</label>
          <textarea
            value={emailsText}
            onChange={(e) => setEmailsText(e.target.value)}
            rows={6}
            placeholder={t('invite.emailsPlaceholder')}
            className="w-full resize-y rounded-lg border bg-background px-3 py-2 text-sm"
          />
          <p className="mt-1.5 text-xs text-muted-foreground">{t('invite.emailsHint')}</p>

          {error && <div className="mt-3 text-sm text-red-500">{error}</div>}

          <div className="mt-4 flex justify-end">
            <Button
              disabled={sending || accountId == null}
              onClick={() => void handleSend()}
            >
              <Send className="size-3.5" />
              {sending ? t('invite.sending') : t('invite.send')}
            </Button>
          </div>
        </div>

        {result && <InviteResultCard result={result} />}
      </div>
    </div>
  )
}

function InviteResultCard({ result }: { result: InviteResult }) {
  const { t } = useTranslation()
  return (
    <div className="rounded-2xl border bg-card p-5">
      <div className="mb-3 flex items-center gap-2">
        <Mail className="size-4 text-muted-foreground" />
        <h4 className="text-base font-semibold">{t('invite.resultTitle')}</h4>
        <span
          className={`ml-auto inline-flex h-6 items-center rounded-full px-2.5 text-xs font-semibold ${
            result.ok
              ? 'bg-emerald-500/10 text-emerald-600'
              : 'bg-red-500/10 text-red-600'
          }`}
        >
          {result.ok ? t('invite.resultOk') : t('invite.resultFailed', { code: result.status_code })}
        </span>
      </div>

      {result.invites && result.invites.length > 0 && (
        <div className="mb-3 space-y-2">
          {result.invites.map((inv, i) => (
            <div key={inv.referral_id || inv.email || i} className="rounded-lg border bg-background px-3 py-2 text-sm">
              <div className="font-medium text-foreground">{inv.email || '-'}</div>
              {inv.invite_url && (
                <a
                  href={inv.invite_url}
                  target="_blank"
                  rel="noreferrer"
                  className="break-all text-xs text-primary hover:underline"
                >
                  {inv.invite_url}
                </a>
              )}
            </div>
          ))}
        </div>
      )}

      {result.request_id && (
        <p className="mb-2 text-xs text-muted-foreground">request_id: {result.request_id}</p>
      )}

      <pre className="max-h-64 overflow-auto rounded-lg border bg-muted/40 p-3 text-xs">
        {result.upstream != null
          ? JSON.stringify(result.upstream, null, 2)
          : result.upstream_raw || ''}
      </pre>
    </div>
  )
}
