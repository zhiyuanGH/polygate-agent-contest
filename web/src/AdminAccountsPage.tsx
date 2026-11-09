import { useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Alert, Button, Card, Input, InputNumber, Modal, Space, Table, Tag, Typography, message } from 'antd'
import { api, type AdminAccount, type AdminQuotaPreview, type Usage } from './api'

const { Title, Text } = Typography

function sameQuota(left: Usage, right: Usage) {
  return left.cpuRequestMilli === right.cpuRequestMilli && left.cpuLimitMilli === right.cpuLimitMilli &&
    left.memoryMiB === right.memoryMiB && left.pods === right.pods && left.apps === right.apps
}

export default function AdminAccountsPage() {
  const query = useQueryClient()
  const [toast, context] = message.useMessage()
  const me = useQuery({ queryKey: ['me'], queryFn: api.me })
  const allowed = me.data?.role === 'admin'
  const accounts = useQuery({ queryKey: ['admin-accounts'], queryFn: api.adminAccounts,
    enabled: allowed, refetchInterval: allowed ? 5000 : false })
  const audit = useQuery({ queryKey: ['admin-account-audit'], queryFn: api.adminAccountAudit, enabled: allowed })
  const quotaAvailable = allowed && me.data?.adminQuotaAvailable
  const quotaChanges = useQuery({ queryKey: ['admin-quota-changes'], queryFn: api.adminQuotaChanges,
    enabled: quotaAvailable, refetchInterval: quotaAvailable ? 5000 : false })
  const [pending, setPending] = useState<{ account: AdminAccount; enabled: boolean } | null>(null)
  const [reason, setReason] = useState('')
  const [quotaAccount, setQuotaAccount] = useState<AdminAccount | null>(null)
  const [quota, setQuota] = useState<Usage>({ cpuRequestMilli: 2000, cpuLimitMilli: 4000, memoryMiB: 4096, pods: 4, apps: 2 })
  const [quotaReason, setQuotaReason] = useState('')
  const [quotaPreview, setQuotaPreview] = useState<AdminQuotaPreview | null>(null)
  const [quotaPreviewError, setQuotaPreviewError] = useState('')
  const provisionAvailable = allowed && me.data?.adminProvisionAvailable
  const [enrollOpen, setEnrollOpen] = useState(false)
  const [enrollLines, setEnrollLines] = useState('')
  const [enrollReason, setEnrollReason] = useState('Contest enrollment')
  const [resetAccount, setResetAccount] = useState<AdminAccount | null>(null)
  const [resetReason, setResetReason] = useState('')
  const [credentials, setCredentials] = useState<{ username: string; password: string }[]>([])
  const [enrollError, setEnrollError] = useState('')
  const change = useMutation({
    mutationFn: api.setAdminAccountEnabled,
    onSuccess: async () => {
      await Promise.all([
        query.invalidateQueries({ queryKey: ['admin-accounts'] }),
        query.invalidateQueries({ queryKey: ['admin-account-audit'] }),
      ])
      toast.success('Account status updated')
      setPending(null)
      setReason('')
    },
    onError: (error: Error) => toast.error(error.message),
  })
  const changeQuota = useMutation({
    mutationFn: api.queueAdminQuotaChange,
    onSuccess: async () => {
      await query.invalidateQueries({ queryKey: ['admin-quota-changes'] })
      toast.success('Quota change queued')
      setQuotaAccount(null)
      setQuotaReason('')
    },
    onError: (error: Error) => toast.error(error.message),
  })
  const previewQuota = useMutation({
    mutationFn: api.adminQuotaPreview,
    onSuccess: result => { setQuotaPreview(result); setQuotaPreviewError('') },
    onError: (error: Error) => { setQuotaPreview(null); setQuotaPreviewError(error.message) },
  })
  const enroll = useMutation({
    mutationFn: async () => {
      const lines = enrollLines.split(/\r?\n/).map(line => line.trim()).filter(Boolean)
      if (lines.length < 1 || lines.length > 50) throw new Error('Enter 1 to 50 accounts.')
      const parsed = lines.map(line => {
        const separator = line.indexOf(',')
        if (separator < 1) throw new Error('Use username,display name on each line.')
        return { username: line.slice(0, separator).trim(), displayName: line.slice(separator + 1).trim() }
      })
      if (new Set(parsed.map(item => item.username.toLowerCase())).size !== parsed.length) throw new Error('Usernames must be unique.')
      const delivered: { username: string; password: string }[] = []
      for (const [index, item] of parsed.entries()) {
        try {
          const result = await api.createAdminAccount({ ...item, reason: enrollReason.trim() })
          delivered.push({ username: result.account.username, password: result.temporaryPassword })
          setCredentials([...delivered])
        } catch (error) {
          setEnrollLines(parsed.slice(index).map(remaining => `${remaining.username},${remaining.displayName}`).join('\n'))
          setEnrollError(`Stopped at ${item.username}: ${(error as Error).message}. Earlier credentials are shown below.`)
          return
        }
      }
      setEnrollOpen(false)
      setEnrollLines('')
      setEnrollError('')
    },
    onSuccess: async () => {
      await Promise.all([query.invalidateQueries({ queryKey: ['admin-accounts'] }), query.invalidateQueries({ queryKey: ['admin-account-audit'] })])
    },
    onError: (error: Error) => toast.error(error.message),
  })
  const resetPassword = useMutation({
    mutationFn: api.resetAdminPassword,
    onSuccess: async result => {
      setCredentials([{ username: resetAccount?.username ?? '', password: result.temporaryPassword }])
      setResetAccount(null)
      setResetReason('')
      await query.invalidateQueries({ queryKey: ['admin-account-audit'] })
    },
    onError: (error: Error) => toast.error(error.message),
  })
  if (me.isLoading) return <Card loading />
  if (!allowed) return <Alert type="error" showIcon message="Administrator access is required" />
  const pendingQuotaIds = new Set(quotaChanges.data?.filter(item => item.state === 'Queued').map(item => item.targetAccountId) ?? [])
  const previewCurrent = quotaAccount !== null && quotaPreview !== null &&
    quotaPreview.targetAccountId === quotaAccount.id && sameQuota(quotaPreview.proposedQuota, quota) &&
    sameQuota(quotaPreview.currentQuota, quotaAccount.quota)
  return <>
    {context}
    <Title level={2}>Accounts</Title>
    <Text type="secondary">Disabling an account ends its current sessions and blocks new requests. Existing running apps continue until separately stopped.</Text>
    {provisionAvailable && <div style={{ marginTop: 16 }}><Button type="primary" onClick={() => { setCredentials([]); setEnrollError(''); setEnrollOpen(true) }}>Enroll students</Button></div>}
    {quotaAvailable && <Alert style={{ marginTop: 16 }} type="info" showIcon message="Quota changes are applied by the cluster controller" description="Existing apps keep running when an account quota is lowered. New requests must fit the new limit after the change is applied." />}
    {accounts.error && <Alert type="error" showIcon message="Could not load accounts" description={(accounts.error as Error).message} />}
    <Card style={{ marginTop: 20 }}>
      <Table rowKey="id" dataSource={accounts.data ?? []} loading={accounts.isLoading} pagination={{ pageSize: 20 }}
        columns={[
          { title: 'Username', dataIndex: 'username', sorter: (a: AdminAccount, b: AdminAccount) => a.username.localeCompare(b.username) },
          { title: 'Name', dataIndex: 'displayName' },
          { title: 'Role', dataIndex: 'role' },
          { title: 'Status', render: (_: unknown, item: AdminAccount) => <Tag color={item.enabled ? 'green' : 'red'}>{item.enabled ? 'Enabled' : 'Disabled'}</Tag> },
          { title: 'Apps', dataIndex: 'appCount' },
          { title: 'Quota', render: (_: unknown, item: AdminAccount) => `${item.quota.apps} apps · ${item.quota.memoryMiB / 1024} GiB` },
          { title: 'Action', render: (_: unknown, item: AdminAccount) => item.role === 'student' && <Space>
            <Button size="small" danger={item.enabled} disabled={item.id === me.data?.id} onClick={() => { setReason(''); setPending({ account: item, enabled: !item.enabled }) }}>{item.enabled ? 'Disable' : 'Enable'}</Button>
            {quotaAvailable && <Button size="small" disabled={pendingQuotaIds.has(item.id)} onClick={() => { setQuotaAccount(item); setQuota({ ...item.quota }); setQuotaReason(''); setQuotaPreview(null); setQuotaPreviewError('') }}>{pendingQuotaIds.has(item.id) ? 'Quota pending' : 'Edit quota'}</Button>}
            {provisionAvailable && <Button size="small" onClick={() => { setCredentials([]); setResetReason(''); setResetAccount(item) }}>Reset password</Button>}
          </Space> },
        ]} />
    </Card>
    <Modal title="Enroll students" open={enrollOpen} closable={!enroll.isPending} maskClosable={!enroll.isPending}
      onCancel={() => { if (!enroll.isPending) setEnrollOpen(false) }}
      footer={<Space><Button disabled={enroll.isPending} onClick={() => setEnrollOpen(false)}>Close</Button><Button type="primary" loading={enroll.isPending}
        disabled={enrollReason.trim().length < 3 || enrollReason.trim().length > 500 || !enrollLines.trim()}
        onClick={() => enroll.mutate()}>Create accounts</Button></Space>}>
      <p>Enter one student per line as <code>username,display name</code> (maximum 50). Temporary passwords appear once after creation.</p>
      {enrollError && <Alert type="error" showIcon message={enrollError} style={{ marginBottom: 12 }} />}
      <Input.TextArea aria-label="Student list" value={enrollLines} onChange={event => setEnrollLines(event.target.value)} rows={8} />
      <label>Reason<Input.TextArea aria-label="Enrollment reason" value={enrollReason} onChange={event => setEnrollReason(event.target.value)} rows={2} maxLength={500} /></label>
    </Modal>
    <Modal title={`Reset password: ${resetAccount?.username ?? ''}`} open={resetAccount !== null}
      onCancel={() => setResetAccount(null)}
      footer={<Space><Button onClick={() => setResetAccount(null)}>Cancel</Button><Button type="primary" loading={resetPassword.isPending}
        disabled={resetReason.trim().length < 3 || resetReason.trim().length > 500}
        onClick={() => { if (resetAccount) resetPassword.mutate({ id: resetAccount.id, reason: resetReason.trim() }) }}>Reset password</Button></Space>}>
      <p>This ends the student's sessions and requires a password change at next sign in.</p>
      <Input.TextArea aria-label="Password reset reason" value={resetReason} onChange={event => setResetReason(event.target.value)} maxLength={500} rows={2} />
    </Modal>
    <Modal title="Temporary passwords" open={credentials.length > 0} closable={!enroll.isPending} maskClosable={!enroll.isPending}
      onCancel={() => { if (!enroll.isPending) { setCredentials([]); setEnrollError('') } }}
      footer={<Button disabled={enroll.isPending} onClick={() => { setCredentials([]); setEnrollError('') }}>Done</Button>}>
      <Alert type="warning" showIcon message="Save these passwords now" description="They are displayed only in this browser response and cannot be retrieved later." />
      {enroll.isPending && <Alert style={{ marginTop: 12 }} type="info" message="Creating accounts; keep this window open until the batch finishes." />}
      {enrollError && <Alert style={{ marginTop: 12 }} type="error" message={enrollError} />}
      <Input.TextArea aria-label="Temporary credentials" readOnly value={credentials.map(item => `${item.username}\t${item.password}`).join('\n')} rows={Math.min(10, Math.max(2, credentials.length))} style={{ marginTop: 12 }} />
      <Button style={{ marginTop: 12 }} onClick={() => void navigator.clipboard.writeText(credentials.map(item => `${item.username}\t${item.password}`).join('\n'))}>Copy credentials</Button>
    </Modal>
    {quotaAvailable && <Card title="Recent quota changes" style={{ marginTop: 20 }}>
      {quotaChanges.error && <Alert type="error" showIcon message="Could not load quota changes" description={(quotaChanges.error as Error).message} />}
      <Table rowKey="id" dataSource={quotaChanges.data ?? []} loading={quotaChanges.isLoading} pagination={{ pageSize: 10 }}
        columns={[
          { title: 'Time', dataIndex: 'createdAt', render: (value: string) => new Date(value).toLocaleString() },
          { title: 'Account', dataIndex: 'targetAccountId', render: (value: string) => accounts.data?.find(item => item.id === value)?.username ?? value },
          { title: 'State', dataIndex: 'state' },
          { title: 'Quota', render: (_: unknown, item: { quota: Usage }) => `${item.quota.apps} apps · ${item.quota.memoryMiB / 1024} GiB` },
          { title: 'Reason', dataIndex: 'reason' },
          { title: 'Error', dataIndex: 'lastError' },
        ]} />
    </Card>}
    <Card title="Recent account changes" style={{ marginTop: 20 }}>
      {audit.error && <Alert type="error" showIcon message="Could not load account audit" description={(audit.error as Error).message} />}
      <Table rowKey="id" dataSource={audit.data ?? []} loading={audit.isLoading} pagination={{ pageSize: 10 }}
        columns={[
          { title: 'Time', dataIndex: 'createdAt', render: (value: string) => new Date(value).toLocaleString() },
          { title: 'Action', dataIndex: 'action', render: (value: string) => value.replaceAll('_', ' ') },
          { title: 'Actor', dataIndex: 'actorUsername' },
          { title: 'Account', dataIndex: 'targetAccountId', render: (value: string) => accounts.data?.find(item => item.id === value)?.username ?? value },
          { title: 'Reason', dataIndex: 'reason' },
        ]} />
    </Card>
    <Modal title={`${pending?.enabled ? 'Enable' : 'Disable'} ${pending?.account.username ?? 'account'}`} open={pending !== null}
      onCancel={() => { setPending(null); setReason('') }}
      footer={<Space><Button onClick={() => { setPending(null); setReason('') }}>Cancel</Button><Button type="primary" danger={!pending?.enabled} loading={change.isPending} disabled={reason.trim().length < 3 || reason.trim().length > 500} onClick={() => { if (pending) change.mutate({ id: pending.account.id, enabled: pending.enabled, reason: reason.trim() }) }}>Confirm</Button></Space>}>
      <Text>Enter a reason for the audit record.</Text>
      <Input.TextArea aria-label="Reason" value={reason} onChange={event => setReason(event.target.value)} maxLength={500} showCount rows={3} style={{ marginTop: 12 }} />
    </Modal>
    <Modal title={`Edit quota: ${quotaAccount?.username ?? ''}`} open={quotaAccount !== null}
      onCancel={() => { setQuotaAccount(null); setQuotaReason(''); setQuotaPreview(null) }}
      footer={<Space><Button onClick={() => { setQuotaAccount(null); setQuotaReason(''); setQuotaPreview(null) }}>Cancel</Button>
        <Button loading={previewQuota.isPending} disabled={!quotaAccount || sameQuota(quota, quotaAccount.quota)}
          onClick={() => { if (quotaAccount) previewQuota.mutate({ id: quotaAccount.id, quota }) }}>Preview impact</Button>
        <Button type="primary" loading={changeQuota.isPending}
        disabled={quotaReason.trim().length < 3 || quotaReason.trim().length > 500 || !previewCurrent}
        onClick={() => { if (quotaAccount) changeQuota.mutate({ id: quotaAccount.id, quota, reason: quotaReason.trim() }) }}>Queue change</Button></Space>}>
      <Space direction="vertical" style={{ width: '100%' }}>
        {([
          ['CPU request (m)', 'cpuRequestMilli', 250, 8000],
          ['CPU limit (m)', 'cpuLimitMilli', 1000, 16000],
          ['Memory (MiB)', 'memoryMiB', 1024, 16384],
          ['Pods', 'pods', 1, 8],
          ['Apps', 'apps', 1, 4],
        ] as const).map(([label, field, min, max]) => <label key={field}>{label}<br /><InputNumber aria-label={label} min={min} max={max} value={quota[field]}
          onChange={value => { if (value !== null) { setQuota(current => ({ ...current, [field]: value })); setQuotaPreview(null) } }} style={{ width: '100%' }} /></label>)}
        {quotaPreviewError && <Alert type="error" showIcon message="Could not preview quota impact" description={quotaPreviewError} />}
        {previewCurrent && quotaPreview && <>
          <Alert type={quotaPreview.overQuota.length ? 'warning' : 'info'} showIcon
            message={quotaPreview.overQuota.length ? 'Proposed quota is below configured use' : 'Configured use fits the proposed quota'}
            description={quotaPreview.overQuota.length
              ? `Over quota: ${quotaPreview.overQuota.join(', ')}. Existing running apps will continue; new requests must fit the new quota. Review the apps below before changing it.`
              : 'Existing apps will keep their current settings. This preview is a snapshot; new activity may change usage before the quota change is applied.'} />
          <Text>Configured: {quotaPreview.configured.apps} apps, {quotaPreview.configured.pods} Pods, {quotaPreview.configured.memoryMiB / 1024} GiB memory, {quotaPreview.configured.cpuRequestMilli}m CPU request, {quotaPreview.configured.cpuLimitMilli}m CPU limit. Checked {new Date(quotaPreview.checkedAt).toLocaleString('en-GB')}.</Text>
          <Table rowKey="id" size="small" pagination={false} dataSource={quotaPreview.apps}
            columns={[{ title: 'Existing app', dataIndex: 'name' }, { title: 'Status', dataIndex: 'status' },
              { title: 'Pods', render: (_: unknown, app: AdminQuotaPreview['apps'][number]) => app.resources.replicas },
              { title: 'Memory', render: (_: unknown, app: AdminQuotaPreview['apps'][number]) => `${app.resources.replicas * app.resources.memoryMiB / 1024} GiB` }]} />
        </>}
        <label>Reason<Input.TextArea aria-label="Quota reason" value={quotaReason} onChange={event => setQuotaReason(event.target.value)} maxLength={500} showCount rows={3} /></label>
      </Space>
    </Modal>
  </>
}
