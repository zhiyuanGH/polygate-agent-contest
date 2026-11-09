import { Alert, Card, Typography } from 'antd'
import { Link } from 'react-router-dom'

const { Title, Paragraph, Text } = Typography

export default function HelpPage() {
  return <>
    <div className="page-heading"><div><span className="eyebrow">GET STARTED</span><Title level={2}>Help</Title>
      <Paragraph type="secondary">Deploy a container image or build one from a source ZIP, then open your running app.</Paragraph>
    </div></div>
    <Alert type="info" showIcon style={{ marginBottom: 20 }} message="Internal pilot"
      description="PolyGate is currently available to invited accounts on the laboratory network. Use the provided HTTPS address to sign in." />
    <Card className="form-card" title="Step-by-step illustrated guides" style={{ marginBottom: 20 }}>
      <Paragraph>Follow a complete example from sign-in and ZIP download through build, deployment, app access, logs and cleanup.</Paragraph>
      <Paragraph><a href="/student-guide-en.html" target="_blank" rel="noopener noreferrer">English guide</a> · <a href="/student-guide-zh.html" target="_blank" rel="noopener noreferrer">Chinese guide</a></Paragraph>
    </Card>
    <Card className="form-card" title="Deploy a container image" style={{ marginBottom: 20 }}>
      <ol>
        <li>Open <Link to="/images">My images</Link>. Inspect a public image reference or upload a local <Text code>docker save</Text> tar containing one <Text code>linux/amd64</Text> image (up to 1 GiB).</li>
        <li>Wait until the image is <Text strong>Approved</Text>. High findings remain visible but do not block deployment; Critical, Unknown and unrecognized findings do. For <Text strong>Rejected</Text>, use <Text strong>View report</Text> or <Text strong>Download JSON</Text>. For <Text strong>Failed</Text>, use <Text strong>View failure</Text> to inspect and download the scan error summary.</li>
        <li>Open <Link to="/apps/new">New app</Link>. Enter an app name, the port your container listens on, and its resources. Select <Text strong>Create draft</Text>.</li>
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
      <Paragraph><Text strong>App port</Text> is the port where the HTTP server listens inside your container, not a browser URL or host port. Enter the actual number and bind the process to <Text code>0.0.0.0</Text>; <Text code>127.0.0.1</Text> is not reachable from PolyGate. A Dockerfile <Text code>EXPOSE</Text> line alone does not verify the listening port. Set an HTTP health path only if that path returns HTTP 2xx or 3xx while the app is healthy.</Paragraph>
      <Paragraph>If a build succeeds but its app shows <Text strong>Failed</Text> and Events report <Text code>connection refused</Text>, compare the app port with the program's listening port. On <Link to="/">Overview</Link>, select <Text strong>Edit port</Text>, save the correct port, then select <Text strong>Deploy image</Text> with the same approved image. The Python template exposes port <Text code>8000</Text>.</Paragraph>
      <Paragraph>If Logs show <Text code>PermissionError</Text> while importing code, check file permissions in your image. PolyGate runs the container as UID/GID <Text code>65532</Text> with a read-only root filesystem. Make program files and parent directories readable by that UID (for example, <Text code>RUN chmod -R a+rX /app</Text> in a Dockerfile), and write temporary data under <Text code>/tmp</Text> or a mounted volume. A Dockerfile <Text code>USER</Text> directive does not override the platform UID.</Paragraph>
      <Paragraph>The maximum memory setting is <Text strong>4096 MiB (4 GiB)</Text> per container. A deployment may be refused when your account or the shared cluster is full.</Paragraph>
      <Paragraph>Use <Text strong>Logs</Text> on Overview to inspect container output, events and resource usage. Current and historical container logs show the newest line first. If your app has multiple Pods, choose the Pod in the diagnostics panel. The selected Pod shows waiting and previous exit reasons, such as an image pull error or OOM kill, when Kubernetes reports them. After a restart, turn on <Text strong>Previous attempt</Text> to read the failed container's output. Historical logs can remain available after a Pod is removed, for up to seven days. Load older pages to inspect beyond the latest 200 lines, then choose a discovered Pod under <Text strong>Historical Pod</Text> to show only its logs. Source build logs are under <Text strong>Source ZIP → Build history</Text>. Under <Text strong>My images</Text>, completed scans have vulnerability reports; failed scans have a downloadable failure summary with the error code and attempt count. <Text strong>Stop</Text> removes the running workload and releases its reserved resources; <Text strong>Delete</Text> removes a stopped app.</Paragraph>
      <Paragraph>Files written only inside a container are temporary. Use <Link to="/volumes">Volumes</Link> for persistent files; each account can allocate up to two volumes totaling 10 GiB. Pilot volumes have one online replica, so a storage node outage interrupts access. If an error remains after checking the app status and Logs, give the laboratory operator the app name and approximate time.</Paragraph>
    </Card>
  </>
}
