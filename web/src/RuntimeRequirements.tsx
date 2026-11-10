import { Alert, Typography } from 'antd'
import { Link } from 'react-router-dom'

const { Paragraph, Text } = Typography

export default function RuntimeRequirements({ volumeAvailable }: { volumeAvailable?: boolean }) {
  return <Alert className="runtime-requirements" type="info" showIcon message="Check where your app writes files"
    description={<>
      <Paragraph>Containers run as user/group <Text code>65532</Text> with a read-only root filesystem. Your program and its folders must be readable by that user.</Paragraph>
      <Paragraph>Use <Text code>/tmp</Text> for disposable files; they are lost when the Pod is replaced. For a database or files you want to keep, {volumeAvailable ? <Link to="/volumes">create a volume</Link> : 'request volume access from your administrator'}, mount it at <Text code>/data</Text>, and configure your app to write there.</Paragraph>
      <Paragraph>A volume is optional if your app does not need to keep files. Do not mount over your program directory (such as <Text code>/app</Text>); doing so hides the files packaged in your image.</Paragraph>
    </>} />
}
