# PolyGate student quick start

For every click and matching live screenshots, see the [English walkthrough](PolyGate_Student_Walkthrough_EN.md) or [中文图文指南](PolyGate_学生全流程图文指南_ZH.md).

Open **https://polygate.polylink.org/** and select **Sign in**. Enter the account details provided by the laboratory administrator. If prompted, set a new password before continuing. The illustrated walkthrough above shows every step with screenshots.

## Deploy a container image

1. Open **My images**. Enter a public container image reference and select **Inspect image**, or select **Upload image** to submit a local `docker save` `.tar` containing one `linux/amd64` image (up to 1 GiB).
2. Wait for **Approved**. If the result is **Rejected**, select **View report** or **Download JSON** to see why. If it is **Failed**, select **View failure** for the error details. Only approved images can be deployed.
3. Open **New app**. Enter a name and the port on which your container listens. Set the CPU, memory and replica count, then select **Save draft**.
4. On **Overview**, choose **Deploy image** for that draft and select the approved image. Wait until the app shows **Running**, then choose **Open app**.

**App port** is the HTTP server's listening port inside the container, not a browser URL or host port. Your application must listen on `0.0.0.0` at that port. The samples use Python `8000`, Node.js `3000`, and Dockerfile `8080`; your own image may differ. A Dockerfile `EXPOSE` line does not confirm what port the process actually uses. If the app has a health endpoint returning HTTP 2xx or 3xx, enter its path in **HTTP health path**; otherwise leave the field empty to use the default TCP check. The maximum memory setting is **4096 MiB (4 GiB)** per container. Deployment can be refused when your account or the shared cluster capacity is full.

If **Overview → Logs** shows `PermissionError`, check that the app can read its files and write temporary data to `/tmp` or a selected volume. Fix the image, submit it again, and deploy it to the existing app.

## Public app URL and base path

**Open app** uses `https://apps.polylink.org/<username>/<app-name>/`. Build your page assets, API calls and navigation with relative URLs, or configure your framework to use this base path. Root URLs such as `/assets` and `/api` may fail. Check the complete app in a browser after deployment. See the [path setup guide](PolyGate_学生作品路径适配.md) if your framework needs additional settings.

## Build from a source ZIP

For a first test, open **Help** and download one of the three sample ZIPs. The page lists its build method and app port; no cloud API key is needed.

1. Open **Source ZIP**. Choose **Use my Dockerfile**, **Python template** or **Node.js template**.
2. Put a `Dockerfile` at the ZIP root for the Dockerfile method. The Python template expects `main.py` at the root and an HTTP server on `0.0.0.0:8000`. The Node.js template expects `package.json`, `package-lock.json`, a `scripts.start` command and an HTTP server on `0.0.0.0:3000`.
3. Choose the ZIP file and select **Queue build**. The compressed ZIP must be at most **100 MiB**; its expanded contents must be at most **1 GiB**.
4. Follow **Build history** and use **View log** to diagnose a failed build. When the resulting image is **Approved**, select **Create and deploy app**, enter the app port and resources, and wait for **Running** on **Overview**.

To update an existing app from a new ZIP, build the new ZIP and wait for **Approved**. Select **Update a running app** beside that build, then choose **Update image** on the app you want to replace. The app keeps its saved port and resource settings, so the new container must listen on the same port. Once the new version is **Running**, use **Restore version** if you need a previously successful approved image revision.

## Manage an app

- On **Overview**, use **Logs** for recent output, events and resource samples. Use **Open app** to visit a running app.
- The quota card shows your configured resources, remaining quota, and recent usage. Drafts and stopped apps also count toward configured quota.
- Use **Stop** to remove its running workload and release its reserved resources. A stopped app can be deployed again. Use **Delete** to remove a stopped app.
- Use **Secrets** for app environment keys. A saved value is not displayed again. Keep your own copy and do not put keys in a Dockerfile, source ZIP or build log.
- **Update image**, **Restore version** and **Change resources** are available for a running app. A rolling update needs enough temporary capacity; a recreate update briefly stops the app.
- **Restart** restarts the app without changing its image or settings. **Recreate** briefly interrupts the app; **Rolling** needs additional capacity while the new version starts.

## Use persistent storage

Open **Volumes** to request storage, then wait for **Ready**. In **New app**, enable **Use persistent storage**, select an unused volume, and set its mount path. A volume can serve one app at a time. Stopping or deleting the app keeps its files; deleting the volume permanently removes them.

Keep your own copy of important files. If an operation fails, check the app status and **Logs**. If the error remains, give the laboratory administrator the app name and approximate time.

## Sign out or switch accounts

Choose **Sign out** in the left navigation. To use a different account in the same browser, select **Sign in** and enter that account's credentials. Signing out does not stop a running app.
