import { Alert, Card, Typography } from 'antd'
import { Link } from 'react-router-dom'
import RuntimeRequirements from './RuntimeRequirements'

const { Title, Paragraph, Text } = Typography

export default function HelpPage() {
  return <>
    <div className="page-heading"><div><span className="eyebrow">GET STARTED</span><Title level={2}>Help</Title>
      <Paragraph type="secondary">Deploy a container image or build one from a source ZIP, then open your running app.</Paragraph>
    </div></div>
    <Alert type="info" showIcon style={{ marginBottom: 20 }} message="Using this console"
      description="Click a ? beside a field for a short explanation. Hover over or focus a disabled button to see why it is unavailable. If an action fails, read the message and suggested next step." />
    <Alert type="info" showIcon style={{ marginBottom: 20 }} message="App addresses"
      description="Sign in at https://polygate.polylink.org. The Open app link on Overview shows the exact address of your running app." />
    <RuntimeRequirements volumeAvailable />
    <Card className="form-card" title="Step-by-step illustrated guides" style={{ marginBottom: 20 }}>
      <Paragraph>Follow a complete example from sign-in and ZIP download through build, deployment, app access, logs and cleanup.</Paragraph>
      <Paragraph><a href="/student-guide-en.html" target="_blank" rel="noopener noreferrer">English guide</a> · <a href="/student-guide-zh.html" target="_blank" rel="noopener noreferrer">Chinese guide</a></Paragraph>
    </Card>
    <Card className="form-card" title="Deploy a container image" style={{ marginBottom: 20 }}>
      <ol>
        <li>Open <Link to="/images">My images</Link>. Inspect a public image reference or upload a local <Text code>docker save</Text> tar containing one <Text code>linux/amd64</Text> image (up to 1 GiB).</li>
        <li>Wait until the image is <Text strong>Approved</Text>. If it shows <Text strong>Rejected</Text>, use <Text strong>View report</Text> or <Text strong>Download JSON</Text> and fix the image. If it shows <Text strong>Failed</Text>, use <Text strong>View failure</Text> for error details, address the cause, then choose <Text strong>Retry scan</Text>. Resubmitting the same image does not start a new scan.</li>
        <li>Open <Link to="/apps/new">New app</Link>. Enter an app name, the port your container listens on, and its resources. Select <Text strong>Save draft</Text>.</li>
        <li>On <Link to="/">Overview</Link>, select <Text strong>Deploy image</Text> and choose your approved image. When the app is <Text strong>Running</Text>, select <Text strong>Open app</Text>.</li>
      </ol>
    </Card>
    <Card className="form-card" title="Build from a source ZIP" style={{ marginBottom: 20 }}>
      <ol>
        <li>Open <Link to="/source">Source ZIP</Link> and choose your Dockerfile, Python or Node.js build method.</li>
        <li>Select a ZIP of at most 100 MiB and choose <Text strong>Queue build</Text>. For a Dockerfile build, put the Dockerfile at the ZIP root.</li>
        <li>Follow <Text strong>Build history</Text> and use <Text strong>View log</Text> if the build fails.</li>
        <li>Once the resulting image is <Text strong>Approved</Text>, select <Text strong>Create and deploy app</Text>. Enter the port your program listens on: Python template <Text code>8000</Text>, Node.js template <Text code>3000</Text>, or your Dockerfile's actual port. Then wait for <Text strong>Running</Text>.</li>
      </ol>
    </Card>
    <Card className="form-card" title="Download a sample project" style={{ marginBottom: 20 }}>
      <Paragraph>Each ZIP is ready to upload on <Link to="/source">Source ZIP</Link>. Choose the listed build method, set the app port, and use <Text code>/health</Text> as the HTTP health path.</Paragraph>
      <ul>
        <li><a href="/mock-agent-python.zip" download>Mock Agent (Python ZIP)</a> — Python template, port 8000; try <Text code>POST /api/chat</Text>.</li>
        <li><a href="/mock-counter-node.zip" download>Counter (Node.js ZIP)</a> — Node.js template, port 3000; try <Text code>GET /api/count</Text>.</li>
        <li><a href="/mock-inspector-dockerfile.zip" download>Request Inspector (Dockerfile ZIP)</a> — Use my Dockerfile, port 8080; try <Text code>GET /api/request</Text>.</li>
      </ul>
      <Paragraph type="secondary">These projects use mock responses and do not call a cloud model API.</Paragraph>
    </Card>
    <Card className="form-card" title="Running and troubleshooting">
      <Paragraph><Text strong>Public app URL:</Text> most student apps open at <Text code>https://apps.polylink.org/&lt;username&gt;/&lt;app-name&gt;/</Text>. The <Text strong>Open app</Text> link on Overview is the exact address for your account. Use relative URLs for page assets, API calls and navigation, or configure your framework to work under the shown path. Requests to root paths such as <Text code>/assets</Text> or <Text code>/api</Text> may fail. Test the complete app in a browser after deployment.</Paragraph>
      <Paragraph><Text strong>App port</Text> is the port where the HTTP server listens inside your container, not a browser URL or host port. Enter the actual number and bind the process to <Text code>0.0.0.0</Text>; <Text code>127.0.0.1</Text> is not reachable from PolyGate. A Dockerfile <Text code>EXPOSE</Text> line alone does not verify the listening port. Set an HTTP health path only if that path returns HTTP 2xx or 3xx while the app is healthy.</Paragraph>
      <Paragraph>If a build succeeds but its app shows <Text strong>Failed</Text> and Events report <Text code>connection refused</Text>, compare the app port with the program's listening port. On <Link to="/">Overview</Link>, select <Text strong>Edit port</Text>, save the correct port, then select <Text strong>Deploy image</Text> with the same approved image. The Python template exposes port <Text code>8000</Text>.</Paragraph>
      <Paragraph>If <Text strong>Logs</Text> show <Text code>PermissionError</Text>, check that the app can read its files and write temporary data to <Text code>/tmp</Text> or a selected volume. Correct the image and redeploy it to the existing app.</Paragraph>
      <Paragraph>If a draft, stopped or failed app needs a different volume, use <Text strong>Edit storage</Text> on Overview. Failed deployments must finish cleanup first. Select a ready volume and its mount path, save, then choose <Text strong>Deploy image</Text>. Existing volume files are kept when you change or remove a binding; they are not copied to a different volume. Persistent apps need one replica. Do not delete the app just to change its storage.</Paragraph>
      <Paragraph>The maximum memory setting is <Text strong>4096 MiB (4 GiB)</Text> per container. A deployment may be refused when your account or the shared cluster is full.</Paragraph>
      <Paragraph>Use <Text strong>Logs</Text> on Overview to check recent output, events and resource usage. Newest log lines appear first. For a failed restart, select <Text strong>Previous attempt</Text> when available. Source build logs are under <Text strong>Source ZIP → Build history</Text>. Use <Text strong>Stop</Text> to stop an app and <Text strong>Delete</Text> to remove a stopped app.</Paragraph>
      <Paragraph>Files kept only inside an app may be lost when it restarts. Use <Link to="/volumes">Volumes</Link> for files you want to keep, and save your own copy of important files. If an error remains after checking the app status and Logs, give the laboratory administrator the app name and approximate time.</Paragraph>
    </Card>
  </>
}
