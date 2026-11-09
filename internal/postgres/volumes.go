package postgres

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"polygate/internal/domain"
	"polygate/internal/server"
)

const volumeColumns = `id::text,name,size_gib,state,created_at`

func scanVolume(row rowScanner) (server.Volume, error) {
	var volume server.Volume
	err := row.Scan(&volume.ID, &volume.Spec.Name, &volume.Spec.SizeGiB,
		&volume.State, &volume.CreatedAt)
	return volume, err
}

func createVolumeError(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		switch pgErr.ConstraintName {
		case "student_volumes_account_id_name_key":
			return &domain.Error{Code: "VOLUME_NAME_TAKEN", Field: "name", Message: "A volume with this name already exists."}
		case "student_volumes_account_id_idempotency_key_key":
			return &domain.Error{Code: "IDEMPOTENCY_KEY_REUSED", Message: "This request key was already used."}
		}
	}
	return storeError(err)
}

func (s *Store) ListVolumes(ctx context.Context, accountID string) ([]server.Volume, error) {
	rows, err := s.Pool.Query(ctx, `SELECT `+volumeColumns+` FROM student_volumes
		WHERE account_id=$1 ORDER BY created_at,id`, accountID)
	if err != nil {
		return nil, storeError(err)
	}
	defer rows.Close()
	items := make([]server.Volume, 0)
	for rows.Next() {
		item, err := scanVolume(rows)
		if err != nil {
			return nil, storeError(err)
		}
		items = append(items, item)
	}
	return items, storeError(rows.Err())
}

func (s *Store) GetVolume(ctx context.Context, accountID, volumeID string) (server.Volume, error) {
	item, err := scanVolume(s.Pool.QueryRow(ctx, `SELECT `+volumeColumns+`
		FROM student_volumes WHERE account_id=$1 AND id=$2`, accountID, volumeID))
	return item, storeError(err)
}

func (s *Store) CreateVolume(ctx context.Context, accountID, subject, key string,
	spec domain.VolumeSpec) (server.Volume, bool, error) {
	var empty server.Volume
	if err := domain.ValidateVolumeSpec(spec); err != nil {
		return empty, false, err
	}
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return empty, false, storeError(err)
	}
	defer tx.Rollback(ctx)
	var enabled, volumeEnabled bool
	err = tx.QueryRow(ctx, `SELECT enabled,volume_enabled FROM accounts WHERE id=$1 FOR UPDATE`, accountID).Scan(&enabled, &volumeEnabled)
	if err != nil {
		return empty, false, storeError(err)
	}
	if !enabled {
		return empty, false, server.ErrDisabled
	}
	if !volumeEnabled {
		return empty, false, server.ErrVolumeDisabled
	}
	previous, err := scanVolume(tx.QueryRow(ctx, `SELECT `+volumeColumns+`
		FROM student_volumes WHERE account_id=$1 AND idempotency_key=$2`, accountID, key))
	if err == nil {
		if previous.Spec != spec {
			return empty, false, &domain.Error{Code: "IDEMPOTENCY_KEY_REUSED", Message: "This request key was already used with different settings."}
		}
		return previous, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return empty, false, storeError(err)
	}
	var count, used int
	err = tx.QueryRow(ctx, `SELECT COUNT(*),COALESCE(SUM(size_gib),0)
		FROM student_volumes WHERE account_id=$1`, accountID).Scan(&count, &used)
	if err != nil {
		return empty, false, storeError(err)
	}
	if count >= domain.StudentVolumeCountLimit || used+spec.SizeGiB > domain.StudentVolumeGiBLimit {
		return empty, false, &domain.Error{Code: "VOLUME_QUOTA_EXCEEDED", Message: "Volumes exceed the account limit of two claims and 10 GiB."}
	}
	id, err := server.NewID()
	if err != nil {
		return empty, false, err
	}
	opID, err := server.NewID()
	if err != nil {
		return empty, false, err
	}
	item, err := scanVolume(tx.QueryRow(ctx, `INSERT INTO student_volumes
		(id,account_id,name,size_gib,idempotency_key) VALUES($1,$2,$3,$4,$5)
		RETURNING `+volumeColumns, id, accountID, spec.Name, spec.SizeGiB, key))
	if err != nil {
		return empty, false, createVolumeError(err)
	}
	detail, _ := json.Marshal(map[string]any{"volumeId": id, "sizeGiB": spec.SizeGiB})
	_, err = tx.Exec(ctx, `INSERT INTO operations
		(id,account_id,actor_subject,kind,state,detail)
		VALUES($1,$2,$3,'CREATE_VOLUME_RESERVATION','Succeeded',$4::jsonb)`,
		opID, accountID, subject, detail)
	if err != nil {
		return empty, false, storeError(err)
	}
	if err := tx.Commit(ctx); err != nil {
		return empty, false, storeError(err)
	}
	return item, false, nil
}

func (s *Store) ExpandVolume(ctx context.Context, accountID, subject, volumeID string, sizeGiB int) (server.Volume, error) {
	var empty server.Volume
	if sizeGiB < 1 || sizeGiB > domain.StudentVolumeGiBLimit {
		return empty, &domain.Error{Code: "INVALID_VOLUME_SIZE", Field: "sizeGiB", Message: "Volume size must be between 1 and 10 GiB."}
	}
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return empty, storeError(err)
	}
	defer tx.Rollback(ctx)
	var enabled, volumeEnabled bool
	if err := tx.QueryRow(ctx, `SELECT enabled,volume_enabled FROM accounts WHERE id=$1 FOR UPDATE`, accountID).Scan(&enabled, &volumeEnabled); err != nil {
		return empty, storeError(err)
	}
	if !enabled {
		return empty, server.ErrDisabled
	}
	if !volumeEnabled {
		return empty, server.ErrVolumeDisabled
	}
	item, err := scanVolume(tx.QueryRow(ctx, `SELECT `+volumeColumns+` FROM student_volumes
		WHERE account_id=$1 AND id=$2 FOR UPDATE`, accountID, volumeID))
	if err != nil {
		return empty, storeError(err)
	}
	if sizeGiB < item.Spec.SizeGiB {
		return empty, &domain.Error{Code: "INVALID_VOLUME_SIZE", Field: "sizeGiB", Message: "Volumes cannot shrink."}
	}
	if sizeGiB == item.Spec.SizeGiB {
		return item, nil
	}
	if item.State != "Ready" {
		return empty, &domain.Error{Code: "VOLUME_NOT_READY", Message: "Wait until this volume is ready before expanding it."}
	}
	var used int
	if err := tx.QueryRow(ctx, `SELECT COALESCE(SUM(size_gib),0) FROM student_volumes
		WHERE account_id=$1`, accountID).Scan(&used); err != nil {
		return empty, storeError(err)
	}
	if used+sizeGiB-item.Spec.SizeGiB > domain.StudentVolumeGiBLimit {
		return empty, &domain.Error{Code: "VOLUME_QUOTA_EXCEEDED", Message: "Volumes exceed the account limit of 10 GiB."}
	}
	opID, err := server.NewID()
	if err != nil {
		return empty, err
	}
	item, err = scanVolume(tx.QueryRow(ctx, `UPDATE student_volumes SET size_gib=$3,state='Expanding',
		lease_until=NULL,locked_by=NULL,updated_at=now()
		WHERE account_id=$1 AND id=$2 RETURNING `+volumeColumns, accountID, volumeID, sizeGiB))
	if err != nil {
		return empty, storeError(err)
	}
	detail, _ := json.Marshal(map[string]any{"volumeId": volumeID, "sizeGiB": sizeGiB})
	_, err = tx.Exec(ctx, `INSERT INTO operations
		(id,account_id,actor_subject,kind,state,detail)
		VALUES($1,$2,$3,'EXPAND_VOLUME','Pending',$4::jsonb)`, opID, accountID, subject, detail)
	if err != nil {
		return empty, storeError(err)
	}
	if err := tx.Commit(ctx); err != nil {
		return empty, storeError(err)
	}
	return item, nil
}

func (s *Store) DeleteVolume(ctx context.Context, accountID, subject, volumeID, confirmName string) (server.Volume, error) {
	var empty server.Volume
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return empty, storeError(err)
	}
	defer tx.Rollback(ctx)
	var enabled, volumeEnabled bool
	if err := tx.QueryRow(ctx, `SELECT enabled,volume_enabled FROM accounts WHERE id=$1 FOR UPDATE`, accountID).Scan(&enabled, &volumeEnabled); err != nil {
		return empty, storeError(err)
	}
	if !enabled {
		return empty, server.ErrDisabled
	}
	if !volumeEnabled {
		return empty, server.ErrVolumeDisabled
	}
	item, err := scanVolume(tx.QueryRow(ctx, `SELECT `+volumeColumns+` FROM student_volumes
		WHERE account_id=$1 AND id=$2 FOR UPDATE`, accountID, volumeID))
	if err != nil {
		return empty, storeError(err)
	}
	if confirmName != item.Spec.Name {
		return empty, &domain.Error{Code: "VOLUME_CONFIRMATION_MISMATCH", Field: "confirmName", Message: "Type the volume name to confirm permanent data deletion."}
	}
	if item.State == "Deleting" {
		return item, nil
	}
	if item.State != "Ready" {
		return empty, &domain.Error{Code: "VOLUME_NOT_READY", Message: "Only a ready, unbound volume can be deleted."}
	}
	var bound bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM apps WHERE account_id=$1 AND volume_id=$2)`, accountID, volumeID).Scan(&bound); err != nil {
		return empty, storeError(err)
	}
	if bound {
		return empty, &domain.Error{Code: "VOLUME_ALREADY_BOUND", Message: "Delete the app before deleting its volume."}
	}
	opID, err := server.NewID()
	if err != nil {
		return empty, err
	}
	item, err = scanVolume(tx.QueryRow(ctx, `UPDATE student_volumes SET state='Deleting',lease_until=NULL,
		locked_by=NULL,updated_at=now() WHERE account_id=$1 AND id=$2 RETURNING `+volumeColumns, accountID, volumeID))
	if err != nil {
		return empty, storeError(err)
	}
	detail, _ := json.Marshal(map[string]any{"volumeId": volumeID, "volumeName": item.Spec.Name})
	_, err = tx.Exec(ctx, `INSERT INTO operations(id,account_id,actor_subject,kind,state,detail)
		VALUES($1,$2,$3,'DELETE_VOLUME','Pending',$4::jsonb)`, opID, accountID, subject, detail)
	if err != nil {
		return empty, storeError(err)
	}
	if err := tx.Commit(ctx); err != nil {
		return empty, storeError(err)
	}
	return item, nil
}
