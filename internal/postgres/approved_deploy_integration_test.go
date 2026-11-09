package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"polygate/internal/domain"
	"polygate/internal/harbor"
	"polygate/internal/registry"
	"polygate/internal/server"
)

func TestQueueApprovedImageDeployIsAccountBound(t *testing.T) {
	url := os.Getenv("POLYGATE_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set POLYGATE_TEST_DATABASE_URL to run PostgreSQL integration test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	store, err := Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	aID, _ := server.NewID()
	bID, _ := server.NewID()
	for _, id := range []string{aID, bID} {
		if _, err := store.Pool.Exec(ctx, `INSERT INTO accounts(id,oidc_subject,username,display_name)
            VALUES($1,$2,$2,'Approved Deploy Test')`, id, "approved-deploy-"+id); err != nil {
			t.Fatal(err)
		}
	}
	defer func() {
		clean, done := context.WithTimeout(context.Background(), 10*time.Second)
		defer done()
		ids := []string{aID, bID}
		tx, err := store.Pool.Begin(clean)
		if err != nil {
			return
		}
		defer tx.Rollback(clean)
		_, _ = tx.Exec(clean, `UPDATE global_budget SET
			reserved_cpu_request_milli=reserved_cpu_request_milli-(SELECT COALESCE(SUM(cpu_request_milli),0) FROM (SELECT cpu_request_milli FROM resource_reservations WHERE account_id=ANY($1::uuid[]) UNION ALL SELECT cpu_request_milli FROM rollout_reservations WHERE account_id=ANY($1::uuid[])) u),
			reserved_cpu_limit_milli=reserved_cpu_limit_milli-(SELECT COALESCE(SUM(cpu_limit_milli),0) FROM (SELECT cpu_limit_milli FROM resource_reservations WHERE account_id=ANY($1::uuid[]) UNION ALL SELECT cpu_limit_milli FROM rollout_reservations WHERE account_id=ANY($1::uuid[])) u),
			reserved_memory_mib=reserved_memory_mib-(SELECT COALESCE(SUM(memory_mib),0) FROM (SELECT memory_mib FROM resource_reservations WHERE account_id=ANY($1::uuid[]) UNION ALL SELECT memory_mib FROM rollout_reservations WHERE account_id=ANY($1::uuid[])) u),
			reserved_pods=reserved_pods-(SELECT COALESCE(SUM(pods),0) FROM (SELECT pods FROM resource_reservations WHERE account_id=ANY($1::uuid[]) UNION ALL SELECT pods FROM rollout_reservations WHERE account_id=ANY($1::uuid[])) u)
            WHERE id=true`, ids)
		_, _ = tx.Exec(clean, `DELETE FROM outbox WHERE task_id IN (SELECT id FROM deployment_tasks WHERE account_id=ANY($1::uuid[]))`, ids)
		_, _ = tx.Exec(clean, `DELETE FROM rollout_reservations WHERE account_id=ANY($1::uuid[])`, ids)
		_, _ = tx.Exec(clean, `DELETE FROM deployment_tasks WHERE account_id=ANY($1::uuid[])`, ids)
		_, _ = tx.Exec(clean, `DELETE FROM resource_reservations WHERE account_id=ANY($1::uuid[])`, ids)
		_, _ = tx.Exec(clean, `DELETE FROM app_revisions WHERE account_id=ANY($1::uuid[])`, ids)
		_, _ = tx.Exec(clean, `DELETE FROM operations WHERE account_id=ANY($1::uuid[])`, ids)
		_, _ = tx.Exec(clean, `DELETE FROM apps WHERE account_id=ANY($1::uuid[])`, ids)
		_, _ = tx.Exec(clean, `DELETE FROM student_volumes WHERE account_id=ANY($1::uuid[])`, ids)
		_, _ = tx.Exec(clean, `DELETE FROM image_artifacts WHERE account_id=ANY($1::uuid[])`, ids)
		_, _ = tx.Exec(clean, `DELETE FROM accounts WHERE id=ANY($1::uuid[])`, ids)
		_ = tx.Commit(clean)
	}()
	spec := domain.AppSpec{Name: "approved-agent", Image: "registry.example/team/agent:v1", Port: 8080,
		Resources: domain.Resources{CPURequestMilli: 250, CPULimitMilli: 1000, MemoryMiB: 512, Replicas: 1}}
	app, _, err := store.Create(ctx, aID, "student-a", "approved-create-001", spec)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.QueueApprovedImageDeploy(ctx, aID, "student-a", app.ID,
		"approved-disabled-001", aID); !errors.Is(err, server.ErrImageDeployDisabled) {
		t.Fatalf("account without pilot grant reached image admission: %v", err)
	}
	if _, err := store.Pool.Exec(ctx, `UPDATE accounts SET image_deploy_enabled=true WHERE id=$1`, aID); err != nil {
		t.Fatal(err)
	}
	add := func(accountID, digit string) server.ImageArtifact {
		digest := "sha256:" + strings.Repeat(digit, 64)
		artifact, replay, err := store.SaveImageArtifact(ctx, accountID, "student", registry.Image{
			OS: "linux", Architecture: "amd64", RegistryHost: "registry.example",
			SubmittedReference: "registry.example/team/agent:v1",
			PinnedReference:    "registry.example/team/agent@" + digest,
			ManifestDigest:     digest, PlatformDigest: digest})
		if err != nil || replay {
			t.Fatalf("save image: %+v %v %v", artifact, replay, err)
		}
		return artifact
	}
	owned := add(aID, "a")
	foreign := add(bID, "b")
	rejected := add(aID, "c")
	failed := add(aID, "d")
	if _, err := store.Pool.Exec(ctx, `UPDATE image_artifacts SET scan_status='Failed'
        WHERE id=$1 AND account_id=$2`, failed.ID, aID); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, id string }{
		{"pending", owned.ID}, {"foreign", foreign.ID},
		{"failed", failed.ID}, {"missing", aID},
	} {
		if _, _, err := store.QueueApprovedImageDeploy(ctx, aID, "student-a", app.ID,
			"approved-"+tc.name+"-001", tc.id); domain.AsDomainError(err) == nil {
			t.Fatalf("%s image entered deployment queue: %v", tc.name, err)
		}
	}
	var taskCount, reservationCount int
	if err := store.Pool.QueryRow(ctx, `SELECT
        (SELECT count(*) FROM deployment_tasks WHERE account_id=$1),
        (SELECT count(*) FROM resource_reservations WHERE account_id=$1)`, aID).
		Scan(&taskCount, &reservationCount); err != nil || taskCount != 0 || reservationCount != 0 {
		t.Fatalf("failed admission reserved resources: %d %d %v", taskCount, reservationCount, err)
	}
	report := []byte(`{"vulnerabilities":[]}`)
	sum := sha256.Sum256(report)
	evidence := harbor.Evidence{Digest: owned.PlatformDigest, ReportID: "approved-report-1",
		ReportSHA256: "sha256:" + hex.EncodeToString(sum[:]), ScannerName: "Trivy",
		ScannedAt: time.Now().UTC(), Report: report, Severities: map[string]int{}}
	project, _ := harbor.AccountProjectName(aID)
	reference := harbor.RegistryHost + "/" + project + "/agent@" + owned.PlatformDigest
	if decision, _, err := store.recordImageScanDirect(ctx, aID, owned.ID, reference, evidence); err != nil || decision != "Approved" {
		t.Fatalf("approved evidence: %s %v", decision, err)
	}
	rejectedEvidence := evidence
	rejectedEvidence.Digest = rejected.PlatformDigest
	rejectedEvidence.ReportID = "rejected-report-1"
	rejectedEvidence.Total = 1
	rejectedEvidence.Severities = map[string]int{"high": 1}
	rejectedRef := harbor.RegistryHost + "/" + project + "/agent@" + rejected.PlatformDigest
	if decision, _, err := store.recordImageScanDirect(ctx, aID, rejected.ID,
		rejectedRef, rejectedEvidence); err != nil || decision != "Rejected" {
		t.Fatalf("rejected evidence: %s %v", decision, err)
	}
	listed, err := store.ListImageArtifacts(ctx, aID)
	if err != nil {
		t.Fatal(err)
	}
	foundRejectionSummary := false
	for _, item := range listed {
		if item.ID == rejected.ID {
			foundRejectionSummary = item.ScanStatus == "Rejected" &&
				item.ScanSummary != nil && item.ScanSummary.Severities["High"] == 1
		}
	}
	if !foundRejectionSummary {
		t.Fatal("student image list omitted normalized rejection summary")
	}
	if _, err := store.Pool.Exec(ctx, `UPDATE accounts SET enabled=false WHERE id=$1`, aID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.QueueApprovedImageDeploy(ctx, aID, "student-a", app.ID,
		"approved-account-disabled-001", owned.ID); !errors.Is(err, server.ErrDisabled) {
		t.Fatalf("disabled account reached approved image admission: %v", err)
	}
	if _, err := store.Pool.Exec(ctx, `UPDATE accounts SET enabled=true WHERE id=$1`, aID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.QueueApprovedImageDeploy(ctx, aID, "student-a", app.ID,
		"approved-rejected-001", rejected.ID); domain.AsDomainError(err) == nil {
		t.Fatalf("rejected scan entered deployment queue: %v", err)
	}
	if _, err := store.Pool.Exec(ctx, `UPDATE image_artifacts
        SET scan_summary=scan_summary-'policyVersion' WHERE id=$1`, owned.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.QueueApprovedImageDeploy(ctx, aID, "student-a", app.ID,
		"approved-policy-001", owned.ID); domain.AsDomainError(err) == nil {
		t.Fatalf("obsolete scan policy admitted: %v", err)
	}
	if _, err := store.Pool.Exec(ctx, `UPDATE image_artifacts
        SET scan_summary=jsonb_set(scan_summary,'{policyVersion}',to_jsonb($2::text),true)
        WHERE id=$1`, owned.ID, harbor.PolicyVersion); err != nil {
		t.Fatal(err)
	}
	task, replay, err := store.QueueApprovedImageDeploy(ctx, aID, "student-a", app.ID,
		"approved-deploy-001", owned.ID)
	if err != nil || replay || task.ID == "" {
		t.Fatalf("approved image not queued: %+v %v %v", task, replay, err)
	}
	if again, replay, err := store.QueueApprovedImageDeploy(ctx, aID, "student-a", app.ID,
		"approved-deploy-001", owned.ID); err != nil || !replay || again.ID != task.ID {
		t.Fatalf("approved image replay mismatch: %+v %v %v", again, replay, err)
	}
	if _, _, err := store.QueueTrustedDeploy(ctx, aID, "student-a", app.ID,
		"approved-deploy-001", reference, 8080); domain.AsDomainError(err) == nil {
		t.Fatalf("approved image idempotency key accepted for sample path: %v", err)
	}
	var sourceKind, artifactID, deployedDigest string
	if err := store.Pool.QueryRow(ctx, `SELECT r.source_kind,r.image_artifact_id::text,
        r.platform_image_digest FROM app_revisions r WHERE r.id=$1`, task.RevisionID).
		Scan(&sourceKind, &artifactID, &deployedDigest); err != nil || sourceKind != "image" ||
		artifactID != owned.ID || deployedDigest != reference {
		t.Fatalf("revision lost approved artifact: %s %s %s %v", sourceKind, artifactID, deployedDigest, err)
	}
	first, found, err := store.ClaimNext(ctx, "approved-worker", time.Minute)
	if err != nil || !found || first.ID != task.ID || first.Kind != "Deploy" {
		t.Fatalf("initial claim: %+v found=%v err=%v", first, found, err)
	}
	if err := store.Complete(ctx, first.ID, "approved-worker"); err != nil {
		t.Fatal("complete initial deployment:", err)
	}
	approve := func(digit, reportID string) server.ImageArtifact {
		artifact := add(aID, digit)
		newEvidence := evidence
		newEvidence.Digest = artifact.PlatformDigest
		newEvidence.ReportID = reportID
		newReference := harbor.RegistryHost + "/" + project + "/agent@" + artifact.PlatformDigest
		if decision, _, err := store.recordImageScanDirect(ctx, aID, artifact.ID, newReference, newEvidence); err != nil || decision != "Approved" {
			t.Fatalf("approve update artifact: %s %v", decision, err)
		}
		return artifact
	}
	secondImage := approve("e", "update-report-1")
	if _, _, err := store.QueueApprovedImageUpdate(ctx, aID, "student-a", app.ID,
		"update-foreign-001", foreign.ID); domain.AsDomainError(err) == nil {
		t.Fatalf("foreign update artifact accepted: %v", err)
	}
	update, replay, err := store.QueueApprovedImageUpdate(ctx, aID, "student-a", app.ID,
		"update-image-001", secondImage.ID)
	if err != nil || replay || update.ID == "" {
		t.Fatalf("queue update: %+v replay=%v err=%v", update, replay, err)
	}
	if again, replay, err := store.QueueApprovedImageUpdate(ctx, aID, "student-a", app.ID,
		"update-image-001", secondImage.ID); err != nil || !replay || again.ID != update.ID {
		t.Fatalf("update replay: %+v replay=%v err=%v", again, replay, err)
	}
	var currentRevision string
	if err := store.Pool.QueryRow(ctx, `SELECT revision_id::text FROM resource_reservations WHERE app_id=$1`, app.ID).Scan(&currentRevision); err != nil || currentRevision != task.RevisionID {
		t.Fatalf("old revision was replaced before rollout: %s %v", currentRevision, err)
	}
	var surgeCount int
	if err := store.Pool.QueryRow(ctx, `SELECT COUNT(*) FROM rollout_reservations WHERE app_id=$1`, app.ID).Scan(&surgeCount); err != nil || surgeCount != 1 {
		t.Fatalf("surge not reserved: count=%d err=%v", surgeCount, err)
	}
	claimed, found, err := store.ClaimNext(ctx, "update-worker", time.Minute)
	if err != nil || !found || claimed.ID != update.ID || claimed.Kind != "Update" {
		t.Fatalf("update claim: %+v found=%v err=%v", claimed, found, err)
	}
	if err := store.Complete(ctx, update.ID, "update-worker"); err != nil {
		t.Fatal("complete update:", err)
	}
	if err := store.Pool.QueryRow(ctx, `SELECT revision_id::text FROM resource_reservations WHERE app_id=$1`, app.ID).Scan(&currentRevision); err != nil || currentRevision != update.RevisionID {
		t.Fatalf("successful update did not become current: %s %v", currentRevision, err)
	}
	if err := store.Pool.QueryRow(ctx, `SELECT COUNT(*) FROM rollout_reservations WHERE app_id=$1`, app.ID).Scan(&surgeCount); err != nil || surgeCount != 0 {
		t.Fatalf("successful update retained surge: count=%d err=%v", surgeCount, err)
	}
	if _, _, err := store.QueueApprovedRevisionRollback(ctx, aID, "student-a", app.ID,
		"restore-foreign-001", foreign.ID); domain.AsDomainError(err) == nil {
		t.Fatalf("foreign revision accepted for rollback: %v", err)
	}
	restored, replay, err := store.QueueApprovedRevisionRollback(ctx, aID, "student-a", app.ID,
		"restore-image-001", task.RevisionID)
	if err != nil || replay || restored.ID == "" {
		t.Fatalf("queue manual rollback: %+v replay=%v err=%v", restored, replay, err)
	}
	if again, replay, err := store.QueueApprovedRevisionRollback(ctx, aID, "student-a", app.ID,
		"restore-image-001", task.RevisionID); err != nil || !replay || again.ID != restored.ID {
		t.Fatalf("rollback replay: %+v replay=%v err=%v", again, replay, err)
	}
	claimed, found, err = store.ClaimNext(ctx, "restore-worker", time.Minute)
	if err != nil || !found || claimed.ID != restored.ID || claimed.Kind != "Update" {
		t.Fatalf("manual rollback claim: %+v found=%v err=%v", claimed, found, err)
	}
	if err := store.Complete(ctx, restored.ID, "restore-worker"); err != nil {
		t.Fatal("complete manual rollback:", err)
	}
	var restoreAudit string
	if err := store.Pool.QueryRow(ctx, `SELECT state FROM operations WHERE id=$1 AND kind='ROLLBACK_REQUEST'`, restored.OperationID).Scan(&restoreAudit); err != nil || restoreAudit != "Succeeded" {
		t.Fatalf("manual rollback audit: %s %v", restoreAudit, err)
	}
	if err := store.Pool.QueryRow(ctx, `SELECT revision_id::text FROM resource_reservations WHERE app_id=$1`, app.ID).Scan(&currentRevision); err != nil || currentRevision != restored.RevisionID {
		t.Fatalf("manual rollback did not create current revision: %s %v", currentRevision, err)
	}
	thirdImage := approve("f", "update-report-2")
	failedUpdate, replay, err := store.QueueApprovedImageUpdate(ctx, aID, "student-a", app.ID,
		"update-image-002", thirdImage.ID)
	if err != nil || replay {
		t.Fatalf("queue failed-update trial: %+v replay=%v err=%v", failedUpdate, replay, err)
	}
	claimed, found, err = store.ClaimNext(ctx, "failed-update-worker", time.Minute)
	if err != nil || !found || claimed.ID != failedUpdate.ID {
		t.Fatalf("failed-update claim: %+v found=%v err=%v", claimed, found, err)
	}
	if _, err := store.Pool.Exec(ctx, `UPDATE deployment_tasks SET attempt=12 WHERE id=$1`, failedUpdate.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.Retry(ctx, failedUpdate.ID, "failed-update-worker", "WAITING_FOR_READY", 0); err != nil {
		t.Fatal("queue automatic rollback:", err)
	}
	rollback, found, err := store.ClaimNext(ctx, "rollback-worker", time.Minute)
	if err != nil || !found || rollback.Kind != "Rollback" || rollback.RevisionID != restored.RevisionID {
		t.Fatalf("rollback claim: %+v found=%v err=%v", rollback, found, err)
	}
	if err := store.Complete(ctx, rollback.ID, "rollback-worker"); err != nil {
		t.Fatal("complete rollback:", err)
	}
	var appStatus, auditState string
	if err := store.Pool.QueryRow(ctx, `SELECT a.status,o.state FROM apps a JOIN operations o ON o.app_id=a.id
		WHERE a.id=$1 AND o.kind='UPDATE_REQUEST' AND o.detail->>'taskId'=$2`, app.ID, failedUpdate.ID).Scan(&appStatus, &auditState); err != nil || appStatus != "Running" || auditState != "Failed" {
		t.Fatalf("rollback outcome: app=%s operation=%s err=%v", appStatus, auditState, err)
	}
	if err := store.Pool.QueryRow(ctx, `SELECT revision_id::text FROM resource_reservations WHERE app_id=$1`, app.ID).Scan(&currentRevision); err != nil || currentRevision != restored.RevisionID {
		t.Fatalf("rollback changed successful revision: %s %v", currentRevision, err)
	}
	if err := store.Pool.QueryRow(ctx, `SELECT COUNT(*) FROM rollout_reservations WHERE app_id=$1`, app.ID).Scan(&surgeCount); err != nil || surgeCount != 0 {
		t.Fatalf("rollback retained surge: count=%d err=%v", surgeCount, err)
	}
	if _, err := store.Pool.Exec(ctx, `UPDATE accounts SET volume_enabled=true WHERE id=$1`, aID); err != nil {
		t.Fatal(err)
	}
	volume, _, err := store.CreateVolume(ctx, aID, "student-a", "persistent-volume-001", domain.VolumeSpec{Name: "persistent-data", SizeGiB: 5})
	if err != nil {
		t.Fatal("create persistent test volume:", err)
	}
	if _, err := store.Pool.Exec(ctx, `UPDATE student_volumes SET state='Ready' WHERE id=$1`, volume.ID); err != nil {
		t.Fatal(err)
	}
	persistentSpec := spec
	persistentSpec.Name = "persistent-agent"
	persistentSpec.Persistent = true
	persistentSpec.VolumeID = volume.ID
	persistentSpec.MountPath = "/data"
	persistentApp, _, err := store.Create(ctx, aID, "student-a", "persistent-app-001", persistentSpec)
	if err != nil {
		t.Fatal("create persistent test app:", err)
	}
	persistentDeploy, _, err := store.QueueApprovedImageDeploy(ctx, aID, "student-a", persistentApp.ID,
		"persistent-deploy-001", owned.ID)
	if err != nil {
		t.Fatal("queue persistent deployment:", err)
	}
	persistentTask, found, err := store.ClaimNext(ctx, "persistent-worker", time.Minute)
	if err != nil || !found || persistentTask.ID != persistentDeploy.ID || !persistentTask.Spec.Persistent {
		t.Fatalf("persistent deploy claim: %+v found=%v err=%v", persistentTask, found, err)
	}
	if err := store.Complete(ctx, persistentDeploy.ID, "persistent-worker"); err != nil {
		t.Fatal("complete persistent deployment:", err)
	}
	var budgetBefore, budgetAfter domain.Usage
	budgetSQL := `SELECT reserved_cpu_request_milli,reserved_cpu_limit_milli,reserved_memory_mib,reserved_pods FROM global_budget WHERE id=true`
	if err := store.Pool.QueryRow(ctx, budgetSQL).Scan(&budgetBefore.CPURequestMilli, &budgetBefore.CPULimitMilli,
		&budgetBefore.MemoryMiB, &budgetBefore.Pods); err != nil {
		t.Fatal(err)
	}
	persistentUpdate, _, err := store.QueueApprovedImageUpdate(ctx, aID, "student-a", persistentApp.ID,
		"persistent-update-001", secondImage.ID)
	if err != nil {
		t.Fatal("queue Recreate update:", err)
	}
	if err := store.Pool.QueryRow(ctx, budgetSQL).Scan(&budgetAfter.CPURequestMilli, &budgetAfter.CPULimitMilli,
		&budgetAfter.MemoryMiB, &budgetAfter.Pods); err != nil || budgetAfter != budgetBefore {
		t.Fatalf("Recreate reserved surge: before=%+v after=%+v err=%v", budgetBefore, budgetAfter, err)
	}
	if err := store.Pool.QueryRow(ctx, `SELECT COUNT(*) FROM rollout_reservations WHERE app_id=$1`, persistentApp.ID).Scan(&surgeCount); err != nil || surgeCount != 0 {
		t.Fatalf("Recreate created surge row: count=%d err=%v", surgeCount, err)
	}
	persistentTask, found, err = store.ClaimNext(ctx, "persistent-update-worker", time.Minute)
	if err != nil || !found || persistentTask.ID != persistentUpdate.ID || persistentTask.Kind != "Update" || !persistentTask.Spec.Persistent {
		t.Fatalf("persistent update claim: %+v found=%v err=%v", persistentTask, found, err)
	}
	if err := store.Complete(ctx, persistentUpdate.ID, "persistent-update-worker"); err != nil {
		t.Fatal("complete Recreate update:", err)
	}
	persistentRestore, _, err := store.QueueApprovedRevisionRollback(ctx, aID, "student-a", persistentApp.ID,
		"persistent-restore-001", persistentDeploy.RevisionID)
	if err != nil {
		t.Fatal("queue Recreate restore:", err)
	}
	persistentTask, found, err = store.ClaimNext(ctx, "persistent-restore-worker", time.Minute)
	if err != nil || !found || persistentTask.ID != persistentRestore.ID || !persistentTask.Spec.Persistent {
		t.Fatalf("persistent restore claim: %+v found=%v err=%v", persistentTask, found, err)
	}
	if err := store.Complete(ctx, persistentRestore.ID, "persistent-restore-worker"); err != nil {
		t.Fatal("complete Recreate restore:", err)
	}
	persistentFailed, _, err := store.QueueApprovedImageUpdate(ctx, aID, "student-a", persistentApp.ID,
		"persistent-failure-001", thirdImage.ID)
	if err != nil {
		t.Fatal("queue failing Recreate update:", err)
	}
	persistentTask, found, err = store.ClaimNext(ctx, "persistent-failed-worker", time.Minute)
	if err != nil || !found || persistentTask.ID != persistentFailed.ID {
		t.Fatalf("failing persistent claim: %+v found=%v err=%v", persistentTask, found, err)
	}
	if _, err := store.Pool.Exec(ctx, `UPDATE deployment_tasks SET attempt=12 WHERE id=$1`, persistentFailed.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.Retry(ctx, persistentFailed.ID, "persistent-failed-worker", "WAITING_FOR_READY", 0); err != nil {
		t.Fatal("queue persistent automatic rollback:", err)
	}
	persistentTask, found, err = store.ClaimNext(ctx, "persistent-rollback-worker", time.Minute)
	if err != nil || !found || persistentTask.Kind != "Rollback" || persistentTask.RevisionID != persistentRestore.RevisionID || !persistentTask.Spec.Persistent {
		t.Fatalf("persistent rollback claim: %+v found=%v err=%v", persistentTask, found, err)
	}
	if err := store.Complete(ctx, persistentTask.ID, "persistent-rollback-worker"); err != nil {
		t.Fatal("complete persistent automatic rollback:", err)
	}
	if err := store.Pool.QueryRow(ctx, `SELECT revision_id::text FROM resource_reservations WHERE app_id=$1`, persistentApp.ID).Scan(&currentRevision); err != nil || currentRevision != persistentRestore.RevisionID {
		t.Fatalf("persistent rollback changed successful revision: %s %v", currentRevision, err)
	}
	if err := store.Pool.QueryRow(ctx, `SELECT COUNT(*) FROM rollout_reservations WHERE app_id=$1`, persistentApp.ID).Scan(&surgeCount); err != nil || surgeCount != 0 {
		t.Fatalf("persistent rollback retained surge: count=%d err=%v", surgeCount, err)
	}
	persistentStop, _, err := store.QueueStop(ctx, aID, "student-a", persistentApp.ID, "persistent-stop-001")
	if err != nil {
		t.Fatal("queue persistent stop:", err)
	}
	persistentTask, found, err = store.ClaimNext(ctx, "persistent-stop-worker", time.Minute)
	if err != nil || !found || persistentTask.ID != persistentStop.ID {
		t.Fatalf("persistent stop claim: %+v found=%v err=%v", persistentTask, found, err)
	}
	if err := store.CompleteStop(ctx, persistentStop.ID, "persistent-stop-worker"); err != nil {
		t.Fatal("complete persistent stop:", err)
	}
	if err := store.Delete(ctx, aID, "student-a", persistentApp.ID); err != nil {
		t.Fatal("delete stopped persistent app:", err)
	}
}
