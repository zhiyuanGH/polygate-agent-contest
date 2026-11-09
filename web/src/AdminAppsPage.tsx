import { useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Alert, Button, Card, Input, Modal, Space, Table, Tag, Typography, message } from 'antd'
import { api, type AdminApp, type AdminAppAccessAudit } from './api'

const { Title, Text } = Typography

export default function AdminAppsPage() {
  const query = useQueryClient()
  const [toast, context] = message.useMessage()
  const me = useQuery({ queryKey: ['me'], queryFn: api.me })
  const allowed = me.data?.role === 'admin'
  const apps = useQuery({ queryKey: ['admin-apps'], queryFn: api.adminApps,
    enabled: allowed, refetchInterval: allowed ? 10000 : false })
  const audit = useQuery({ queryKey: ['admin-app-access-audit'], queryFn: api.adminAppAccessAudit,
    enabled: allowed })
  const [pending, setPending] = useState<AdminApp | null>(null)
  const [reason, setReason] = useState('')
  const change = useMutation({
    mutationFn: api.setAdminAppAccess,
    onSuccess: async () => {
      await Promise.all([
        query.invalidateQueries({ queryKey: ['admin-apps'] }),
        query.invalidateQueries({ queryKey: ['admin-app-access-audit'] }),
        query.invalidateQueries({ queryKey: ['apps'] }),
      ])
      toast.success('App access updated')
      setPending(null)
      setReason('')
    },
    onError: (error: Error) => toast.error(error.message),
  })
  if (me.isLoading) return <Card loading />
  if (!allowed) return <Alert type="error" showIcon message="Administrator access is required" />
  const privateHost = (id: string) => me.data?.privateAppDomain
    ? `https://${id}.${me.data.privateAppDomain}/` : ''
  const nextMode = pending?.accessMode === 'Public' ? 'Owner and administrators' : 'Public'
  return <>
    {context}
    <Title level={2}>App access</Title>
    <Text type="secondary">Public apps use the shared path URL. Restricted apps use a separate hostname and are available only to their owner and administrators.</Text>
    {apps.error && <Alert style={{ marginTop: 16 }} type="error" showIcon message="Could not load apps" description={(apps.error as Error).message} />}
    <Card style={{ marginTop: 20 }}>
      <Table<AdminApp> rowKey="id" dataSource={apps.data ?? []} loading={apps.isLoading} pagination={{ pageSize: 20 }}
        columns={[
          { title: 'Owner', dataIndex: 'username', sorter: (a, b) => a.username.localeCompare(b.username) },
          { title: 'App', dataIndex: 'name' },
          { title: 'Status', dataIndex: 'status' },
          { title: 'Access', render: (_: unknown, app) => <Tag color={app.accessMode === 'Public' ? 'green' : 'gold'}>{app.accessMode}</Tag> },
          { title: 'Private route', render: (_: unknown, app) => privateHost(app.id)
            ? <a href={privateHost(app.id)} target="_blank" rel="noopener noreferrer">{privateHost(app.id)}</a>
            : 'Not configured' },
          { title: 'Action', render: (_: unknown, app) => <Button size="small"
            disabled={app.accessMode === 'Public' && (app.status !== 'Running' || !me.data?.privateAppDomain)}
            onClick={() => { setPending(app); setReason('') }}>
            {app.accessMode === 'Public' ? 'Restrict access' : 'Make public'}
          </Button> },
        ]} />
    </Card>
    <Card title="Recent access changes" style={{ marginTop: 20 }}>
      {audit.error && <Alert type="error" showIcon message="Could not load access audit" description={(audit.error as Error).message} />}
      <Table<AdminAppAccessAudit> rowKey="id" dataSource={audit.data ?? []} loading={audit.isLoading} pagination={{ pageSize: 10 }}
        columns={[
          { title: 'Time', dataIndex: 'createdAt', render: (value: string) => new Date(value).toLocaleString('en-GB') },
          { title: 'Owner', dataIndex: 'username' },
          { title: 'App', dataIndex: 'appName' },
          { title: 'Change', render: (_: unknown, item) => `${item.previousMode} → ${item.newMode}` },
          { title: 'Actor', dataIndex: 'actorSubject' },
          { title: 'Reason', dataIndex: 'reason' },
        ]} />
    </Card>
    <Modal title={`${nextMode === 'Public' ? 'Make public' : 'Restrict'}: ${pending?.username ?? ''}/${pending?.name ?? ''}`}
      open={pending !== null} onCancel={() => { if (!change.isPending) setPending(null) }}
      footer={<Space><Button disabled={change.isPending} onClick={() => setPending(null)}>Cancel</Button>
        <Button type="primary" loading={change.isPending} disabled={reason.trim().length < 3 || reason.trim().length > 500}
          onClick={() => { if (pending) change.mutate({ id: pending.id, mode: nextMode, reason: reason.trim() }) }}>Confirm</Button></Space>}>
      {nextMode !== 'Public' && <Alert type="info" showIcon message="Private route must be ready"
        description={<>Publish <code>{pending ? privateHost(pending.id) : ''}</code> on the PolyGate Cloudflare Tunnel to the existing app gateway. PolyGate checks that exact route before changing access.</>}
        style={{ marginBottom: 12 }} />}
      <Text>Changing access ends existing private app sessions. Enter a reason for the audit record.</Text>
      <Input.TextArea aria-label="Access change reason" value={reason} onChange={event => setReason(event.target.value)}
        maxLength={500} showCount rows={3} style={{ marginTop: 12 }} />
    </Modal>
  </>
}
