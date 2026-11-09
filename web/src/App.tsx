import { useEffect, useRef, useState } from 'react'
import { useInfiniteQuery, useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Alert, Button, Card, Form, Input, InputNumber, Modal, Popconfirm, Progress, Select, Space, Spin, Switch, Table, Typography, message } from 'antd'
import { NavLink, Route, Routes, useNavigate, useSearchParams } from 'react-router-dom'
import { useTranslation } from 'react-i18next'
import { ApiError, api, type AccountActualUsage, type App as AppRecord, type AppEvent, type AppSpec, type BuildTemplate, type Me, type Operation, type PodMetric, type Resources, type SourceBuild, type Volume } from './api'
import ImagePage from './ImagePage'
import AdminAccountsPage from './AdminAccountsPage'
import AdminResourcesPage from './AdminResourcesPage'
import ActivityPage from './ActivityPage'
import HelpPage from './HelpPage'

const { Title, Text, Paragraph } = Typography

function appAccessURL(base: string, appID: string): string {
  const url = new URL(base)
  url.hostname = `${appID}.${url.hostname}`
  url.pathname = '/'
  return url.toString()
}

function Shell({ children }: { children: React.ReactNode }) {
  const { t } = useTranslation()
  const me = useQuery({ queryKey: ['me'], queryFn: api.me })
  if (me.error instanceof ApiError && me.error.code === 'UNAUTHENTICATED') {
    return <div className="login-page"><Card className="login-card"><div className="brand-mark"><img src="/polyu-logo.webp" alt="PolyU logo" /></div><Title level={2}>Welcome to PolyGate</Title><Paragraph type="secondary">Sign in with your PolyGate student account to continue.</Paragraph><Button type="primary" size="large" onClick={() => { window.location.href = '/auth/login' }}>Sign in</Button></Card></div>
  }
  return <div className="shell">
    <aside className="sidebar">
      <div className="brand"><span className="brand-mark"><img src="/polyu-logo.webp" alt="PolyU logo" /></span><span><strong>{t('product')}</strong><small>{t('subtitle')}</small></span></div>
      <nav aria-label="Main navigation">
        <NavLink end to="/">{t('overview')}</NavLink>
        <NavLink to="/apps/new">{t('newApp')}</NavLink>
        <NavLink to="/images">My images</NavLink>
        {me.data?.secretAvailable && <NavLink to="/secrets">Secrets</NavLink>}
        {me.data?.volumeAvailable && <NavLink to="/volumes">Volumes</NavLink>}
        <NavLink to="/source">{t('source')}</NavLink>
        <NavLink to="/activity">Activity</NavLink>
        <NavLink to="/help">Help</NavLink>
        {me.data?.role === 'admin' && <NavLink to="/admin/accounts">Accounts</NavLink>}
        {me.data?.role === 'admin' && <NavLink to="/admin/cluster">Cluster</NavLink>}
      </nav>
      <div className="sidebar-foot">{me.data?.mode === 'authenticated' ? <Button size="small" ghost onClick={() => { void api.logout().then(() => { window.location.href = '/auth/oidc-logout' }).catch((error: unknown) => { window.alert(error instanceof Error ? error.message : 'Could not sign out. Please try again.') }) }}>Sign out</Button> : <><span className="status-dot" />{t('localMode')}</>}</div>
    </aside>
    <main className="main">
      <header className="topbar"><div className="breadcrumb">Workspace <span>/</span> {me.data?.role === 'admin' ? 'Administrator' : 'Student'}</div><div className="avatar">{me.data?.displayName.slice(0, 1) ?? 'S'}</div></header>
      <div className="content-scroll">
        <div className="content">
          {me.data?.mode !== 'authenticated' && <Alert className="mode-alert" type="info" showIcon message={t('localMode')} description={t('localDetail')} />}
          {children}
        </div>
      </div>
    </main>
  </div>
}

function Metric({ label, current, maximum, unit }: { label: string; current: number; maximum: number; unit: string }) {
  const format = (value: number) => new Intl.NumberFormat('en-GB', { maximumFractionDigits: 2 }).format(value)
  const remaining = maximum - current
  return <div className="metric">
    <div className="metric-heading"><Text>{label}</Text><Text type="secondary">{format(current)} / {format(maximum)} {unit}</Text></div>
    <Progress percent={maximum ? Math.min(100, Math.round(current / maximum * 100)) : 0} showInfo={false} strokeColor="#8f1329" trailColor="#f2e5e8" />
    <Text className="metric-remaining" type={remaining < 0 ? 'danger' : 'secondary'}>{remaining < 0 ? `${format(-remaining)} ${unit} over quota` : `${format(remaining)} ${unit} available`}</Text>
  </div>
}

function ResourceCard({ me, fetchedAt, actual, actualError, volumes, volumeError, volumeFetchedAt }: {
  me: Me; fetchedAt: number; actual?: AccountActualUsage; actualError?: string
  volumes?: Volume[]; volumeError?: string; volumeFetchedAt?: number
}) {
  const { t } = useTranslation()
  return <Card className="resource-card" title={t('quota')} extra={<Text type="secondary">{t('capacity')}</Text>}>
    <Metric label="Applications" current={me.configured.apps} maximum={me.quota.apps} unit="apps" />
    <Metric label="Pods" current={me.configured.pods} maximum={me.quota.pods} unit="pods" />
    <Metric label="Memory" current={me.configured.memoryMiB / 1024} maximum={me.quota.memoryMiB / 1024} unit="GiB" />
    <Metric label="CPU requests" current={me.configured.cpuRequestMilli} maximum={me.quota.cpuRequestMilli} unit="m" />
    <Metric label="CPU limits" current={me.configured.cpuLimitMilli} maximum={me.quota.cpuLimitMilli} unit="m" />
    {me.volumeAvailable && (volumeError ? <Text type="warning">Storage quota unavailable: {volumeError}</Text> : volumes ? <>
      <Metric label="Volumes" current={volumes.length} maximum={2} unit="volumes" />
      <Metric label="Storage allocated" current={volumes.reduce((total, volume) => total + volume.spec.sizeGiB, 0)} maximum={10} unit="GiB" />
      <Text type="secondary">Allocated sizes count against your quota until deletion completes. Checked {new Date(volumeFetchedAt ?? fetchedAt).toLocaleString('en-GB')}.</Text>
    </> : <Text type="secondary">Loading storage quota...</Text>)}
    <Text className="resource-note" type="secondary">Configured resources include drafts and stopped apps. Available quota does not guarantee free cluster capacity. Fetched {new Date(fetchedAt).toLocaleString('en-GB')}.</Text>
    <div className="resource-actual">
      <Text strong>Actual running usage</Text>
      {actualError ? <Text type="warning">Metrics unavailable: {actualError}</Text> : actual ? <>
        <Text>{actual.cpuMilli} m CPU · {(actual.memoryBytes / 1073741824).toFixed(2)} GiB memory</Text>
        <Text type="secondary">{actual.sampledPods} / {actual.expectedPods} expected Pods sampled · Checked {new Date(actual.checkedAt).toLocaleString('en-GB')}</Text>
        {!actual.complete && <Text type="warning">Some running Pod samples are not available yet.</Text>}
      </> : <Text type="secondary">Loading usage samples...</Text>}
    </div>
  </Card>
}

function StatusCell({ app, enabled, latestUpdate }: { app: AppRecord; enabled: boolean; latestUpdate?: Operation }) {
  const details = useQuery({
    queryKey: ['app-status', app.id],
    queryFn: () => api.appStatus(app.id),
    enabled: enabled && app.status !== 'Draft',
    refetchInterval: enabled ? 5000 : false,
  })
  return <Space direction="vertical" size={0}>
    <span className="draft-pill">{app.status}</span>
    {details.data?.errorCode && <Text type="secondary" style={{ fontSize: 12 }}>{details.data.errorCode.replaceAll('_', ' ').toLowerCase()}</Text>}
    {latestUpdate?.state === 'Failed' && <Text type="danger" style={{ fontSize: 12 }}>
      {latestUpdate.kind === 'RESOURCE_UPDATE_REQUEST' ? 'Resource update failed; previous settings restored' : latestUpdate.kind === 'RESTART_REQUEST' ? 'Restart failed; previous workload restored' : 'Image update failed; previous version restored'}{latestUpdate.detail?.errorCode ? ` (${latestUpdate.detail.errorCode.replaceAll('_', ' ').toLowerCase()})` : ''}.
    </Text>}
  </Space>
}

function Overview() {
  const { t } = useTranslation()
  const [searchParams, setSearchParams] = useSearchParams()
  const sourceArtifactId = searchParams.get('artifactId')
  const me = useQuery({ queryKey: ['me'], queryFn: api.me, refetchInterval: 15000 })
  const actualUsage = useQuery({ queryKey: ['account-actual-usage'], queryFn: api.accountActualUsage,
    enabled: me.data?.mode === 'authenticated', refetchInterval: 15000, retry: 1 })
  const accountVolumes = useQuery({ queryKey: ['volumes'], queryFn: api.volumes,
    enabled: me.data?.volumeAvailable, refetchInterval: 15000, retry: 1 })
  const apps = useQuery({ queryKey: ['apps'], queryFn: api.apps, refetchInterval: me.data?.mode === 'authenticated' ? 5000 : false })
  const operations = useQuery({ queryKey: ['operations'], queryFn: api.operations,
    enabled: me.data?.mode === 'authenticated',
    refetchInterval: me.data?.mode === 'authenticated' ? 5000 : false })
  const images = useQuery({ queryKey: ['images'], queryFn: api.images, enabled: me.data?.imageDeploymentAvailable })
  const query = useQueryClient()
  const [toast, context] = message.useMessage()
  const [imageAppId, setImageAppId] = useState<string | null>(null)
  const [imageAction, setImageAction] = useState<'deploy' | 'update'>('deploy')
  const [rollbackAppId, setRollbackAppId] = useState<string | null>(null)
  const [rollbackRevisionId, setRollbackRevisionId] = useState<string | undefined>()
  const [editingApp, setEditingApp] = useState<AppRecord | null>(null)
  const [editingRuntime, setEditingRuntime] = useState<AppRecord | null>(null)
  const [restartAppId, setRestartAppId] = useState<string | null>(null)
  const [restartStrategy, setRestartStrategy] = useState<'Rolling' | 'Recreate'>('Recreate')
  const [resourceForm] = Form.useForm<{ resources: Resources; strategy: 'Rolling' | 'Recreate' }>()
  const [runtimeForm] = Form.useForm<{ port: number; healthPath: string }>()
  const revisions = useQuery({ queryKey: ['revisions', rollbackAppId], queryFn: () => api.appRevisions(rollbackAppId!), enabled: rollbackAppId !== null })
  const [logsApp, setLogsApp] = useState<AppRecord | null>(null)
  const [selectedLogPod, setSelectedLogPod] = useState('')
  const [historyHours, setHistoryHours] = useState<24 | 168>(24)
  const [historyPod, setHistoryPod] = useState('')
  const [knownHistoryPods, setKnownHistoryPods] = useState<string[]>([])
  const appPods = useQuery({ queryKey: ['appPods', logsApp?.id], queryFn: () => api.appPods(logsApp!.id),
    enabled: logsApp !== null && logsApp.status !== 'Stopped',
    refetchInterval: logsApp !== null && logsApp.status !== 'Stopped' ? 5000 : false })
  const activeLogPod = appPods.data?.some(pod => pod.name === selectedLogPod) ? selectedLogPod : undefined
  const diagnosticPod = appPods.data?.find(pod => pod.name === activeLogPod) ?? appPods.data?.[0]
  const podDiagnostic = diagnosticPod ? [
    diagnosticPod.waitingReason && `Waiting: ${diagnosticPod.waitingReason}`,
    diagnosticPod.reason && `Pod: ${diagnosticPod.reason}`,
    diagnosticPod.message,
    diagnosticPod.lastTerminationReason && `Previous exit: ${diagnosticPod.lastTerminationReason}${diagnosticPod.lastExitCode === undefined ? '' : ` (code ${diagnosticPod.lastExitCode})`}`,
  ].filter(Boolean).join(' · ') : ''
  const appLogs = useQuery({ queryKey: ['appLogs', logsApp?.id, activeLogPod],
    queryFn: () => api.appLogs(logsApp!.id, activeLogPod),
    enabled: logsApp !== null && logsApp.status !== 'Stopped' && (appPods.data?.length ?? 0) > 0,
    refetchInterval: logsApp !== null && logsApp.status !== 'Stopped' ? 5000 : false })
  const appLogHistory = useInfiniteQuery({ queryKey: ['appLogHistory', logsApp?.id, historyHours, historyPod],
    queryFn: ({ pageParam }) => api.appLogHistory(logsApp!.id, historyHours, pageParam, historyPod),
    initialPageParam: '', getNextPageParam: page => page.nextBefore || undefined,
    enabled: logsApp !== null && me.data?.appLogHistoryAvailable === true, refetchOnWindowFocus: false })
  useEffect(() => { setHistoryPod(''); setKnownHistoryPods([]) }, [logsApp?.id])
  useEffect(() => {
    if (!logsApp) return
    const discovered = [...(appPods.data ?? []).map(pod => pod.name),
      ...(appLogHistory.data?.pages.flatMap(page => page.items.map(item => item.pod)) ?? [])].filter(Boolean)
    setKnownHistoryPods(previous => {
      const next = [...new Set([...previous, ...discovered])].sort()
      return next.length === previous.length && next.every((pod, index) => pod === previous[index]) ? previous : next
    })
  }, [logsApp, appPods.data, appLogHistory.data])
  const appEvents = useQuery({ queryKey: ['appEvents', logsApp?.id], queryFn: () => api.appEvents(logsApp!.id),
    enabled: logsApp !== null, refetchInterval: logsApp !== null ? 5000 : false })
  const appMetrics = useQuery({ queryKey: ['appMetrics', logsApp?.id], queryFn: () => api.appMetrics(logsApp!.id),
    enabled: logsApp !== null, refetchInterval: logsApp !== null ? 15000 : false })
  const [artifactId, setArtifactId] = useState<string | undefined>()
  const deleting = useMutation({ mutationFn: api.deleteApp, onSuccess: async () => { await Promise.all([query.invalidateQueries({ queryKey: ['me'] }), query.invalidateQueries({ queryKey: ['apps'] })]); toast.success('App deleted') }, onError: (error: Error) => toast.error(error.message) })
  const deploying = useMutation({ mutationFn: api.deployApp, onSuccess: async () => { await query.invalidateQueries({ queryKey: ['apps'] }); toast.success('Trusted sample deployment queued') }, onError: (error: Error) => toast.error(error.message) })
  const deployingImage = useMutation({ mutationFn: api.deployImage, onSuccess: async () => { await Promise.all([query.invalidateQueries({ queryKey: ['apps'] }), query.invalidateQueries({ queryKey: ['me'] })]); setImageAppId(null); setArtifactId(undefined); toast.success('Approved image deployment queued') }, onError: (error: Error) => toast.error(error.message) })
  const updatingImage = useMutation({ mutationFn: api.updateImage, onSuccess: async () => { await Promise.all([query.invalidateQueries({ queryKey: ['apps'] }), query.invalidateQueries({ queryKey: ['me'] })]); setImageAppId(null); setArtifactId(undefined); setSearchParams({}); toast.success('Image update queued') }, onError: (error: Error) => toast.error(error.message) })
  const rollingBack = useMutation({ mutationFn: api.rollbackRevision, onSuccess: async () => { await Promise.all([query.invalidateQueries({ queryKey: ['apps'] }), query.invalidateQueries({ queryKey: ['revisions', rollbackAppId] })]); setRollbackAppId(null); setRollbackRevisionId(undefined); toast.success('Version restore queued') }, onError: (error: Error) => toast.error(error.message) })
  const editingResources = useMutation({ mutationFn: api.updateAppResources, onSuccess: async () => { await Promise.all([query.invalidateQueries({ queryKey: ['apps'] }), query.invalidateQueries({ queryKey: ['me'] })]); setEditingApp(null); toast.success('Resource settings saved') }, onError: (error: Error) => toast.error(error.message) })
  const editingRuntimeMutation = useMutation({ mutationFn: api.updateAppRuntime, onSuccess: async () => { await Promise.all([query.invalidateQueries({ queryKey: ['apps'] }), query.invalidateQueries({ queryKey: ['operations'] })]); setEditingRuntime(null); toast.success('App port and health path saved. Deploy the image again to retry.') }, onError: (error: Error) => toast.error(error.message) })
  const updatingResources = useMutation({ mutationFn: api.updateRunningResources, onSuccess: async () => { await query.invalidateQueries({ queryKey: ['apps'] }); setEditingApp(null); toast.success('Resource update queued') }, onError: (error: Error) => toast.error(error.message) })
  const restarting = useMutation({ mutationFn: api.restartApp, onSuccess: async () => { await query.invalidateQueries({ queryKey: ['apps'] }); setRestartAppId(null); toast.success('Restart queued') }, onError: (error: Error) => toast.error(error.message) })
  const stopping = useMutation({ mutationFn: api.stopApp, onSuccess: async () => { await query.invalidateQueries({ queryKey: ['apps'] }); toast.success('Stop requested') }, onError: (error: Error) => toast.error(error.message) })
  if (me.isPending || apps.isPending) return <div className="center"><Spin size="large" /></div>
  if (me.error || apps.error) return <Alert type="error" showIcon message="Could not load your workspace" description={(me.error ?? apps.error)?.message} />
  const columns = [
    { title: t('appName'), dataIndex: ['spec', 'name'], key: 'name', render: (name: string) => <strong>{name}</strong> },
    { title: 'Draft image', dataIndex: ['spec', 'image'], key: 'image', ellipsis: true },
    { title: t('status'), dataIndex: 'status', key: 'status', render: (_: unknown, item: AppRecord) => <StatusCell app={item} enabled={me.data.mode === 'authenticated'} latestUpdate={operations.data?.find(operation => operation.appId === item.id && (operation.kind === 'UPDATE_REQUEST' || operation.kind === 'ROLLBACK_REQUEST' || operation.kind === 'RESOURCE_UPDATE_REQUEST' || operation.kind === 'RESTART_REQUEST'))} /> },
    { title: t('limitMemory'), key: 'memory', render: (_: unknown, item: AppRecord) => `${item.spec.resources.memoryMiB / 1024} GiB` },
    { title: t('createdAt'), dataIndex: 'createdAt', key: 'createdAt', render: (date: string) => new Date(date).toLocaleString('en-GB') },
    { title: 'Actions', key: 'actions', width: 300, render: (_: unknown, item: AppRecord) => <Space wrap size={[0, 0]}>
      {(me.data.appLogsAvailable && ['Queued', 'Deploying', 'Running', 'Failed'].includes(item.status) ||
        me.data.appLogHistoryAvailable && item.status === 'Stopped') &&
        <Button type="link" size="small" onClick={() => { setSelectedLogPod(''); setLogsApp(item) }}>Logs</Button>}
      {me.data.trustedDeploymentAvailable && (item.status === 'Draft' || item.status === 'Stopped' || item.status === 'Failed') &&
        <Button type="link" size="small" loading={deploying.isPending && deploying.variables === item.id} onClick={() => deploying.mutate(item.id)}>Deploy sample</Button>}
      {me.data.imageDeploymentAvailable && (item.status === 'Draft' || item.status === 'Stopped' || item.status === 'Failed') &&
        <Button type="link" size="small" onClick={() => { setArtifactId(undefined); setImageAction('deploy'); setImageAppId(item.id) }}>Deploy image</Button>}
      {me.data.imageDeploymentAvailable && item.status === 'Running' &&
        <Button type="link" size="small" onClick={() => { setArtifactId(sourceArtifactId ?? undefined); setImageAction('update'); setImageAppId(item.id) }}>Update image</Button>}
      {me.data.imageDeploymentAvailable && item.status === 'Running' &&
        <Button type="link" size="small" onClick={() => { setRollbackRevisionId(undefined); setRollbackAppId(item.id) }}>Restore version</Button>}
      {me.data.appAccessBaseURL && item.status === 'Running' &&
        <Button type="link" size="small" href={appAccessURL(me.data.appAccessBaseURL, item.id)} target="_blank" rel="noopener noreferrer">Open app</Button>}
      {item.status === 'Running' &&
        <Button type="link" size="small" onClick={() => { resourceForm.setFieldsValue({ resources: item.spec.resources, strategy: item.spec.persistent ? 'Recreate' : 'Rolling' }); setEditingApp(item) }}>Change resources</Button>}
      {item.status === 'Running' &&
        <Button type="link" size="small" onClick={() => { setRestartStrategy('Recreate'); setRestartAppId(item.id) }}>Restart</Button>}
      {(item.status === 'Draft' || item.status === 'Stopped') &&
        <Button type="link" size="small" onClick={() => { resourceForm.setFieldsValue({ resources: item.spec.resources, strategy: 'Rolling' }); setEditingApp(item) }}>Edit resources</Button>}
      {['Draft', 'Stopped', 'Failed'].includes(item.status) &&
        <Button type="link" size="small" onClick={() => { runtimeForm.setFieldsValue({ port: item.spec.port, healthPath: item.spec.healthPath ?? '' }); setEditingRuntime(item) }}>Edit port</Button>}
      {['Queued', 'Deploying', 'Running'].includes(item.status) &&
        <Popconfirm title={item.status === 'Running' ? 'Stop this app?' : 'Cancel this deployment?'} description={item.spec.persistent ? 'The app workload will be removed. Its volume and data will remain available.' : 'The app workload will be removed and its reserved resources released after cleanup.'} okText={item.status === 'Running' ? 'Stop' : 'Cancel deployment'} cancelText="Back" onConfirm={() => stopping.mutate(item.id)}><Button type="link" size="small" loading={stopping.isPending && stopping.variables === item.id}>{item.status === 'Running' ? 'Stop' : 'Cancel deployment'}</Button></Popconfirm>}
      {(item.status === 'Draft' || item.status === 'Stopped' || item.status === 'Failed') &&
        <Popconfirm title="Delete this app?" description={item.spec.persistent ? 'The app will be deleted. Its volume and data will remain available.' : 'This action cannot be undone.'} okText="Delete" cancelText="Cancel" onConfirm={() => deleting.mutate(item.id)}><Button type="link" danger size="small">Delete</Button></Popconfirm>}
    </Space> },
  ]
  return <>
    {context}
    <div className="page-heading"><div><span className="eyebrow">STUDENT CONSOLE</span><Title level={2}>{t('title')}</Title><Paragraph type="secondary">{t('intro')}</Paragraph></div><NavLink to="/apps/new"><Button type="primary" size="large">+ {t('newApp')}</Button></NavLink></div>
    {me.data.trustedDeploymentAvailable && <Alert type="info" showIcon style={{ marginBottom: 20 }} message="Trusted sample deployment" description="Deploy sample runs a platform-approved image on port 5678. The image and port entered in your draft are not used for this test." />}
    {me.data.imageDeploymentAvailable && <Alert type="info" showIcon style={{ marginBottom: 20 }} message="Approved image deployment" description="Submit an image on My images, wait for Approved, then choose it when deploying an app. The app's saved resources and port apply." />}
    {sourceArtifactId && <Alert type="info" showIcon style={{ marginBottom: 20 }} message="Approved source build selected" description="Choose Update image on a running app to deploy this build as its next version. The app's existing port and resource settings will apply." />}
    <Modal title="Restart app" open={restartAppId !== null} onCancel={() => setRestartAppId(null)}
      onOk={() => { if (restartAppId) restarting.mutate({ appId: restartAppId, strategy: restartStrategy }) }}
      okText="Restart" confirmLoading={restarting.isPending}>
      <Paragraph>Restart replaces the app Pod without changing its image, settings or volume data.</Paragraph>
      <Select aria-label="Restart strategy" style={{ width: '100%' }} value={restartStrategy}
        onChange={setRestartStrategy} options={[
          { value: 'Recreate', label: 'Recreate — brief interruption, no extra quota' },
          ...(!apps.data?.find(item => item.id === restartAppId)?.spec.persistent ? [{ value: 'Rolling', label: 'Rolling — keep app available, requires extra quota' }] : []),
        ]} />
    </Modal>
    <Modal title={imageAction === 'update' ? 'Update to an approved image' : 'Deploy an approved image'} open={imageAppId !== null} onCancel={() => { setImageAppId(null); setArtifactId(undefined) }} onOk={() => { if (imageAppId && artifactId) (imageAction === 'update' ? updatingImage : deployingImage).mutate({ appId: imageAppId, artifactId }) }} okText={imageAction === 'update' ? 'Update image' : 'Deploy image'} okButtonProps={{ disabled: !artifactId || deployingImage.isPending || updatingImage.isPending }} confirmLoading={deployingImage.isPending || updatingImage.isPending}>
      <Paragraph>{imageAction === 'update' ? (apps.data?.find(item => item.id === imageAppId)?.spec.persistent ? 'Choose a different approved image. This app uses a volume, so the old Pod must stop before the new one starts. Expect a short interruption. If the update fails, PolyGate restores the previous image.' : 'Choose a different approved image. The existing version stays available during the rolling update; one extra Pod must fit within your quota. If the update fails, PolyGate restores the previous image.') : "Choose one of your approved images. PolyGate uses its pinned private digest and the app's saved port and resource settings."}</Paragraph>
      {images.error && <Alert type="error" showIcon message="Could not load approved images" description={images.error.message} />}
      <Select aria-label="Approved image" showSearch optionFilterProp="label" style={{ width: '100%' }} placeholder="Select an approved image" value={artifactId} loading={images.isPending} onChange={setArtifactId} options={images.data?.filter(item => item.scanStatus === 'Approved').map(item => ({ value: item.id, label: item.submittedReference })) ?? []} />
      {images.data && !images.data.some(item => item.scanStatus === 'Approved') && <Alert type="warning" showIcon style={{ marginTop: 16 }} message="No approved images yet" description={<span>Submit an image in <NavLink to="/images" onClick={() => setImageAppId(null)}>My images</NavLink> and wait for the security check.</span>} />}
    </Modal>
    <Modal title="Restore a previous version" open={rollbackAppId !== null} onCancel={() => { setRollbackAppId(null); setRollbackRevisionId(undefined) }} onOk={() => { if (rollbackAppId && rollbackRevisionId) rollingBack.mutate({ appId: rollbackAppId, revisionId: rollbackRevisionId }) }} okText="Restore version" okButtonProps={{ disabled: !rollbackRevisionId || rollingBack.isPending }} confirmLoading={rollingBack.isPending}>
      <Paragraph>{apps.data?.find(item => item.id === rollbackAppId)?.spec.persistent ? 'Choose a previous successful approved image. The old Pod stops before this version starts, so expect a short interruption. PolyGate keeps the volume and its data.' : 'Choose a previous successful approved image. PolyGate creates a new revision and rolls it out with one extra Pod. Application data is not changed.'}</Paragraph>
      {revisions.error && <Alert type="error" showIcon message="Could not load version history" description={revisions.error.message} />}
      <Select aria-label="Previous version" style={{ width: '100%' }} loading={revisions.isPending} placeholder="Select a previous version" value={rollbackRevisionId} onChange={setRollbackRevisionId} options={revisions.data?.filter(item => item.successful && !item.current && item.sourceKind === 'image' && item.imageArtifactId).map(item => ({ value: item.id, label: `Version ${item.number} · ${new Date(item.createdAt).toLocaleString('en-GB')}` })) ?? []} />
      {revisions.data && !revisions.data.some(item => item.successful && !item.current && item.sourceKind === 'image' && item.imageArtifactId) && <Alert type="info" showIcon style={{ marginTop: 16 }} message="No restorable version" description="A previous successful approved image is required." />}
    </Modal>
    <Modal title={editingApp ? `${editingApp.status === 'Running' ? 'Change' : 'Edit'} resources: ${editingApp.spec.name}` : 'Edit resources'} open={editingApp !== null} onCancel={() => setEditingApp(null)} onOk={() => resourceForm.submit()} okText={editingApp?.status === 'Running' ? 'Update resources' : 'Save resources'} confirmLoading={editingResources.isPending || updatingResources.isPending}>
      <Paragraph>{editingApp?.status === 'Running' ? 'Rolling keeps the current version available while the new Pods become ready and needs room for both sets of resources. Choose Recreate if capacity is insufficient; it stops the old Pods first and causes a short interruption. A failed change restores the previous settings.' : 'Save the CPU, memory and replica settings for the next deployment.'}</Paragraph>
      <Form<{ resources: Resources; strategy: 'Rolling' | 'Recreate' }> form={resourceForm} layout="vertical" onFinish={values => { if (!editingApp) return; if (editingApp.status === 'Running') updatingResources.mutate({ appId: editingApp.id, resources: values.resources, strategy: editingApp.spec.persistent ? 'Recreate' : values.strategy }); else editingResources.mutate({ appId: editingApp.id, resources: values.resources }) }}>
        <Form.Item label="CPU request (m)" name={['resources', 'cpuRequestMilli']} rules={[{ required: true }]}><InputNumber min={100} max={2000} step={50} style={{ width: '100%' }} /></Form.Item>
        <Form.Item label="CPU limit (m)" name={['resources', 'cpuLimitMilli']} rules={[{ required: true }]}><InputNumber min={100} max={2000} step={50} style={{ width: '100%' }} /></Form.Item>
        <Form.Item label="Memory (MiB)" name={['resources', 'memoryMiB']} rules={[{ required: true }]}><Select options={[128, 256, 512, 1024, 2048, 3072, 4096].map(n => ({ value: n, label: `${n} MiB` }))} /></Form.Item>
        <Form.Item label="Replicas" name={['resources', 'replicas']} rules={[{ required: true }]}><Select options={(editingApp?.spec.persistent ? [1] : [1, 2]).map(n => ({ value: n, label: String(n) }))} /></Form.Item>
        {editingApp?.status === 'Running' && !editingApp.spec.persistent && <Form.Item label="Replacement strategy" name="strategy" rules={[{ required: true }]}><Select options={[{ value: 'Rolling', label: 'Rolling (keep serving)' }, { value: 'Recreate', label: 'Recreate (short interruption)' }]} /></Form.Item>}
        {editingApp?.status === 'Running' && editingApp.spec.persistent && <Alert type="info" showIcon message="Persistent volume update" description="The old Pod stops before the new Pod mounts the volume. Expect a short interruption; volume data is retained." />}
      </Form>
    </Modal>
    <Modal title={editingRuntime ? `Edit app port: ${editingRuntime.spec.name}` : 'Edit app port'} open={editingRuntime !== null} onCancel={() => setEditingRuntime(null)} onOk={() => runtimeForm.submit()} okText="Save settings" confirmLoading={editingRuntimeMutation.isPending}>
      <Paragraph>Enter the port your container actually listens on. The Python template uses 8000; the Node.js template uses 3000. Save, then choose <strong>Deploy image</strong> to retry a failed app.</Paragraph>
      <Form<{ port: number; healthPath: string }> form={runtimeForm} layout="vertical" onFinish={values => { if (editingRuntime) editingRuntimeMutation.mutate({ appId: editingRuntime.id, port: values.port, healthPath: values.healthPath ?? '' }) }}>
        <Form.Item label="App port" name="port" rules={[{ required: true, message: 'Enter the listening port.' }]}><InputNumber min={1} max={65535} style={{ width: '100%' }} /></Form.Item>
        <Form.Item label="HTTP health path (optional)" name="healthPath" rules={[{ pattern: /^\/(?!\/)(?!.*\.\.)(?:[A-Za-z0-9/_\.~-]*)$/, message: 'Enter an absolute path without spaces, query or fragment.' }, { max: 128 }]}><Input placeholder="/health" maxLength={128} /></Form.Item>
      </Form>
    </Modal>
    <Modal title={logsApp ? `Diagnostics: ${logsApp.spec.name}` : 'Application diagnostics'} open={logsApp !== null}
      onCancel={() => setLogsApp(null)} footer={<Button onClick={() => { if (logsApp) { query.invalidateQueries({ queryKey: ['appPods', logsApp.id] }); query.invalidateQueries({ queryKey: ['appLogs', logsApp.id] }); query.invalidateQueries({ queryKey: ['appLogHistory', logsApp.id] }); query.invalidateQueries({ queryKey: ['appEvents', logsApp.id] }); query.invalidateQueries({ queryKey: ['appMetrics', logsApp.id] }) } }}>Refresh</Button>} width={800}>
      <Title level={5}>Current resource usage</Title>
      <Paragraph type="secondary">Recent CPU and memory samples for this app's running Pods. Updates every 15 seconds.</Paragraph>
      {appMetrics.error && <Alert type="warning" showIcon message="Resource usage unavailable" description={appMetrics.error.message} />}
      {!appMetrics.error && <Table<PodMetric> rowKey="pod" size="small" pagination={false} loading={appMetrics.isPending}
        dataSource={appMetrics.data ?? []} locale={{ emptyText: 'No resource sample is available yet.' }}
        columns={[{ title: 'Pod', dataIndex: 'pod', key: 'pod', ellipsis: true },
          { title: 'CPU', dataIndex: 'cpuMilli', key: 'cpu', render: (value: number) => `${value} m` },
          { title: 'Memory', dataIndex: 'memoryBytes', key: 'memory', render: (value: number) => `${(value / 1048576).toFixed(1)} MiB` },
          { title: 'Sampled', dataIndex: 'timestamp', key: 'timestamp', render: (value: string) => value ? new Date(value).toLocaleString('en-GB') : 'Unavailable' }]} />}
      {logsApp?.status !== 'Stopped' && <><Title level={5}>Current container logs</Title>
        <Paragraph type="secondary">Latest 200 lines, up to 128 KiB. Refreshes every five seconds while open.</Paragraph>
        {appPods.error && <Alert type="error" showIcon message="Could not list application Pods" description={appPods.error.message} />}
        {appPods.isPending && <Spin />}
        {appPods.data && appPods.data.length > 0 && <Select aria-label="Container Pod" value={activeLogPod ?? ''}
          onChange={setSelectedLogPod} style={{ width: '100%', marginBottom: 12 }}
          options={[{ value: '', label: 'Latest Pod' }, ...appPods.data.map(pod => ({ value: pod.name,
            label: `${pod.name} · ${pod.ready ? 'Ready' : pod.phase || 'Not ready'} · ${pod.restarts} restarts` }))]} />}
        {diagnosticPod && podDiagnostic && <Alert type="warning" showIcon style={{ marginBottom: 12 }}
          message={`${diagnosticPod.name}: ${diagnosticPod.ready ? 'Ready' : diagnosticPod.phase || 'Not ready'}; ${diagnosticPod.restarts} restarts`}
          description={podDiagnostic} />}
        {appPods.data?.length === 0 && <Paragraph type="secondary">No current Pod. Check historical logs and recent events below.</Paragraph>}
        {appLogs.error && <Alert type="error" showIcon message="Could not load application logs" description={appLogs.error.message} />}
        {appLogs.isPending && <Spin />}
        {appLogs.data && <><Text type="secondary">Pod: {appLogs.data.pod}</Text><pre style={{ maxHeight: 440, overflow: 'auto', padding: 12, background: '#111827', color: '#f9fafb', whiteSpace: 'pre-wrap', overflowWrap: 'anywhere' }}>{appLogs.data.data || 'No output yet.'}</pre></>}</>}
      {me.data?.appLogHistoryAvailable && <><Title level={5}>Historical container logs</Title>
        <Paragraph type="secondary">Up to seven days of logs. Each page shows up to 200 lines; load older pages to discover removed Pods. Select a Pod to filter its history. Refresh to load new lines.</Paragraph>
        <Select aria-label="Historical log range" value={historyHours} onChange={setHistoryHours} options={[{ value: 24, label: 'Last 24 hours' }, { value: 168, label: 'Last 7 days' }]} style={{ width: 160, marginBottom: 12 }} />
        <Select aria-label="Historical Pod" showSearch optionFilterProp="label" value={historyPod} onChange={setHistoryPod}
          options={[{ value: '', label: 'All Pods' }, ...knownHistoryPods.map(pod => ({ value: pod, label: pod }))]}
          style={{ width: 360, maxWidth: '100%', marginBottom: 12, marginLeft: 8 }} />
        {appLogHistory.error && <Alert type="error" showIcon message="Could not load historical logs" description={appLogHistory.error.message} />}
        {appLogHistory.isPending && <Spin />}
        {appLogHistory.data && <><pre style={{ maxHeight: 440, overflow: 'auto', padding: 12, background: '#111827', color: '#f9fafb', whiteSpace: 'pre-wrap', overflowWrap: 'anywhere' }}>
          {appLogHistory.data.pages.some(page => page.items.length) ? appLogHistory.data.pages.flatMap(page => page.items).map(item => `${item.timestamp} ${item.pod} ${item.line}`).join('\n') : 'No stored logs in this range.'}
        </pre>{appLogHistory.hasNextPage && <Button onClick={() => void appLogHistory.fetchNextPage()} loading={appLogHistory.isFetchingNextPage}>Load older logs</Button>}</>}</>}
      <Title level={5}>Recent events</Title>
      {appEvents.error && <Alert type="error" showIcon message="Could not load application events" description={appEvents.error.message} />}
      <Table<AppEvent> rowKey={(_, index) => String(index)} size="small" pagination={false} loading={appEvents.isPending}
        dataSource={appEvents.data ?? []} locale={{ emptyText: 'No events recorded for this app.' }}
        columns={[{ title: 'Time', dataIndex: 'at', key: 'at', render: (value: string) => new Date(value).toLocaleString('en-GB') },
          { title: 'Type', dataIndex: 'type', key: 'type' }, { title: 'Reason', dataIndex: 'reason', key: 'reason' },
          { title: 'Object', dataIndex: 'object', key: 'object', ellipsis: true },
          { title: 'Message', dataIndex: 'message', key: 'message', ellipsis: true }]} />
    </Modal>
    <div className="overview-grid"><ResourceCard me={me.data} fetchedAt={me.dataUpdatedAt} actual={actualUsage.data} actualError={actualUsage.error?.message} volumes={accountVolumes.data} volumeError={accountVolumes.error?.message} volumeFetchedAt={accountVolumes.dataUpdatedAt} /><Card title="Account" className="account-card"><div className="account-badge">LS</div><Title level={4}>{me.data.displayName}</Title><Text type="secondary">Student account</Text><div className="account-line"><span>Account ID</span><code>{me.data.id}</code></div><div className="account-line"><span>Access</span><span>Personal workspace</span></div></Card></div>
    <Card className="apps-card" title={t('apps')} extra={<NavLink to="/apps/new">Create app</NavLink>}><Table<AppRecord> rowKey="id" columns={columns} dataSource={apps.data} pagination={false} locale={{ emptyText: <div className="empty-table"><div className="empty-icon">▣</div><strong>{t('emptyApps')}</strong><p>Create an app draft to see it here.</p></div> }} /></Card>
  </>
}

function NewApp() {
  const { t } = useTranslation()
  const me = useQuery({ queryKey: ['me'], queryFn: api.me })
  const volumes = useQuery({ queryKey: ['volumes'], queryFn: api.volumes, enabled: me.data?.volumeAvailable })
  const apps = useQuery({ queryKey: ['apps'], queryFn: api.apps, enabled: me.data?.volumeAvailable })
  const [form] = Form.useForm<AppSpec>()
  const navigate = useNavigate()
  const [params] = useSearchParams()
  const sourceArtifactId = params.get('artifactId')
  const images = useQuery({ queryKey: ['images'], queryFn: api.images, enabled: sourceArtifactId !== null })
  const sourceImage = images.data?.find(item => item.id === sourceArtifactId)
  const secrets = useQuery({ queryKey: ['secrets'], queryFn: api.secrets, enabled: me.data?.secretAvailable })
  const [createdDraft, setCreatedDraft] = useState<string | null>(null)
  useEffect(() => {
    if (sourceImage?.scanStatus === 'Approved') form.setFieldValue('image', sourceImage.pinnedReference)
  }, [form, sourceImage])
  const query = useQueryClient()
  const [toast, context] = message.useMessage()
  const mutation = useMutation({ mutationFn: async (spec: AppSpec) => {
    const app = await api.createApp(spec)
    if (sourceArtifactId) {
      try { await api.deployImage({ appId: app.id, artifactId: sourceArtifactId }) }
      catch (error) { return { app, deployError: error as Error } }
    }
    return { app, deployError: null }
  }, onSuccess: async ({ app, deployError }) => {
    await Promise.all([query.invalidateQueries({ queryKey: ['me'] }), query.invalidateQueries({ queryKey: ['apps'] })])
    if (deployError) { setCreatedDraft(app.id); toast.error('Draft saved, but deployment was not queued. Open Overview and retry with an approved image.'); return }
    toast.success(sourceArtifactId ? 'Approved build deployment queued' : t('created'))
    navigate('/')
  }, onError: (error: Error) => toast.error(error.message) })
  const persistent = Form.useWatch('persistent', form)
  const boundVolumes = new Set(apps.data?.map(item => item.spec.volumeId).filter(Boolean))
  const readyVolumes = volumes.data?.filter(item => item.state === 'Ready' && !boundVolumes.has(item.id)) ?? []
  return <>
    {context}
    <div className="page-heading"><div><span className="eyebrow">APPLICATIONS</span><Title level={2}>{t('newAppTitle')}</Title><Paragraph type="secondary">{t('newAppIntro')}</Paragraph></div></div>
    {sourceArtifactId && (images.isPending ? <Alert type="info" showIcon message="Checking the built image" />
      : sourceImage?.scanStatus === 'Approved' ? <Alert type="success" showIcon message="Approved build selected" description="Enter the port your program listens on: Python template 8000, Node.js template 3000, or the port in your Dockerfile. Submitting will create the draft and queue deployment." />
        : <Alert type="warning" showIcon message="Build image is not approved" description="Wait for its security scan on My images before creating and deploying this app." />)}
    {createdDraft && <Alert type="warning" showIcon message="Draft created; deployment needs a retry" description={<NavLink to="/">Open Overview to deploy an approved image to this draft.</NavLink>} />}
    <Card className="form-card">
      <Form<AppSpec> form={form} layout="vertical" initialValues={{ port: sourceArtifactId ? undefined : 8080, persistent: false, resources: { cpuRequestMilli: 250, cpuLimitMilli: 1000, memoryMiB: 1024, replicas: 1 } }} onFinish={values => mutation.mutate(values)}>
        <div className="form-section"><h3>Application</h3><p>{sourceArtifactId ? 'Choose a short name for the selected approved image.' : 'Choose a short name and enter the proposed image reference. For deployment, select a security approved image from My images.'}</p></div>
        <div className="form-grid">
          <Form.Item label={t('appName')} name="name" rules={[{ required: true, pattern: /^[a-z]([a-z0-9-]{0,38}[a-z0-9])?$/, message: 'Use 1–40 lowercase letters, numbers or hyphens.' }]}><Input placeholder="my-agent" maxLength={40} /></Form.Item>
          <Form.Item label={t('image')} name="image" rules={[{ required: true, message: 'Enter a container image.' }]}><Input placeholder="registry.example.com/my-agent:v1" disabled={sourceArtifactId !== null} /></Form.Item>
        </div>
        <Form.Item label={t('port')} name="port" extra={sourceArtifactId ? 'Container listening port: Python sample 8000, Node.js sample 3000, or the actual port for your Dockerfile app. Enter a number, not a URL.' : 'Enter the HTTP port your process listens on inside the container, not the browser or host port. Bind to 0.0.0.0.'} rules={[{ required: true, message: 'Enter the port your container listens on.' }]}><InputNumber min={1} max={65535} style={{ width: '100%' }} /></Form.Item>
        <Form.Item label="HTTP health path (optional)" name="healthPath"
          extra="Use a path such as /health only if your app returns HTTP 2xx or 3xx while healthy. When empty, PolyGate checks the TCP port."
          rules={[{ pattern: /^\/(?!\/)(?!.*\.\.)(?:[A-Za-z0-9/_\.~-]*)$/, message: 'Enter an absolute path without spaces, query or fragment.' }, { max: 128 }]}>
          <Input placeholder="/health" maxLength={128} />
        </Form.Item>
        {me.data?.secretAvailable && <Form.Item label="Environment secrets (optional)" name="secretKeys"
          extra="Only the selected keys are passed to this app as environment variables. Values are never shown in PolyGate."
          rules={[{ type: 'array', max: 8, message: 'Select at most eight keys.' }]}>
          <Select mode="multiple" loading={secrets.isPending} placeholder="Select saved keys" options={(secrets.data ?? []).map(item => ({ value: item.name, label: item.name }))} />
        </Form.Item>}
        {me.data?.secretAvailable && secrets.error && <Alert type="error" showIcon message="Could not load environment secrets" description={secrets.error.message} />}
        <div className="form-section separated"><h3>Resources</h3><p>Memory request equals memory limit in the first release.</p></div>
        <div className="form-grid">
          <Form.Item label={t('cpuRequest')} name={['resources', 'cpuRequestMilli']} rules={[{ required: true }]}><InputNumber min={100} max={2000} step={50} style={{ width: '100%' }} /></Form.Item>
          <Form.Item label={t('cpuLimit')} name={['resources', 'cpuLimitMilli']} rules={[{ required: true }]}><InputNumber min={100} max={2000} step={50} style={{ width: '100%' }} /></Form.Item>
          <Form.Item label={t('memory')} name={['resources', 'memoryMiB']} rules={[{ required: true }]}><Select options={[128, 256, 512, 1024, 2048, 3072, 4096].map(n => ({ value: n, label: `${n} MiB${n === 4096 ? ' (maximum)' : ''}` }))} /></Form.Item>
          <Form.Item label={t('replicas')} name={['resources', 'replicas']} rules={[{ required: true }]}><Select options={(persistent ? [1] : [1, 2]).map(n => ({ value: n, label: `${n}` }))} /></Form.Item>
        </div>
        {me.data?.volumeAvailable && <Form.Item label={t('persistent')} name="persistent" valuePropName="checked"><Switch onChange={checked => { if (checked) form.setFieldValue(['resources', 'replicas'], 1); else form.setFieldsValue({ volumeId: undefined, mountPath: undefined }) }} /></Form.Item>}
        {me.data?.volumeAvailable && persistent && <>
          <Alert type="info" showIcon style={{ marginBottom: 16 }} message="One app per volume" description="Stopping or deleting the app keeps the volume and its data. Select a ready, unused volume." />
          {volumes.error && <Alert type="error" showIcon message="Could not load volumes" description={volumes.error.message} />}
          <div className="form-grid">
            <Form.Item label="Volume" name="volumeId" rules={[{ required: true, message: 'Select a ready volume.' }]}><Select loading={volumes.isPending} placeholder="Select a ready volume" options={readyVolumes.map(item => ({ value: item.id, label: `${item.spec.name} (${item.spec.sizeGiB} GiB)` }))} /></Form.Item>
            <Form.Item label="Mount path" name="mountPath" initialValue="/data" rules={[{ required: true, pattern: /^\/(?!proc(?:\/|$)|sys(?:\/|$)|dev(?:\/|$)|tmp(?:\/|$)|run(?:\/|$)|var\/run(?:\/|$))[A-Za-z0-9/_-]+$/, message: 'Use a safe absolute path such as /data.' }]}><Input placeholder="/data" /></Form.Item>
          </div>
          {readyVolumes.length === 0 && <Paragraph type="secondary">No unused ready volume. <NavLink to="/volumes">Create a volume</NavLink> and wait until it is ready.</Paragraph>}
        </>}
        <div className="form-actions"><Space><Button type="primary" htmlType="submit" disabled={createdDraft !== null || (sourceArtifactId !== null && sourceImage?.scanStatus !== 'Approved')} loading={mutation.isPending}>{sourceArtifactId ? 'Create and deploy' : t('createDraft')}</Button><Button onClick={() => navigate('/')}>{t('cancel')}</Button></Space></div>
      </Form>
    </Card>
  </>
}

function SecretsPage() {
  const me = useQuery({ queryKey: ['me'], queryFn: api.me })
  const secrets = useQuery({ queryKey: ['secrets'], queryFn: api.secrets, enabled: me.data?.secretAvailable })
  const apps = useQuery({ queryKey: ['apps'], queryFn: api.apps, enabled: me.data?.secretAvailable })
  const query = useQueryClient()
  const [toast, context] = message.useMessage()
  const [form] = Form.useForm<{ name: string; value: string }>()
  const saving = useMutation({ mutationFn: api.putSecret, onSuccess: async () => {
    form.resetFields()
    await query.invalidateQueries({ queryKey: ['secrets'] })
    toast.success('Secret saved')
  }, onError: (error: Error) => toast.error(error.message) })
  const removing = useMutation({ mutationFn: api.deleteSecret, onSuccess: async () => {
    await query.invalidateQueries({ queryKey: ['secrets'] })
    toast.success('Secret removed')
  }, onError: (error: Error) => toast.error(error.message) })
  if (me.isPending) return <div className="center"><Spin size="large" /></div>
  if (!me.data?.secretAvailable) return <Alert type="info" showIcon message="Environment secrets are not available for this pilot." />
  return <>
    {context}
    <div className="page-heading"><div><span className="eyebrow">ENVIRONMENT</span><Title level={2}>Secrets</Title><Paragraph type="secondary">Save API tokens and select the keys each app needs when creating it. Values are never shown again. Replaced values take effect when the app starts a new Pod.</Paragraph></div></div>
    <Alert type="warning" showIcon message="Internal pilot: use limited test tokens" description="Kubernetes Secrets are not yet encrypted at rest. Use short-lived tokens with spending limits; do not save production credentials here." style={{ marginBottom: 24 }} />
    <Card className="form-card" title="Add or replace a secret">
      <Form form={form} layout="vertical" onFinish={values => saving.mutate(values)}>
        <Form.Item name="name" label="Environment variable name" rules={[{ required: true }, { pattern: /^[A-Z][A-Z0-9_]{0,63}$/, message: 'Use up to 64 uppercase letters, numbers and underscores; start with a letter.' }, { validator: async (_, value: string) => { if (value?.startsWith('POLYGATE_') || value?.startsWith('KUBERNETES_')) throw new Error('This prefix is reserved.') } }]}><Input autoComplete="off" placeholder="MODEL_API_KEY" maxLength={64} /></Form.Item>
        <Form.Item name="value" label="Value" rules={[{ required: true }, { validator: async (_, value: string) => { if (new TextEncoder().encode(value ?? '').length > 8192) throw new Error('Value must be at most 8192 bytes.') } }]}><Input.Password autoComplete="new-password" placeholder="Paste the token" /></Form.Item>
        <Button type="primary" htmlType="submit" loading={saving.isPending}>Save secret</Button>
      </Form>
    </Card>
    <Card className="apps-card" title="Saved keys" style={{ marginTop: 24 }}>
      {secrets.error && <Alert type="error" showIcon message="Could not load saved keys" description={secrets.error.message} />}
      <Table rowKey="name" loading={secrets.isPending} dataSource={secrets.data ?? []} pagination={false}
        locale={{ emptyText: 'No secrets saved yet.' }} columns={[
          { title: 'Name', dataIndex: 'name', key: 'name', render: (name: string) => <code>{name}</code> },
          { title: 'Used by', key: 'used', render: (_: unknown, item: { name: string }) => apps.data?.filter(app => app.spec.secretKeys?.includes(item.name)).map(app => app.spec.name).join(', ') || 'No apps' },
          { title: 'Action', key: 'action', render: (_: unknown, item: { name: string }) => <Popconfirm title={`Remove ${item.name}?`} description="Apps using this key must be deleted first." onConfirm={() => removing.mutate(item.name)}><Button danger size="small" loading={removing.isPending}>Remove</Button></Popconfirm> },
        ]} />
    </Card>
  </>
}

function VolumesPage() {
  const me = useQuery({ queryKey: ['me'], queryFn: api.me })
  const volumes = useQuery({ queryKey: ['volumes'], queryFn: api.volumes, enabled: me.data?.volumeAvailable, refetchInterval: 5000 })
  const apps = useQuery({ queryKey: ['apps'], queryFn: api.apps, enabled: me.data?.volumeAvailable })
  const query = useQueryClient()
  const [toast, context] = message.useMessage()
  const [form] = Form.useForm<{ name: string; sizeGiB: number }>()
  const [expandingVolume, setExpandingVolume] = useState<Volume | null>(null)
  const [expandedSize, setExpandedSize] = useState<number | null>(null)
  const [deletingVolume, setDeletingVolume] = useState<Volume | null>(null)
  const [deleteConfirmation, setDeleteConfirmation] = useState('')
  const creating = useMutation({ mutationFn: api.createVolume, onSuccess: async () => {
    form.resetFields()
    await query.invalidateQueries({ queryKey: ['volumes'] })
    toast.success('Volume requested. Wait for Ready before attaching it to an app.')
  }, onError: (error: Error) => toast.error(error.message) })
  const expanding = useMutation({ mutationFn: api.expandVolume, onSuccess: async () => {
    setExpandingVolume(null)
    setExpandedSize(null)
    await query.invalidateQueries({ queryKey: ['volumes'] })
    toast.success('Expansion requested. Wait for Ready before starting a new app.')
  }, onError: (error: Error) => toast.error(error.message) })
  const deleting = useMutation({ mutationFn: api.deleteVolume, onSuccess: async () => {
    setDeletingVolume(null)
    setDeleteConfirmation('')
    await query.invalidateQueries({ queryKey: ['volumes'] })
    toast.success('Volume deletion requested. Data is removed after backend cleanup completes.')
  }, onError: (error: Error) => toast.error(error.message) })
  const items = volumes.data ?? []
  const used = items.reduce((total, item) => total + item.spec.sizeGiB, 0)
  const remaining = Math.max(0, 10 - used)
  const boundVolumes = new Set(apps.data?.map(item => item.spec.volumeId).filter(Boolean))
  const columns = [
    { title: 'Name', dataIndex: ['spec', 'name'], key: 'name' },
    { title: 'Size', key: 'size', render: (_: unknown, item: Volume) => `${item.spec.sizeGiB} GiB` },
    { title: 'Status', dataIndex: 'state', key: 'state' },
    { title: 'Created', dataIndex: 'createdAt', key: 'createdAt', render: (date: string) => new Date(date).toLocaleString('en-GB') },
    { title: 'Actions', key: 'actions', render: (_: unknown, item: Volume) => item.state === 'Ready'
      ? <Space>
          {remaining > 0 && item.spec.sizeGiB < 10 && <Button type="link" onClick={() => { setExpandingVolume(item); setExpandedSize(item.spec.sizeGiB + 1) }}>Expand</Button>}
          <Button type="link" danger disabled={apps.isPending || boundVolumes.has(item.id)}
            onClick={() => { setDeletingVolume(item); setDeleteConfirmation('') }}>Delete</Button>
        </Space> : null },
  ]
  if (me.isPending) return <div className="center"><Spin size="large" /></div>
  if (!me.data?.volumeAvailable) return <Alert type="info" showIcon message="Volumes are not available for this account yet." />
  return <>
    {context}
    <div className="page-heading"><div><span className="eyebrow">PERSISTENT STORAGE</span><Title level={2}>Your volumes</Title><Paragraph type="secondary">Each account may hold up to two volumes and 10 GiB total. One app can use a volume at a time.</Paragraph></div></div>
    <Alert type="warning" showIcon style={{ marginBottom: 20 }} message="Single-copy pilot storage" description="Volumes currently have one online copy on the .45 server and daily external backups. A server failure can interrupt access; changes since the latest backup may be lost. Keep your own copy of important files." />
    {volumes.error && <Alert type="error" showIcon message="Could not load volumes" description={volumes.error.message} />}
    <Card className="form-card" title="Request a volume" style={{ marginBottom: 20 }}>
      <Paragraph type="secondary">A new volume is usable only after its status becomes Ready. Stopping or deleting an app keeps the volume and its data.</Paragraph>
      <Form form={form} layout="vertical" initialValues={{ sizeGiB: 5 }} onFinish={values => creating.mutate(values)}>
        <div className="form-grid">
          <Form.Item label="Name" name="name" rules={[{ required: true, pattern: /^[a-z]([a-z0-9-]{0,38}[a-z0-9])?$/, message: 'Use 1–40 lowercase letters, numbers or hyphens.' }]}><Input placeholder="agent-data" maxLength={40} /></Form.Item>
          <Form.Item label="Size (GiB)" name="sizeGiB" rules={[{ required: true }]}><InputNumber min={1} max={remaining} style={{ width: '100%' }} /></Form.Item>
        </div>
        <Button type="primary" htmlType="submit" disabled={items.length >= 2 || remaining < 1 || volumes.isPending} loading={creating.isPending}>Request volume</Button>
      </Form>
    </Card>
    <Card className="apps-card" title={`Volumes (${used} / 10 GiB)`}><Table<Volume> rowKey="id" columns={columns} dataSource={items} loading={volumes.isPending} pagination={false} locale={{ emptyText: 'No volumes yet' }} /></Card>
    <Modal title={`Expand ${expandingVolume?.spec.name ?? 'volume'}`} open={expandingVolume !== null} onCancel={() => setExpandingVolume(null)}
      onOk={() => { if (expandingVolume && expandedSize) expanding.mutate({ id: expandingVolume.id, sizeGiB: expandedSize }) }}
      okText="Expand volume" confirmLoading={expanding.isPending}
      okButtonProps={{ disabled: !expandingVolume || !expandedSize || expandedSize <= expandingVolume.spec.sizeGiB || expandedSize > expandingVolume.spec.sizeGiB + remaining }}>
      <Paragraph>Volumes can grow but cannot shrink. Wait for Ready before using the new capacity.</Paragraph>
      <InputNumber aria-label="New volume size in GiB" min={(expandingVolume?.spec.sizeGiB ?? 0) + 1}
        max={(expandingVolume?.spec.sizeGiB ?? 0) + remaining} value={expandedSize} onChange={setExpandedSize} style={{ width: '100%' }} />
    </Modal>
    <Modal title={`Permanently delete ${deletingVolume?.spec.name ?? 'volume'}?`}
      open={deletingVolume !== null} onCancel={() => { setDeletingVolume(null); setDeleteConfirmation('') }}
      onOk={() => { if (deletingVolume) deleting.mutate({ id: deletingVolume.id, confirmName: deleteConfirmation }) }}
      okText="Delete volume and data" okButtonProps={{ danger: true, disabled: !deletingVolume || deleteConfirmation !== deletingVolume.spec.name }}
      confirmLoading={deleting.isPending}>
      <Paragraph>This permanently removes the volume and all its files. Delete any app using it first.</Paragraph>
      <Paragraph>Type <Text strong>{deletingVolume?.spec.name}</Text> to confirm.</Paragraph>
      <Input aria-label="Volume name confirmation" value={deleteConfirmation} onChange={event => setDeleteConfirmation(event.target.value)} />
    </Modal>
  </>
}

function SourcePage() {
  const { t } = useTranslation()
  const me = useQuery({ queryKey: ['me'], queryFn: api.me })
  const [file, setFile] = useState<File | null>(null)
  const [template, setTemplate] = useState<BuildTemplate>('dockerfile')
  const fileInput = useRef<HTMLInputElement>(null)
  const query = useQueryClient()
  const [toast, context] = message.useMessage()
  const validation = useMutation({ mutationFn: api.validateSource, onSuccess: () => toast.success(t('zipReady')), onError: (error: Error) => toast.error(error.message) })
  const submission = useMutation({ mutationFn: (selected: File) => api.submitSourceBuild(selected, template), onSuccess: async () => {
    await query.invalidateQueries({ queryKey: ['source-builds'] })
    setFile(null)
    if (fileInput.current) fileInput.current.value = ''
    toast.success('Build queued')
  }, onError: (error: Error) => toast.error(error.message) })
  const builds = useQuery({ queryKey: ['source-builds'], queryFn: api.sourceBuilds,
    enabled: me.data?.mode === 'authenticated' && me.data.sourceBuildAvailable,
    refetchInterval: me.data?.sourceBuildAvailable ? 5000 : false })
  const buildEnabled = me.data?.mode === 'authenticated' && me.data.sourceBuildAvailable
  const images = useQuery({ queryKey: ['images'], queryFn: api.images,
    enabled: buildEnabled, refetchInterval: buildEnabled ? 5000 : false })
  const [selectedBuild, setSelectedBuild] = useState<string | null>(null)
  const [logOffset, setLogOffset] = useState(0)
  const [logText, setLogText] = useState('')
  const decoder = useRef(new TextDecoder())
  const log = useQuery({ queryKey: ['source-build-log', selectedBuild, logOffset],
    queryFn: () => api.sourceBuildLog(selectedBuild!, logOffset), enabled: buildEnabled && selectedBuild !== null,
    refetchInterval: 1500 })
  useEffect(() => {
    if (!log.data || !selectedBuild || log.data.nextOffset <= logOffset) return
    const bytes = Uint8Array.from(atob(log.data.data), char => char.charCodeAt(0))
    const chunk = decoder.current.decode(bytes, { stream: true })
    setLogText(previous => (previous + chunk).slice(-2 * 1024 * 1024))
    setLogOffset(log.data.nextOffset)
  }, [log.data, logOffset, selectedBuild])
  const showLog = (id: string) => {
    decoder.current = new TextDecoder()
    setLogText('')
    setLogOffset(0)
    setSelectedBuild(id)
  }
  const columns = [
    { title: 'Log', key: 'log', render: (_: unknown, item: SourceBuild) => <Button size="small" onClick={() => showLog(item.id)}>View log</Button> },
    { title: 'Submitted', dataIndex: 'createdAt', key: 'createdAt', render: (date: string) => new Date(date).toLocaleString('en-GB') },
    { title: 'Source SHA-256', dataIndex: 'sourceSha256', key: 'sourceSha256', render: (digest: string) => <code title={digest}>{digest.slice(0, 20)}…</code> },
    { title: 'Status', dataIndex: 'state', key: 'state', render: (state: SourceBuild['state']) => state === 'Leased' ? 'Building' : state },
    { title: 'Attempts', dataIndex: 'attempt', key: 'attempt' },
    { title: 'Result', key: 'result', render: (_: unknown, item: SourceBuild) => item.resultImageArtifactId
      ? images.data?.find(image => image.id === item.resultImageArtifactId)?.scanStatus === 'Approved'
        ? <Space direction="vertical" size={0}><NavLink to={`/apps/new?artifactId=${encodeURIComponent(item.resultImageArtifactId)}`}>Create and deploy app</NavLink><NavLink to={`/?artifactId=${encodeURIComponent(item.resultImageArtifactId)}`}>Update a running app</NavLink></Space>
        : <NavLink to="/images">View image and security scan</NavLink>
      : item.resultImageDeletedAt ? <Text type="secondary">Image removed</Text>
        : item.errorCode ? <Text type="danger">{item.errorCode.replaceAll('_', ' ').toLowerCase()}</Text> : '—' },
  ]
  return <>
    {context}
    <div className="page-heading"><div><span className="eyebrow">SOURCE BUILD</span><Title level={2}>{buildEnabled ? 'Build from a source ZIP' : t('zipTitle')}</Title><Paragraph type="secondary">{buildEnabled ? 'Upload a trusted source ZIP, choose a build method, then review the build log and image scan before deployment.' : t('zipIntro')}</Paragraph></div></div>
    {buildEnabled && <Card className="form-card" title="Build method">
      <Select<BuildTemplate> aria-label="Build method" value={template} style={{ width: '100%', maxWidth: 420 }} onChange={setTemplate} options={[
        { value: 'dockerfile', label: 'Use my Dockerfile' },
        { value: 'python', label: 'Python template' },
        { value: 'node', label: 'Node.js template' },
      ]} />
      <Paragraph type="secondary" style={{ marginTop: 12 }}>{template === 'dockerfile'
        ? 'Put a Dockerfile at the root of your ZIP.'
        : template === 'python'
          ? 'Put main.py at the root. requirements.txt is optional. Run an HTTP server on 0.0.0.0:8000; the image runs as a non-root user.'
          : 'Put package.json and package-lock.json at the root with a scripts.start command. The command runs directly at startup; npm is available during build only. Listen on 0.0.0.0:3000; the image runs as a non-root user.'}</Paragraph>
    </Card>}
    <Card className="form-card upload-card"><div className="upload-icon">⌑</div><Title level={4}>{t('chooseZIP')}</Title><Paragraph type="secondary">ZIP only · 100 MiB compressed · 1 GiB expanded</Paragraph><input ref={fileInput} className="hidden-file-input" aria-label="Source ZIP" type="file" accept=".zip,application/zip" onChange={event => setFile(event.target.files?.[0] ?? null)} /><Button onClick={() => fileInput.current?.click()}>Browse ZIP...</Button>{file && <p className="selected-file">Selected: {file.name}</p>}<div className="upload-action"><Button type="primary" disabled={!file || file.size > 100 * 1024 * 1024} loading={validation.isPending || submission.isPending} onClick={() => { if (file) { if (buildEnabled) submission.mutate(file); else validation.mutate(file) } }}>{buildEnabled ? 'Queue build' : t('validateZIP')}</Button></div>{file && file.size > 100 * 1024 * 1024 && <Alert type="error" showIcon message="ZIP exceeds 100 MiB" />}{validation.data && !buildEnabled && <Alert type="success" showIcon message={validation.data.message} description={`${(validation.data.sizeBytes / 1024).toFixed(1)} KiB inspected`} />}</Card>
    {buildEnabled && <Card className="apps-card" title="Build history" extra={<Button size="small" loading={builds.isFetching} onClick={() => void builds.refetch()}>Refresh</Button>}>
      {builds.error && <Alert type="error" showIcon message="Could not load build history" description={builds.error.message} />}
      <Table<SourceBuild> rowKey="id" columns={columns} dataSource={builds.data ?? []} loading={builds.isPending} pagination={{ pageSize: 10 }} locale={{ emptyText: 'No builds submitted yet' }} />
    </Card>}
    <Modal title="Build log" open={selectedBuild !== null} onCancel={() => setSelectedBuild(null)} footer={null} width={900}>
      {log.error && <Alert type="error" showIcon message="Could not load build log" description={log.error.message} />}
      <pre style={{ maxHeight: '60vh', overflow: 'auto', whiteSpace: 'pre-wrap', wordBreak: 'break-word' }}>{logText || (log.isFetching ? 'Loading log...' : log.data?.retained === false && ['Succeeded', 'Failed'].includes(log.data.state) ? 'No build log is retained for this task.' : 'No output yet.')}</pre>
      <Text type="secondary">Showing the latest 2 MiB of output. The log updates while the build runs.</Text>
    </Modal>
  </>
}

export default function App() {
  return <Shell><Routes><Route path="/" element={<Overview />} /><Route path="/apps/new" element={<NewApp />} /><Route path="/images" element={<ImagePage />} /><Route path="/secrets" element={<SecretsPage />} /><Route path="/volumes" element={<VolumesPage />} /><Route path="/source" element={<SourcePage />} /><Route path="/activity" element={<ActivityPage />} /><Route path="/help" element={<HelpPage />} /><Route path="/admin/accounts" element={<AdminAccountsPage />} /><Route path="/admin/cluster" element={<AdminResourcesPage />} /></Routes></Shell>
}
