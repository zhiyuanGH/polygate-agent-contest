import { useQuery } from '@tanstack/react-query'
import { Alert, Card, Progress, Table, Typography } from 'antd'
import { api, type ClusterNode } from './api'

const { Title, Text } = Typography
const gib = (bytes: number) => (bytes / 1024 ** 3).toFixed(1)
const percent = (used: number, total: number) => total > 0 ? Math.min(100, Math.round(100 * used / total)) : 0

export default function AdminResourcesPage() {
  const me = useQuery({ queryKey: ['me'], queryFn: api.me })
  const allowed = me.data?.role === 'admin'
  const resources = useQuery({ queryKey: ['admin-cluster-resources'], queryFn: api.adminClusterResources,
    enabled: allowed, refetchInterval: allowed ? 30000 : false })

  if (me.isLoading) return <Card loading />
  if (!allowed) return <Alert type="error" showIcon message="Administrator access is required" />
  return <>
    <Title level={2}>Cluster resources</Title>
    <Text type="secondary">Live host CPU, memory, and root disk usage. Application quotas are managed on the Accounts page.</Text>
    {resources.error && <Alert style={{ marginTop: 16 }} type="error" showIcon message="Could not load cluster metrics"
      description={(resources.error as Error).message} />}
    <Card style={{ marginTop: 20 }} title="Nodes" extra={resources.data && <Text type="secondary">Sampled {new Date(resources.data.sampledAt).toLocaleString()}</Text>}>
      <Table<ClusterNode> rowKey="name" dataSource={resources.data?.nodes ?? []} loading={resources.isLoading}
        pagination={false} locale={{ emptyText: 'No node samples are available.' }}
        columns={[
          { title: 'Node', dataIndex: 'name' },
          { title: 'CPU used', render: (_: unknown, node: ClusterNode) => <><Text>{node.cpuUsedPercent.toFixed(1)}%</Text><Progress
            percent={Math.min(100, Math.round(node.cpuUsedPercent))} showInfo={false} status={node.cpuUsedPercent > 90 ? 'exception' : 'normal'} /></> },
          { title: 'Memory used', render: (_: unknown, node: ClusterNode) => <><Text>{gib(node.memoryUsedBytes)} / {gib(node.memoryTotalBytes)} GiB</Text><Progress
            percent={percent(node.memoryUsedBytes, node.memoryTotalBytes)} showInfo={false}
            status={percent(node.memoryUsedBytes, node.memoryTotalBytes) > 90 ? 'exception' : 'normal'} /></> },
          { title: 'Root disk free', render: (_: unknown, node: ClusterNode) => <><Text>{gib(node.rootAvailableBytes)} / {gib(node.rootTotalBytes)} GiB free</Text><Progress
            percent={percent(node.rootTotalBytes - node.rootAvailableBytes, node.rootTotalBytes)} showInfo={false}
            status={percent(node.rootAvailableBytes, node.rootTotalBytes) < 15 ? 'exception' : 'normal'} /></> },
        ]} />
    </Card>
  </>
}
