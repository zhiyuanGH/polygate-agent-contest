import type { ReactElement, ReactNode } from 'react'
import { Button, Popover, Tooltip } from 'antd'

export function HelpHint({ title, children }: { title: string; children: ReactNode }) {
  return <Popover title={title} content={<div style={{ maxWidth: 300 }}>{children}</div>} trigger="click">
    <Button type="text" size="small" className="help-hint" aria-label={`Help: ${title}`}>?</Button>
  </Popover>
}

export function DisabledReason({ reason, children }: { reason?: string; children: ReactElement }) {
  if (!reason) return children
  return <Tooltip title={reason} trigger={['hover', 'focus']}>
    <span className="disabled-reason" tabIndex={0} aria-label={reason} aria-disabled="true">{children}</span>
  </Tooltip>
}
