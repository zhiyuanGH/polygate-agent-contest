import { useInfiniteQuery, useQuery } from '@tanstack/react-query'
import { Alert, Button, Card, Space, Table, Tag, Typography } from 'antd'
import { api, type Operation } from './api'

const { Title, Paragraph, Text } = Typography

function label(kind: string) {
  const words = kind.toLowerCase().replaceAll('_', ' ')
  return words.charAt(0).toUpperCase() + words.slice(1)
}

export default function ActivityPage() {
  const me = useQuery({ queryKey: ['me'], queryFn: api.me })
  const activity = useInfiniteQuery({ queryKey: ['operation-history'],
    queryFn: ({ pageParam }) => api.operationHistory(pageParam), initialPageParam: '',
    getNextPageParam: page => page.nextCursor || undefined,
    enabled: me.data?.mode === 'authenticated' })
  const items = activity.data?.pages.flatMap(page => page.items) ?? []

  return <>
    <div className="page-heading">
      <div><span className="eyebrow">ACCOUNT ACTIVITY</span><Title level={2}>Recent activity</Title>
        <Paragraph type="secondary">Your account operations, newest first. Load older records as needed. Times use your browser's time zone.</Paragraph></div>
      <Button onClick={() => void activity.refetch()} loading={activity.isFetching}>Refresh</Button>
    </div>
    {activity.error && <Alert type="error" showIcon message="Could not load activity" description={activity.error.message} style={{ marginBottom: 16 }} />}
    <Card>
      <Table<Operation> rowKey="id" dataSource={items} loading={activity.isPending}
        pagination={{ pageSize: 10 }} locale={{ emptyText: 'No activity yet.' }}
        columns={[
          { title: 'Time', dataIndex: 'createdAt', key: 'time', width: 190,
            render: (value: string) => new Date(value).toLocaleString('en-GB') },
          { title: 'Action', dataIndex: 'kind', key: 'kind', render: (value: string) => label(value) },
          { title: 'Status', dataIndex: 'state', key: 'state', width: 130,
            render: (value: string) => <Tag color={value === 'Succeeded' || value === 'Approved' ? 'green' : value === 'Failed' || value === 'Rejected' ? 'red' : 'blue'}>{value}</Tag> },
          { title: 'Details', key: 'details', render: (_: unknown, item: Operation) => <Space direction="vertical" size={0}>
            {item.appId && <Text type="secondary">App {item.appId.slice(0, 8)}</Text>}
            {item.detail?.errorCode && <Text type="danger">{item.detail.errorCode.replaceAll('_', ' ')}</Text>}
          </Space> },
        ]} />
      {activity.hasNextPage && <Button onClick={() => void activity.fetchNextPage()}
        loading={activity.isFetchingNextPage}>Load older activity</Button>}
    </Card>
  </>
}
