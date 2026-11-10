# PolyGate sample source ZIPs

Use these three small projects to try PolyGate's **Source ZIP** workflow. They work without cloud credentials or model API access.

Download a ZIP here or from the PolyGate **Help** page. Upload it without extracting it.

The full browser steps are in the [English illustrated walkthrough](../../docs/PolyGate_Student_Walkthrough_EN.md) and [Chinese illustrated walkthrough](../../docs/PolyGate_学生全流程图文指南_ZH.md).

| ZIP | Build method | App port | What you can try |
| --- | --- | ---: | --- |
| [`mock-agent-python.zip`](dist/mock-agent-python.zip) | Python template | 8000 | Ask a question and see a mock reply |
| [`mock-counter-node.zip`](dist/mock-counter-node.zip) | Node.js template | 3000 | Click the counter button |
| [`mock-inspector-dockerfile.zip`](dist/mock-inspector-dockerfile.zip) | Use my Dockerfile | 8080 | Follow the example links |

In PolyGate, open **Source ZIP**, choose the matching build method, upload the ZIP, and select **Queue build**. After the build and image scan show success, choose **Create and deploy app** and enter the port above. For the HTTP health path, use `/health`.

The Python sample returns a mock reply. The Node.js counter resets when the app restarts. These examples do not call a real model.

The projects work at an app URL such as `https://apps.polylink.org/<username>/<app-name>/`. Use **Open app** in PolyGate to open the correct address.
