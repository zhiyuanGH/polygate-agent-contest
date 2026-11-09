# PolyGate student quick start (internal pilot)

For every click and matching live screenshots, see the [English walkthrough](PolyGate_Student_Walkthrough_EN.md) or [中文图文指南](PolyGate_学生全流程图文指南_ZH.md).

PolyGate currently accepts invited pilot accounts on the laboratory network. Open **https://YOUR-POLYGATE-HOST/** and select **Sign in**. The pilot uses a temporary self-signed certificate; check the IP address before accepting your browser's certificate warning. Keep your account password private. New accounts must choose a new password on first sign-in. For screenshots, see the [illustrated guide](PolyGate_Student_Illustrated_Guide.md).

## Deploy a container image

1. Open **My images**. Enter a public container image reference and select **Inspect image**.
2. Wait for the image to show **Approved**. PolyGate pins the digest and copies the image to its private registry. An image marked **Rejected** or **Failed** cannot be deployed.
3. Open **New app**. Enter a name, the image reference and the port on which your container listens. Set the CPU, memory and replica count, then select **Create draft**.
4. On **Overview**, choose **Deploy image** for that draft and select the approved image. Wait until the app shows **Running**, then choose **Open app**.

**App port** is the HTTP server's listening port inside the container, not a browser URL or host port. Your application must listen on `0.0.0.0` at that port. The samples use Python `8000`, Node.js `3000`, and Dockerfile `8080`; your own image may differ. A Dockerfile `EXPOSE` line does not confirm what port the process actually uses. If the app has a health endpoint returning HTTP 2xx or 3xx, enter its path in **HTTP health path**; otherwise leave the field empty to use the default TCP check. The maximum memory setting is **4096 MiB (4 GiB)** per container. Deployment can be refused when your account or the shared cluster capacity is full.

## Build from a source ZIP

For a first test, open **Help** and download one of the three sample ZIPs. The page lists its build method and app port; no cloud API key is needed.

1. Open **Source ZIP**. Choose **Use my Dockerfile**, **Python template** or **Node.js template**.
2. Put a `Dockerfile` at the ZIP root for the Dockerfile method. The Python template expects `main.py` at the root and an HTTP server on `0.0.0.0:8000`. The Node.js template expects `package.json`, `package-lock.json`, a `scripts.start` command and an HTTP server on `0.0.0.0:3000`.
3. Choose the ZIP file and select **Queue build**. The compressed ZIP must be at most **100 MiB**; its expanded contents must be at most **1 GiB**.
4. Follow **Build history** and use **View log** to diagnose a failed build. When the resulting image is **Approved**, select **Create and deploy app**, enter the app port and resources, and wait for **Running** on **Overview**.

To update an existing app from a new ZIP, build the new ZIP and wait for **Approved**. Select **Update a running app** beside that build, then choose **Update image** on the app you want to replace. The app keeps its saved port and resource settings, so the new container must listen on the same port. Once the new version is **Running**, use **Restore version** if you need a previously successful approved image revision.

## Manage an app

- On **Overview**, use **Logs** for recent output, events and resource samples. Use **Open app** to visit a running app.
- The quota card shows configured resources, the amount left to configure, and recent CPU and memory samples from running Pods. Drafts and stopped apps count toward configured quota; a missing Pod sample is marked as incomplete. Free quota does not guarantee that the cluster can schedule a new app immediately.
- Use **Stop** to remove its running workload and release its reserved resources. A stopped app can be deployed again. Use **Delete** to remove a stopped app.
- **Secrets** stores environment keys for an app. A saved value is not displayed again; replacing it takes effect when the app starts a new Pod. This internal pilot does not yet encrypt Kubernetes Secrets at rest: use short-lived tokens with spending limits, and do not save production credentials. Do not paste an API key into a Dockerfile, source ZIP or build log.
- **Update image**, **Restore version** and **Change resources** are available for a running app. A rolling update needs enough temporary capacity for the old and new Pods. A recreate update briefly stops the app.
- **Restart** replaces the app Pod without changing its image or settings. Choose **Recreate** for a brief interruption without extra quota, or **Rolling** to keep the app available while a replacement Pod starts; Rolling requires extra quota.

## Use persistent storage

Invited pilot accounts can open **Volumes** to request up to two volumes and 10 GiB total. Wait for **Ready**, then turn on **Persistent storage** in **New app**, select an unused volume and set its mount path. A volume can serve one app at a time. Stopping or deleting the app keeps its files; deleting the volume permanently removes them.

Pilot volumes have one online copy on the `.45` server. Daily and weekly backups go to the separate `.217` server, but a `.45` failure can interrupt access and changes since the latest backup may be lost. Keep your own copy of important files. If an operation fails, check the app status and **Logs** before retrying. If the error remains, give the laboratory operator the app name and approximate time.

## Sign out or switch accounts

Choose **Sign out** in the left navigation. This closes both your PolyGate session and the browser's identity-provider session. To use a different account in the same browser, select **Sign in** and enter the other account's credentials. Signing out does not stop a running app.

On desktop screens, the navigation and top bar stay in place while long workspace content scrolls inside the main panel. On narrow screens, the page scrolls normally.
