package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"reflect"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"polygate/internal/domain"
	"polygate/internal/server"
)

type Store struct{ Pool *pgxpool.Pool }

func Open(ctx context.Context, databaseURL string) (*Store, error) {
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		return nil, err
	}
	if err = pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return &Store{Pool: pool}, nil
}

func (s *Store) Close() { s.Pool.Close() }

func (s *Store) Account(ctx context.Context, id string) (server.Account, error) {
	var a server.Account
	var enabled bool
	err := s.Pool.QueryRow(ctx, `SELECT id::text, username, display_name, role, enabled, image_deploy_enabled,
		source_build_enabled,volume_enabled,
        quota_cpu_request_milli, quota_cpu_limit_milli, quota_memory_mib,
        quota_pods, quota_apps FROM accounts WHERE id=$1`, id).Scan(
		&a.ID, &a.Username, &a.DisplayName, &a.Role, &enabled, &a.ImageDeployEnabled, &a.SourceBuildEnabled, &a.VolumeEnabled,
		&a.Quota.CPURequestMilli, &a.Quota.CPULimitMilli,
		&a.Quota.MemoryMiB, &a.Quota.Pods, &a.Quota.Apps)
	if err != nil {
		return a, storeError(err)
	}
	if !enabled {
		return a, server.ErrDisabled
	}
	return a, nil
}

const appColumns = `id::text, name, image, port, cpu_request_milli,
    cpu_limit_milli, memory_mib, replicas, persistent,
    COALESCE(volume_id::text,''),COALESCE(mount_path,''),health_path,secret_keys,status,created_at`

type rowScanner interface{ Scan(...any) error }

func scanApp(row rowScanner) (server.App, error) {
	var a server.App
	err := row.Scan(&a.ID, &a.Spec.Name, &a.Spec.Image, &a.Spec.Port,
		&a.Spec.Resources.CPURequestMilli, &a.Spec.Resources.CPULimitMilli,
		&a.Spec.Resources.MemoryMiB, &a.Spec.Resources.Replicas,
		&a.Spec.Persistent, &a.Spec.VolumeID, &a.Spec.MountPath, &a.Spec.HealthPath, &a.Spec.SecretKeys, &a.Status, &a.CreatedAt)
	if len(a.Spec.SecretKeys) == 0 {
		a.Spec.SecretKeys = nil
	}
	return a, err
}

func (s *Store) List(ctx context.Context, id string) ([]server.App, domain.Usage, error) {
	rows, err := s.Pool.Query(ctx, `SELECT `+appColumns+` FROM apps WHERE account_id=$1 ORDER BY created_at,id`, id)
	if err != nil {
		return nil, domain.Usage{}, storeError(err)
	}
	defer rows.Close()
	items := make([]server.App, 0)
	var usage domain.Usage
	for rows.Next() {
		item, err := scanApp(rows)
		if err != nil {
			return nil, domain.Usage{}, storeError(err)
		}
		items = append(items, item)
		usage = usage.Add(item.Spec.Resources.Usage())
	}
	if err = rows.Err(); err != nil {
		return nil, domain.Usage{}, storeError(err)
	}
	return items, usage, nil
}

func (s *Store) Get(ctx context.Context, id, appID string) (server.App, error) {
	a, err := scanApp(s.Pool.QueryRow(ctx, `SELECT `+appColumns+` FROM apps WHERE account_id=$1 AND id=$2`, id, appID))
	if err != nil {
		return a, storeError(err)
	}
	return a, nil
}

func (s *Store) Create(ctx context.Context, id, subject, key string, spec domain.AppSpec) (server.App, bool, error) {
	return s.create(ctx, id, subject, key, spec, nil)
}

func (s *Store) CreateWithSecretCheck(ctx context.Context, id, subject, key string, spec domain.AppSpec,
	check func(context.Context) error) (server.App, bool, error) {
	return s.create(ctx, id, subject, key, spec, check)
}

func (s *Store) create(ctx context.Context, id, subject, key string, spec domain.AppSpec,
	check func(context.Context) error) (server.App, bool, error) {
	if err := domain.ValidateSpec(spec); err != nil {
		return server.App{}, false, err
	}
	if len(spec.SecretKeys) == 0 {
		spec.SecretKeys = nil
	}
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return server.App{}, false, storeError(err)
	}
	defer tx.Rollback(ctx)
	var quota domain.Quota
	var enabled, volumeEnabled bool
	err = tx.QueryRow(ctx, `SELECT enabled,volume_enabled,quota_cpu_request_milli,quota_cpu_limit_milli,
        quota_memory_mib,quota_pods,quota_apps FROM accounts WHERE id=$1 FOR UPDATE`, id).Scan(
		&enabled, &volumeEnabled, &quota.CPURequestMilli, &quota.CPULimitMilli, &quota.MemoryMiB, &quota.Pods, &quota.Apps)
	if err != nil {
		return server.App{}, false, storeError(err)
	}
	if !enabled {
		return server.App{}, false, server.ErrDisabled
	}
	if spec.Persistent && !volumeEnabled {
		return server.App{}, false, server.ErrVolumeDisabled
	}
	prev, err := scanApp(tx.QueryRow(ctx, `SELECT `+appColumns+` FROM apps WHERE account_id=$1 AND idempotency_key=$2`, id, key))
	if err == nil {
		if !reflect.DeepEqual(prev.Spec, spec) {
			return server.App{}, false, &domain.Error{Code: "IDEMPOTENCY_KEY_REUSED", Message: "This request key was already used with different settings."}
		}
		return prev, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return server.App{}, false, storeError(err)
	}
	if check != nil {
		if err := check(ctx); err != nil {
			return server.App{}, false, err
		}
	}
	if spec.Persistent {
		var state string
		err = tx.QueryRow(ctx, `SELECT state FROM student_volumes
			WHERE account_id=$1 AND id=$2 FOR SHARE`, id, spec.VolumeID).Scan(&state)
		if errors.Is(err, pgx.ErrNoRows) {
			return server.App{}, false, &domain.Error{Code: "VOLUME_NOT_READY", Field: "volumeId", Message: "Select a ready volume from this account."}
		}
		if err != nil {
			return server.App{}, false, storeError(err)
		}
		if state != "Ready" {
			return server.App{}, false, &domain.Error{Code: "VOLUME_NOT_READY", Field: "volumeId", Message: "Wait until this volume is ready."}
		}
	}
	rows, err := tx.Query(ctx, `SELECT cpu_request_milli,cpu_limit_milli,memory_mib,replicas
        FROM apps WHERE account_id=$1`, id)
	if err != nil {
		return server.App{}, false, storeError(err)
	}
	var usage domain.Usage
	for rows.Next() {
		var res domain.Resources
		if err = rows.Scan(&res.CPURequestMilli, &res.CPULimitMilli, &res.MemoryMiB, &res.Replicas); err != nil {
			rows.Close()
			return server.App{}, false, storeError(err)
		}
		usage = usage.Add(res.Usage())
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return server.App{}, false, storeError(err)
	}
	if err = checkNoPendingQuotaChange(ctx, tx, id); err != nil {
		return server.App{}, false, err
	}
	if err = domain.CheckAdmission(usage, domain.Usage{}, spec.Resources.Usage(), quota, domain.Quota{}); err != nil {
		return server.App{}, false, err
	}
	appID, err := server.NewID()
	if err != nil {
		return server.App{}, false, err
	}
	opID, err := server.NewID()
	if err != nil {
		return server.App{}, false, err
	}
	var createdAt time.Time
	err = tx.QueryRow(ctx, `INSERT INTO apps(id,account_id,name,image,port,cpu_request_milli,
	        cpu_limit_milli,memory_mib,replicas,persistent,status,idempotency_key,volume_id,mount_path,health_path,secret_keys)
	        VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,'Draft',$11,NULLIF($12,'')::uuid,NULLIF($13,''),$14,$15) RETURNING created_at`,
		appID, id, spec.Name, spec.Image, spec.Port, spec.Resources.CPURequestMilli,
		spec.Resources.CPULimitMilli, spec.Resources.MemoryMiB, spec.Resources.Replicas, spec.Persistent,
		key, spec.VolumeID, spec.MountPath, spec.HealthPath, append([]string{}, spec.SecretKeys...)).Scan(&createdAt)
	if err != nil {
		return server.App{}, false, createError(err)
	}
	_, err = tx.Exec(ctx, `INSERT INTO operations(id,account_id,app_id,actor_subject,kind,state)
        VALUES($1,$2,$3,$4,'CREATE_DRAFT','Succeeded')`, opID, id, appID, subject)
	if err != nil {
		return server.App{}, false, storeError(err)
	}
	if err = tx.Commit(ctx); err != nil {
		return server.App{}, false, storeError(err)
	}
	return server.App{ID: appID, Spec: spec, Status: "Draft", CreatedAt: createdAt}, false, nil
}

func (s *Store) Delete(ctx context.Context, id, subject, appID string) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return storeError(err)
	}
	defer tx.Rollback(ctx)
	var enabled bool
	if err = tx.QueryRow(ctx, `SELECT enabled FROM accounts WHERE id=$1 FOR UPDATE`, id).Scan(&enabled); err != nil {
		return storeError(err)
	}
	if !enabled {
		return server.ErrDisabled
	}
	var status string
	if err = tx.QueryRow(ctx, `SELECT status FROM apps WHERE account_id=$1 AND id=$2 FOR UPDATE`, id, appID).Scan(&status); err != nil {
		return storeError(err)
	}
	if status != "Draft" && status != "Stopped" && status != "Failed" {
		return &domain.Error{Code: "APP_NOT_DELETABLE", Message: "Only drafts, stopped apps, and cleaned-up failed apps can be deleted."}
	}
	if status == "Stopped" || status == "Failed" {
		var busy bool
		if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM resource_reservations WHERE app_id=$1)
            OR EXISTS(SELECT 1 FROM deployment_tasks WHERE app_id=$1 AND state IN ('Queued','Leased'))`, appID).Scan(&busy); err != nil {
			return storeError(err)
		}
		if busy {
			return &domain.Error{Code: "APP_CLEANUP_PENDING", Message: "Wait until workload cleanup and resource release finish."}
		}
		if status == "Failed" {
			var cleaned bool
			if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM deployment_tasks
                WHERE app_id=$1 AND desired_state IN ('Stopped','Failed') AND state='Succeeded'
                  AND created_at >= COALESCE((SELECT max(created_at) FROM deployment_tasks
                      WHERE app_id=$1 AND desired_state='Running'), '-infinity'::timestamptz))`, appID).Scan(&cleaned); err != nil {
				return storeError(err)
			}
			if !cleaned {
				return &domain.Error{Code: "APP_CLEANUP_PENDING", Message: "Wait until workload cleanup is verified."}
			}
		}
	}
	opID, err := server.NewID()
	if err != nil {
		return err
	}
	detail, _ := json.Marshal(map[string]string{"appId": appID})
	kind := "DELETE_DRAFT"
	if status == "Stopped" || status == "Failed" {
		if status == "Failed" {
			kind = "DELETE_FAILED_APP"
		} else {
			kind = "DELETE_STOPPED_APP"
		}
		// Historical tasks and revisions retain foreign keys to this app. Once
		// cleanup has completed, remove them in dependency order inside the same
		// transaction; operations remain as the account audit trail.
		if _, err = tx.Exec(ctx, `DELETE FROM outbox WHERE task_id IN
            (SELECT id FROM deployment_tasks WHERE app_id=$1)`, appID); err != nil {
			return storeError(err)
		}
		if _, err = tx.Exec(ctx, `DELETE FROM deployment_tasks WHERE app_id=$1`, appID); err != nil {
			return storeError(err)
		}
		if _, err = tx.Exec(ctx, `DELETE FROM app_revisions WHERE app_id=$1`, appID); err != nil {
			return storeError(err)
		}
	}
	_, err = tx.Exec(ctx, `INSERT INTO operations(id,account_id,app_id,actor_subject,kind,state,detail)
        VALUES($1,$2,$3,$4,$5,'Succeeded',$6::jsonb)`, opID, id, appID, subject, kind, detail)
	if err != nil {
		return storeError(err)
	}
	_, err = tx.Exec(ctx, `DELETE FROM apps WHERE account_id=$1 AND id=$2`, id, appID)
	if err != nil {
		return storeError(err)
	}
	return storeError(tx.Commit(ctx))
}

func (s *Store) ListOperations(ctx context.Context, id string) ([]server.Operation, error) {
	rows, err := s.Pool.Query(ctx, `SELECT id::text,COALESCE(app_id::text,''),kind,state,detail,created_at
        FROM operations WHERE account_id=$1 ORDER BY created_at DESC,id DESC LIMIT 50`, id)
	if err != nil {
		return nil, storeError(err)
	}
	defer rows.Close()
	items := make([]server.Operation, 0)
	for rows.Next() {
		var op server.Operation
		if err = rows.Scan(&op.ID, &op.AppID, &op.Kind, &op.State, &op.Detail, &op.CreatedAt); err != nil {
			return nil, storeError(err)
		}
		items = append(items, op)
	}
	if err = rows.Err(); err != nil {
		return nil, storeError(err)
	}
	return items, nil
}

func (s *Store) RecordSourceValidation(ctx context.Context, id, subject string, size int64, state, code string) error {
	opID, err := server.NewID()
	if err != nil {
		return err
	}
	detail, _ := json.Marshal(map[string]any{"sizeBytes": size, "errorCode": code})
	_, err = s.Pool.Exec(ctx, `INSERT INTO operations(id,account_id,actor_subject,kind,state,detail)
        VALUES($1,$2,$3,'VALIDATE_ZIP',$4,$5::jsonb)`, opID, id, subject, state, detail)
	return storeError(err)
}

func storeError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return server.ErrNotFound
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		log.Printf("database statement failed: SQLSTATE %s", pgErr.Code)
	} else {
		log.Printf("database operation failed: %T", err)
	}
	return server.ErrUnavailable
}

func createError(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		if pgErr.ConstraintName == "apps_account_id_name_key" {
			return &domain.Error{Code: "APP_NAME_TAKEN", Field: "name", Message: "An app with this name already exists."}
		}
		if pgErr.ConstraintName == "apps_account_id_idempotency_key_key" {
			return &domain.Error{Code: "IDEMPOTENCY_KEY_REUSED", Message: "This request key was already used."}
		}
		if pgErr.ConstraintName == "apps_volume_exclusive_idx" {
			return &domain.Error{Code: "VOLUME_ALREADY_BOUND", Field: "volumeId", Message: "This volume is already bound to an app."}
		}
	}
	return storeError(err)
}
