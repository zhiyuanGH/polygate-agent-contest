import { useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Alert, Button, Card, Input, Modal, Popconfirm, Progress, Space, Spin, Table, Tag, Typography, message } from 'antd'
import { Link } from 'react-router-dom'
import { api, type ImageArtifact } from './api'

const { Title, Text, Paragraph } = Typography
const scanFinished = (item: ImageArtifact) => item.scanStatus !== 'PendingScan'
const maxAutomaticScanWaitMs = 30 * 60 * 1000
const harborReportType = 'application/vnd.security.vulnerability.report; version=1.1'

function scanLabel(item: ImageArtifact) {
  if (item.scanStatus === 'PendingScan') return item.scanTaskState === 'Leased' ? 'Scanning' : 'Queued'
  return item.scanStatus
}

function scanDetails(item: ImageArtifact) {
  if (item.scanStatus === 'Failed') return item.scanErrorCode
    ? `Inspection failed (${item.scanErrorCode}). This image cannot be deployed.`
    : 'Inspection failed. This image cannot be deployed.'
  if (item.scanStatus === 'PendingScan' && Date.now() - new Date(item.createdAt).getTime() >= maxAutomaticScanWaitMs)
    return 'The check is taking longer than expected. Refresh this list later.'
  if (!item.scanSummary) return item.scanStatus === 'Rejected'
    ? 'The image did not meet the current security policy.' : ''
  const counts = ['Critical', 'High', 'Unknown', 'Medium', 'Low', 'Negligible']
    .filter(level => (item.scanSummary?.severities[level] ?? 0) > 0)
    .map(level => `${level}: ${item.scanSummary?.severities[level]}`)
  const result = counts.length ? counts.join(' · ') : 'No vulnerabilities reported'
  return item.scanStatus === 'Rejected'
    ? `${result}. Critical or Unknown findings block deployment.`
    : `${result}. Checked under ${item.scanSummary.policyVersion}.`
}

export default function ImagePage() {
  const [reference, setReference] = useState('')
  const [archive, setArchive] = useState<File | null>(null)
  const [uploadProgress, setUploadProgress] = useState(0)
  const [reportImage, setReportImage] = useState<ImageArtifact | null>(null)
  const [failureImage, setFailureImage] = useState<ImageArtifact | null>(null)
  const me = useQuery({ queryKey: ['me'], queryFn: api.me })
  const images = useQuery({
    queryKey: ['images'], queryFn: api.images,
    enabled: me.data?.mode === 'authenticated',
    refetchInterval: query => query.state.data?.some(item => !scanFinished(item) &&
      Date.now() - new Date(item.createdAt).getTime() < maxAutomaticScanWaitMs) ? 5000 : false,
  })
  const query = useQueryClient()
  const [toast, context] = message.useMessage()
  const inspect = useMutation({
    mutationFn: api.inspectImage,
    onSuccess: async () => {
      setReference('')
      await query.invalidateQueries({ queryKey: ['images'] })
      toast.success('Image queued for security inspection')
    },
    onError: (error: Error) => toast.error(error.message),
  })
  const remove = useMutation({
    mutationFn: api.removeImage,
    onSuccess: async () => {
      await query.invalidateQueries({ queryKey: ['images'] })
      toast.success('Image removal queued. Registry quota will update after cleanup.')
    },
    onError: (error: Error) => toast.error(error.message),
  })
  const upload = useMutation({
    mutationFn: (file: File) => api.uploadImageTar(file, setUploadProgress),
    onSuccess: async () => {
      setArchive(null)
      setUploadProgress(0)
      await query.invalidateQueries({ queryKey: ['images'] })
      toast.success('Image uploaded. Wait for its security check before deployment.')
    },
    onError: (error: Error) => toast.error(error.message),
  })
  const report = useMutation({ mutationFn: api.imageScanReport,
    onError: (error: Error) => toast.error(error.message) })
  const failure = useMutation({ mutationFn: api.imageScanFailure,
    onError: (error: Error) => toast.error(error.message) })
  const harborReport = report.data?.[harborReportType] as Record<string, unknown> | undefined
  const vulnerabilities = Array.isArray(harborReport?.vulnerabilities)
    ? harborReport.vulnerabilities as Record<string, unknown>[] : []
  const columns = [
    { title: 'Submitted image', dataIndex: 'submittedReference', key: 'submittedReference', render: (value: string) => <Text className="image-reference" copyable={{ text: value }}>{value}</Text> },
    { title: 'Security check', key: 'scan', render: (_: unknown, item: ImageArtifact) => <Space direction="vertical" size={2}>
      <Tag color={item.scanStatus === 'Approved' ? 'green' : item.scanStatus === 'Rejected' || item.scanStatus === 'Failed' ? 'red' : 'blue'}>{scanLabel(item)}</Tag>
      {scanDetails(item) && <Text type="secondary" className="scan-details">{scanDetails(item)}</Text>}
    </Space> },
    { title: 'Pinned digest', dataIndex: 'platformDigest', key: 'platformDigest', render: (value: string) => <Text className="image-digest" copyable={{ text: value }}>{value.slice(-23)}</Text> },
    { title: 'Submitted', dataIndex: 'createdAt', key: 'createdAt', render: (value: string) => new Date(value).toLocaleString('en-GB') },
    { title: 'Action', key: 'action', render: (_: unknown, item: ImageArtifact) => <Space wrap>
      {['Approved', 'Rejected'].includes(item.scanStatus) && <>
        <Button size="small" onClick={() => { setReportImage(item); report.mutate(item.id) }}>View report</Button>
        <a href={`/api/v1/images/${encodeURIComponent(item.id)}/scan-report?download=1`}>Download JSON</a>
      </>}
      {item.scanStatus === 'Failed' && <>
        <Button size="small" onClick={() => { setFailureImage(item); failure.mutate(item.id) }}>View failure</Button>
        <a href={`/api/v1/images/${encodeURIComponent(item.id)}/scan-failure?download=1`}>Download JSON</a>
      </>}
      <Popconfirm title="Remove this image?" description="Images used by an app cannot be removed. Build history will remain, but its image can no longer be deployed."
        okText="Remove" cancelText="Keep" onConfirm={() => remove.mutate(item.id)}>
        <Button size="small" danger disabled={item.scanStatus === 'PendingScan' || remove.isPending}>Remove</Button>
      </Popconfirm>
    </Space> },
  ]
  return <>
    {context}
    <div className="page-heading"><div><span className="eyebrow">CONTAINER IMAGES</span><Title level={2}>My images</Title><Paragraph type="secondary">Upload a Docker image tar or submit a registry reference. PolyGate stores the image privately and checks it before deployment.</Paragraph></div></div>
    {!me.data?.imageDeploymentAvailable && <Alert type="info" showIcon className="image-access-note" message="Image deployment is not enabled for this account" description="You may inspect images, but only pilot accounts can deploy approved images at this stage." />}
    {me.data?.mode === 'authenticated' ? <Card className="form-card image-submit-card" title="Inspect an image">
      <Space.Compact className="image-submit-row"><Input aria-label="Container image reference" value={reference} maxLength={512} placeholder="registry-1.docker.io/library/my-agent:1.0" onChange={event => setReference(event.target.value)} onPressEnter={() => { if (reference.trim()) inspect.mutate(reference.trim()) }} /><Button type="primary" loading={inspect.isPending} disabled={!reference.trim()} onClick={() => inspect.mutate(reference.trim())}>Inspect image</Button></Space.Compact>
      <Paragraph type="secondary" className="image-help">Use a registry allowed by the platform. Only images marked Approved can be selected for deployment.</Paragraph>
    </Card> : <Alert type="info" showIcon message="Sign in to inspect images" />}
    {me.data?.imageDeploymentAvailable && <Card className="form-card image-submit-card" title="Upload an image tar" style={{ marginTop: 20 }}>
      <Paragraph type="secondary">Run <code>docker save -o my-agent.tar my-agent:latest</code>, then select the tar file here. One linux/amd64 image, at most 1 GiB.</Paragraph>
      <input aria-label="Docker image tar" type="file" accept=".tar,application/x-tar" disabled={upload.isPending}
        onChange={event => { setArchive(event.target.files?.[0] ?? null); setUploadProgress(0) }} />
      <div style={{ marginTop: 12 }}><Button type="primary" loading={upload.isPending}
        disabled={!archive || !archive.name.toLowerCase().endsWith('.tar') || archive.size > 1024 * 1024 * 1024}
        onClick={() => { if (archive) upload.mutate(archive) }}>Upload image</Button></div>
      {upload.isPending && <><Progress percent={uploadProgress} style={{ marginTop: 12 }} /><Text type="secondary">{uploadProgress === 100 ? 'Importing into the private registry…' : 'Uploading in 8 MiB parts…'}</Text></>}
      {archive && archive.size > 1024 * 1024 * 1024 && <Alert type="error" showIcon message="Archive exceeds 1 GiB" style={{ marginTop: 12 }} />}
    </Card>}
    <Card className="apps-card" title="Inspection history" extra={<Space><Button size="small" loading={images.isFetching} onClick={() => void images.refetch()} disabled={me.data?.mode !== 'authenticated'}>Refresh</Button>{me.data?.imageDeploymentAvailable && <Link to="/apps/new">Create an app</Link>}</Space>}>
      {images.isPending && me.data?.mode === 'authenticated' ? <div className="center"><Spin /></div> : images.error ? <Alert type="error" showIcon message="Could not load images" description={images.error.message} /> : <Table<ImageArtifact> rowKey="id" columns={columns} dataSource={images.data ?? []} pagination={{ pageSize: 10 }} locale={{ emptyText: 'No images submitted yet.' }} />}
    </Card>
    <Modal title="Trivy scan report" open={reportImage !== null} onCancel={() => setReportImage(null)} footer={reportImage && <a href={`/api/v1/images/${encodeURIComponent(reportImage.id)}/scan-report?download=1`}>Download full JSON report</a>} width={1000}>
      {report.isPending ? <Spin /> : report.error ? <Alert type="error" showIcon message="Could not load scan report" description={report.error.message} /> : reportImage && <>
        <Paragraph>{scanDetails(reportImage)}</Paragraph>
        {vulnerabilities.length > 0 ? <Table size="small" rowKey={(_, index) => String(index)}
          dataSource={vulnerabilities} pagination={{ pageSize: 20 }} scroll={{ x: 820, y: '52vh' }}
          columns={[
            { title: 'Severity', dataIndex: 'severity', key: 'severity', width: 100 },
            { title: 'Vulnerability', dataIndex: 'id', key: 'id', width: 170 },
            { title: 'Package', dataIndex: 'package', key: 'package', width: 170 },
            { title: 'Installed', dataIndex: 'version', key: 'version', width: 130 },
            { title: 'Fixed in', dataIndex: 'fix_version', key: 'fix_version', width: 130 },
          ]} /> : <pre style={{ maxHeight: '55vh', overflow: 'auto', whiteSpace: 'pre-wrap' }}>{JSON.stringify(report.data, null, 2)}</pre>}
      </>}
    </Modal>
    <Modal title="Inspection failure" open={failureImage !== null} onCancel={() => setFailureImage(null)} footer={failureImage && <a href={`/api/v1/images/${encodeURIComponent(failureImage.id)}/scan-failure?download=1`}>Download failure JSON</a>} width={700}>
      {failure.isPending ? <Spin /> : failure.error ? <Alert type="error" showIcon message="Could not load inspection failure" description={failure.error.message} /> : failure.data && <>
        <Alert type="warning" showIcon message="No vulnerability report was produced" description={failure.data.summary} />
        <Paragraph style={{ marginTop: 16 }}>Error code: <Text code>{failure.data.errorCode}</Text></Paragraph>
        <Paragraph>Attempts: {failure.data.attempts} · Last checked: {new Date(failure.data.checkedAt).toLocaleString('en-GB')}</Paragraph>
      </>}
    </Modal>
  </>
}
