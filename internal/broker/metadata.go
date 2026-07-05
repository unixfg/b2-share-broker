package broker

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

type StoredObject struct {
	SHA256        string    `json:"sha256"`
	ObjectKey     string    `json:"objectKey"`
	Size          int64     `json:"size"`
	ContentType   string    `json:"contentType"`
	Extension     string    `json:"extension"`
	FirstFilename string    `json:"firstFilename"`
	Uploader      string    `json:"uploader"`
	Status        string    `json:"status"`
	CreatedAt     time.Time `json:"createdAt"`
	DeletedAt     time.Time `json:"deletedAt,omitempty"`
}

type ShareAlias struct {
	Slug             string    `json:"slug"`
	ObjectSHA256     string    `json:"sha256,omitempty"`
	ObjectKey        string    `json:"objectKey,omitempty"`
	Owner            string    `json:"owner"`
	DisplayFilename  string    `json:"displayFilename"`
	Visibility       string    `json:"visibility"`
	Status           string    `json:"status"`
	Error            string    `json:"error,omitempty"`
	CreatedAt        time.Time `json:"createdAt"`
	UpdatedAt        time.Time `json:"updatedAt"`
	RedirectCount    int64     `json:"redirectCount"`
	LastRedirectedAt time.Time `json:"lastRedirectedAt,omitempty"`
	Size             int64     `json:"size"`
	ContentType      string    `json:"contentType,omitempty"`
	B2URL            string    `json:"b2Url,omitempty"`
	PublicURL        string    `json:"publicUrl"`
}

type ProcessingJob struct {
	ID              string    `json:"jobId"`
	Owner           string    `json:"owner,omitempty"`
	AliasSlug       string    `json:"aliasSlug"`
	SourceSHA256    string    `json:"sourceSha256,omitempty"`
	SourceObjectKey string    `json:"sourceObjectKey,omitempty"`
	StagingPath     string    `json:"stagingPath,omitempty"`
	TargetSHA256    string    `json:"targetSha256,omitempty"`
	TargetObjectKey string    `json:"targetObjectKey,omitempty"`
	Profile         string    `json:"profile"`
	Status          string    `json:"status"`
	Error           string    `json:"error,omitempty"`
	CreatedAt       time.Time `json:"createdAt"`
	UpdatedAt       time.Time `json:"updatedAt"`
	StartedAt       time.Time `json:"startedAt,omitempty"`
	CompletedAt     time.Time `json:"completedAt,omitempty"`
	DisplayFilename string    `json:"displayFilename,omitempty"`
	SourceSize      int64     `json:"sourceSize,omitempty"`
	SourceType      string    `json:"sourceContentType,omitempty"`
}

type ObjectDerivative struct {
	SourceSHA256 string    `json:"sourceSha256"`
	TargetSHA256 string    `json:"targetSha256"`
	Profile      string    `json:"profile"`
	JobID        string    `json:"jobId"`
	CreatedAt    time.Time `json:"createdAt"`
}

type DeletedShare struct {
	Alias        ShareAlias
	ObjectKey    string
	StagingPaths []string
}

type MetadataStore interface {
	GetObject(ctx context.Context, sha256 string) (StoredObject, bool, error)
	GetDerivedObject(ctx context.Context, sourceSHA256, profile string) (StoredObject, bool, error)
	MarkObjectUnavailable(ctx context.Context, sha256, status string) error
	UpsertAlias(ctx context.Context, alias ShareAlias) error
	GetAlias(ctx context.Context, slug string) (ShareAlias, bool, error)
	RecordAliasRedirect(ctx context.Context, slug string) error
	ListAliases(ctx context.Context, owner, query string, limit int) ([]ShareAlias, error)
	DeleteAlias(ctx context.Context, slug, owner string) (DeletedShare, bool, error)
	CreateIngestJob(ctx context.Context, job ProcessingJob, alias ShareAlias) (ProcessingJob, error)
	GetProcessingJob(ctx context.Context, id, owner string) (ProcessingJob, bool, error)
	ClaimNextProcessingJob(ctx context.Context, worker string) (ProcessingJob, bool, error)
	CompleteProcessingJob(ctx context.Context, id string, object StoredObject, alias ShareAlias) error
	FailProcessingJob(ctx context.Context, id, message string) error
}

var ErrAliasConflict = errors.New("share alias is already owned by another user")

type PostgresMetadataStore struct {
	db *sql.DB
}

func NewPostgresMetadataStore(ctx context.Context, databaseURL string) (*PostgresMetadataStore, error) {
	db, err := sql.Open("pgx", databaseURL)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(5)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(30 * time.Minute)
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, err
	}
	store := &PostgresMetadataStore{db: db}
	if err := store.runMigrations(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return store, nil
}

func (s *PostgresMetadataStore) Close() error {
	return s.db.Close()
}

func (s *PostgresMetadataStore) runMigrations(ctx context.Context) error {
	const advisoryKey = int64(255364328384130850)
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `SELECT pg_advisory_lock($1)`, advisoryKey); err != nil {
		return err
	}
	defer conn.ExecContext(context.Background(), `SELECT pg_advisory_unlock($1)`, advisoryKey)

	for _, statement := range []string{
		`CREATE TABLE IF NOT EXISTS objects (
			sha256 text PRIMARY KEY CHECK (sha256 ~ '^[0-9a-f]{64}$'),
			object_key text NOT NULL UNIQUE,
			size_bytes bigint NOT NULL CHECK (size_bytes > 0),
			content_type text NOT NULL,
			extension text NOT NULL,
			first_filename text NOT NULL,
			uploader_subject text NOT NULL,
			status text NOT NULL DEFAULT 'ready',
			created_at timestamptz NOT NULL DEFAULT now(),
			deleted_at timestamptz
		)`,
		`ALTER TABLE objects ADD COLUMN IF NOT EXISTS status text NOT NULL DEFAULT 'ready'`,
		`ALTER TABLE objects ADD COLUMN IF NOT EXISTS deleted_at timestamptz`,
		`CREATE TABLE IF NOT EXISTS aliases (
			slug text PRIMARY KEY,
			object_sha256 text REFERENCES objects(sha256),
			owner_subject text NOT NULL,
			display_filename text NOT NULL,
			visibility text NOT NULL DEFAULT 'public',
			status text NOT NULL DEFAULT 'pending',
			error text NOT NULL DEFAULT '',
			created_at timestamptz NOT NULL DEFAULT now(),
			updated_at timestamptz NOT NULL DEFAULT now(),
			redirect_count bigint NOT NULL DEFAULT 0,
			last_redirected_at timestamptz
		)`,
		`ALTER TABLE aliases ALTER COLUMN object_sha256 DROP NOT NULL`,
		`ALTER TABLE aliases ADD COLUMN IF NOT EXISTS status text NOT NULL DEFAULT 'ready'`,
		`ALTER TABLE aliases ADD COLUMN IF NOT EXISTS error text NOT NULL DEFAULT ''`,
		`CREATE INDEX IF NOT EXISTS aliases_owner_updated_idx ON aliases(owner_subject, updated_at DESC)`,
		`CREATE TABLE IF NOT EXISTS alias_history (
			id bigserial PRIMARY KEY,
			slug text NOT NULL,
			previous_object_sha256 text NOT NULL,
			new_object_sha256 text NOT NULL,
			changed_by_subject text NOT NULL,
			changed_at timestamptz NOT NULL DEFAULT now()
		)`,
		`CREATE INDEX IF NOT EXISTS alias_history_slug_idx ON alias_history(slug, changed_at DESC)`,
		`CREATE TABLE IF NOT EXISTS processing_jobs (
			id text PRIMARY KEY,
			owner_subject text NOT NULL,
			alias_slug text NOT NULL,
			source_object_sha256 text REFERENCES objects(sha256),
			target_object_sha256 text REFERENCES objects(sha256),
			staging_path text NOT NULL DEFAULT '',
			source_filename text NOT NULL DEFAULT '',
			source_content_type text NOT NULL DEFAULT '',
			source_size_bytes bigint NOT NULL DEFAULT 0,
			profile text NOT NULL,
			status text NOT NULL CHECK (status IN ('queued', 'running', 'completed', 'failed', 'canceled')),
			error text NOT NULL DEFAULT '',
			worker text NOT NULL DEFAULT '',
			created_at timestamptz NOT NULL DEFAULT now(),
			updated_at timestamptz NOT NULL DEFAULT now(),
			started_at timestamptz,
			completed_at timestamptz
		)`,
		`ALTER TABLE processing_jobs ALTER COLUMN source_object_sha256 DROP NOT NULL`,
		`ALTER TABLE processing_jobs ADD COLUMN IF NOT EXISTS staging_path text NOT NULL DEFAULT ''`,
		`ALTER TABLE processing_jobs ADD COLUMN IF NOT EXISTS source_filename text NOT NULL DEFAULT ''`,
		`ALTER TABLE processing_jobs ADD COLUMN IF NOT EXISTS source_content_type text NOT NULL DEFAULT ''`,
		`ALTER TABLE processing_jobs ADD COLUMN IF NOT EXISTS source_size_bytes bigint NOT NULL DEFAULT 0`,
		`ALTER TABLE processing_jobs DROP CONSTRAINT IF EXISTS processing_jobs_status_check`,
		`ALTER TABLE processing_jobs ADD CONSTRAINT processing_jobs_status_check CHECK (status IN ('queued', 'running', 'completed', 'failed', 'canceled'))`,
		`CREATE INDEX IF NOT EXISTS processing_jobs_owner_created_idx ON processing_jobs(owner_subject, created_at DESC)`,
		`CREATE INDEX IF NOT EXISTS processing_jobs_queue_idx ON processing_jobs(status, created_at)`,
		`CREATE TABLE IF NOT EXISTS object_derivatives (
			source_object_sha256 text NOT NULL REFERENCES objects(sha256),
			target_object_sha256 text NOT NULL REFERENCES objects(sha256),
			profile text NOT NULL,
			processing_job_id text NOT NULL REFERENCES processing_jobs(id),
			created_at timestamptz NOT NULL DEFAULT now(),
			PRIMARY KEY (source_object_sha256, profile)
		)`,
	} {
		if _, err := conn.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	return nil
}

func (s *PostgresMetadataStore) GetObject(ctx context.Context, sha256 string) (StoredObject, bool, error) {
	var object StoredObject
	var deleted sql.NullTime
	err := s.db.QueryRowContext(ctx, `SELECT sha256, object_key, size_bytes, content_type, extension, first_filename, uploader_subject, status, created_at, deleted_at FROM objects WHERE sha256 = $1`, sha256).Scan(
		&object.SHA256,
		&object.ObjectKey,
		&object.Size,
		&object.ContentType,
		&object.Extension,
		&object.FirstFilename,
		&object.Uploader,
		&object.Status,
		&object.CreatedAt,
		&deleted,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return StoredObject{}, false, nil
	}
	if err != nil {
		return StoredObject{}, false, err
	}
	if deleted.Valid {
		object.DeletedAt = deleted.Time
	}
	return object, true, nil
}

func (s *PostgresMetadataStore) GetDerivedObject(ctx context.Context, sourceSHA256, profile string) (StoredObject, bool, error) {
	var object StoredObject
	var deleted sql.NullTime
	err := s.db.QueryRowContext(ctx, `SELECT
			o.sha256, o.object_key, o.size_bytes, o.content_type, o.extension, o.first_filename, o.uploader_subject, o.status, o.created_at, o.deleted_at
		FROM object_derivatives d
		JOIN objects o ON o.sha256 = d.target_object_sha256
		WHERE d.source_object_sha256 = $1
			AND d.profile = $2
			AND o.status = 'ready'`, sourceSHA256, profile).Scan(
		&object.SHA256,
		&object.ObjectKey,
		&object.Size,
		&object.ContentType,
		&object.Extension,
		&object.FirstFilename,
		&object.Uploader,
		&object.Status,
		&object.CreatedAt,
		&deleted,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return StoredObject{}, false, nil
	}
	if err != nil {
		return StoredObject{}, false, err
	}
	if deleted.Valid {
		object.DeletedAt = deleted.Time
	}
	return object, true, nil
}

func (s *PostgresMetadataStore) MarkObjectUnavailable(ctx context.Context, sha256, status string) error {
	if status == "" {
		status = "missing"
	}
	_, err := s.db.ExecContext(ctx, `UPDATE objects SET status = $2, deleted_at = now() WHERE sha256 = $1`, sha256, status)
	return err
}

func (s *PostgresMetadataStore) UpsertAlias(ctx context.Context, alias ShareAlias) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := upsertAliasTx(ctx, tx, alias); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *PostgresMetadataStore) GetAlias(ctx context.Context, slug string) (ShareAlias, bool, error) {
	var alias ShareAlias
	err := scanAlias(s.db.QueryRowContext(ctx, aliasSelect()+` WHERE a.slug = $1`, slug), &alias)
	if errors.Is(err, sql.ErrNoRows) {
		return ShareAlias{}, false, nil
	}
	if err != nil {
		return ShareAlias{}, false, err
	}
	return alias, true, nil
}

func (s *PostgresMetadataStore) RecordAliasRedirect(ctx context.Context, slug string) error {
	result, err := s.db.ExecContext(ctx, `UPDATE aliases SET redirect_count = redirect_count + 1, last_redirected_at = now() WHERE slug = $1`, slug)
	if err != nil {
		return err
	}
	if count, err := result.RowsAffected(); err == nil && count == 0 {
		return fmt.Errorf("alias %q not found", slug)
	}
	return nil
}

func (s *PostgresMetadataStore) ListAliases(ctx context.Context, owner, query string, limit int) ([]ShareAlias, error) {
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	query = strings.ToLower(strings.TrimSpace(query))
	pattern := "%" + query + "%"
	rows, err := s.db.QueryContext(ctx, aliasSelect()+`
		WHERE a.owner_subject = $1
			AND a.visibility <> 'deleted'
			AND (
				$2 = ''
				OR lower(a.slug) LIKE $3
				OR lower(a.display_filename) LIKE $3
				OR lower(a.status) LIKE $3
				OR lower(COALESCE(o.content_type, '')) LIKE $3
			)
		ORDER BY a.updated_at DESC
		LIMIT $4`, owner, query, pattern, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var aliases []ShareAlias
	for rows.Next() {
		var alias ShareAlias
		if err := scanAlias(rows, &alias); err != nil {
			return nil, err
		}
		aliases = append(aliases, alias)
	}
	return aliases, rows.Err()
}

func (s *PostgresMetadataStore) DeleteAlias(ctx context.Context, slug, owner string) (DeletedShare, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return DeletedShare{}, false, err
	}
	defer tx.Rollback()

	var alias ShareAlias
	err = scanAlias(tx.QueryRowContext(ctx, aliasSelect()+`
		WHERE a.slug = $1
			AND a.owner_subject = $2
			AND a.visibility <> 'deleted'
		FOR UPDATE OF a`, slug, owner), &alias)
	if errors.Is(err, sql.ErrNoRows) {
		return DeletedShare{}, false, nil
	}
	if err != nil {
		return DeletedShare{}, false, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE aliases
		SET visibility = 'deleted', updated_at = now()
		WHERE slug = $1 AND owner_subject = $2`, slug, owner); err != nil {
		return DeletedShare{}, false, err
	}
	rows, err := tx.QueryContext(ctx, `UPDATE processing_jobs
		SET status = 'canceled', updated_at = now(), completed_at = now(), error = 'share deleted'
		WHERE alias_slug = $1
			AND owner_subject = $2
			AND status IN ('queued', 'running')
		RETURNING staging_path`, slug, owner)
	if err != nil {
		return DeletedShare{}, false, err
	}
	var stagingPaths []string
	for rows.Next() {
		var path string
		if err := rows.Scan(&path); err != nil {
			rows.Close()
			return DeletedShare{}, false, err
		}
		if path != "" {
			stagingPaths = append(stagingPaths, path)
		}
	}
	if err := rows.Close(); err != nil {
		return DeletedShare{}, false, err
	}

	deleted := DeletedShare{Alias: alias, StagingPaths: stagingPaths}
	if alias.ObjectSHA256 != "" && alias.ObjectKey != "" {
		var references int
		if err := tx.QueryRowContext(ctx, `SELECT count(*)
			FROM aliases
			WHERE object_sha256 = $1
				AND visibility <> 'deleted'`, alias.ObjectSHA256).Scan(&references); err != nil {
			return DeletedShare{}, false, err
		}
		if references == 0 {
			if _, err := tx.ExecContext(ctx, `UPDATE objects SET status = 'deleted', deleted_at = now() WHERE sha256 = $1`, alias.ObjectSHA256); err != nil {
				return DeletedShare{}, false, err
			}
			deleted.ObjectKey = alias.ObjectKey
		}
	}
	return deleted, true, tx.Commit()
}

func (s *PostgresMetadataStore) CreateIngestJob(ctx context.Context, job ProcessingJob, alias ShareAlias) (ProcessingJob, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ProcessingJob{}, err
	}
	defer tx.Rollback()
	alias.Status = AliasStatusPending
	alias.Visibility = "public"
	if err := upsertAliasTx(ctx, tx, alias); err != nil {
		return ProcessingJob{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO processing_jobs
		(id, owner_subject, alias_slug, staging_path, source_filename, source_content_type, source_size_bytes, profile, status)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 'queued')`,
		job.ID,
		job.Owner,
		job.AliasSlug,
		job.StagingPath,
		job.DisplayFilename,
		job.SourceType,
		job.SourceSize,
		job.Profile,
	); err != nil {
		return ProcessingJob{}, err
	}
	created, found, err := getProcessingJobTx(ctx, tx, job.ID, job.Owner)
	if err != nil {
		return ProcessingJob{}, err
	}
	if !found {
		return ProcessingJob{}, fmt.Errorf("processing job %q was not recorded", job.ID)
	}
	return created, tx.Commit()
}

func (s *PostgresMetadataStore) GetProcessingJob(ctx context.Context, id, owner string) (ProcessingJob, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ProcessingJob{}, false, err
	}
	defer tx.Rollback()
	job, found, err := getProcessingJobTx(ctx, tx, id, owner)
	if err != nil {
		return ProcessingJob{}, false, err
	}
	return job, found, tx.Commit()
}

func (s *PostgresMetadataStore) ClaimNextProcessingJob(ctx context.Context, worker string) (ProcessingJob, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ProcessingJob{}, false, err
	}
	defer tx.Rollback()

	var id string
	err = tx.QueryRowContext(ctx, `SELECT id
		FROM processing_jobs
		WHERE status = 'queued'
		ORDER BY created_at
		FOR UPDATE SKIP LOCKED
		LIMIT 1`).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return ProcessingJob{}, false, nil
	}
	if err != nil {
		return ProcessingJob{}, false, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE processing_jobs
		SET status = 'running', worker = $2, started_at = now(), updated_at = now()
		WHERE id = $1`, id, worker); err != nil {
		return ProcessingJob{}, false, err
	}
	job, found, err := getProcessingJobTx(ctx, tx, id, "")
	if err != nil {
		return ProcessingJob{}, false, err
	}
	if !found {
		return ProcessingJob{}, false, fmt.Errorf("processing job %q disappeared", id)
	}
	return job, true, tx.Commit()
}

func (s *PostgresMetadataStore) CompleteProcessingJob(ctx context.Context, id string, object StoredObject, alias ShareAlias) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var profile string
	var sourceSHA sql.NullString
	err = tx.QueryRowContext(ctx, `SELECT source_object_sha256, profile
		FROM processing_jobs
		WHERE id = $1
			AND status = 'running'
		FOR UPDATE`, id).Scan(&sourceSHA, &profile)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("running processing job %q not found", id)
	}
	if err != nil {
		return err
	}
	if err := upsertObjectTx(ctx, tx, object); err != nil {
		return err
	}
	alias.ObjectSHA256 = object.SHA256
	alias.Status = AliasStatusReady
	alias.Error = ""
	if err := upsertAliasTx(ctx, tx, alias); err != nil {
		return err
	}
	if sourceSHA.Valid && sourceSHA.String != "" {
		if _, err := tx.ExecContext(ctx, `INSERT INTO object_derivatives (source_object_sha256, target_object_sha256, profile, processing_job_id)
			VALUES ($1, $2, $3, $4)
			ON CONFLICT (source_object_sha256, profile) DO UPDATE SET
				target_object_sha256 = EXCLUDED.target_object_sha256,
				processing_job_id = EXCLUDED.processing_job_id,
				created_at = now()`,
			sourceSHA.String,
			object.SHA256,
			profile,
			id,
		); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE processing_jobs
		SET status = 'completed', target_object_sha256 = $2, completed_at = now(), updated_at = now(), error = ''
		WHERE id = $1`, id, object.SHA256); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *PostgresMetadataStore) FailProcessingJob(ctx context.Context, id, message string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var slug string
	err = tx.QueryRowContext(ctx, `SELECT alias_slug FROM processing_jobs WHERE id = $1 FOR UPDATE`, id).Scan(&slug)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE processing_jobs
		SET status = 'failed', error = $2, completed_at = now(), updated_at = now()
		WHERE id = $1
			AND status IN ('queued', 'running')`, id, message); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE aliases
		SET status = 'failed', error = $2, updated_at = now()
		WHERE slug = $1
			AND visibility <> 'deleted'`, slug, message); err != nil {
		return err
	}
	return tx.Commit()
}

func upsertObjectTx(ctx context.Context, tx *sql.Tx, object StoredObject) error {
	if object.Status == "" {
		object.Status = "ready"
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO objects (sha256, object_key, size_bytes, content_type, extension, first_filename, uploader_subject, status, deleted_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, NULL)
		ON CONFLICT (sha256) DO UPDATE SET
			object_key = EXCLUDED.object_key,
			size_bytes = EXCLUDED.size_bytes,
			content_type = EXCLUDED.content_type,
			extension = EXCLUDED.extension,
			first_filename = EXCLUDED.first_filename,
			status = EXCLUDED.status,
			deleted_at = NULL`,
		object.SHA256,
		object.ObjectKey,
		object.Size,
		object.ContentType,
		object.Extension,
		object.FirstFilename,
		object.Uploader,
		object.Status,
	)
	return err
}

func upsertAliasTx(ctx context.Context, tx *sql.Tx, alias ShareAlias) error {
	var previousObject sql.NullString
	var previousOwner string
	err := tx.QueryRowContext(ctx, `SELECT object_sha256, owner_subject FROM aliases WHERE slug = $1 FOR UPDATE`, alias.Slug).Scan(&previousObject, &previousOwner)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if err == nil && previousOwner != alias.Owner {
		return ErrAliasConflict
	}
	if previousObject.Valid && previousObject.String != "" && alias.ObjectSHA256 != "" && previousObject.String != alias.ObjectSHA256 {
		if _, err := tx.ExecContext(ctx, `INSERT INTO alias_history (slug, previous_object_sha256, new_object_sha256, changed_by_subject) VALUES ($1, $2, $3, $4)`,
			alias.Slug,
			previousObject.String,
			alias.ObjectSHA256,
			alias.Owner,
		); err != nil {
			return err
		}
	}
	if alias.Visibility == "" {
		alias.Visibility = "public"
	}
	if alias.Status == "" {
		if alias.ObjectSHA256 == "" {
			alias.Status = AliasStatusPending
		} else {
			alias.Status = AliasStatusReady
		}
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO aliases (slug, object_sha256, owner_subject, display_filename, visibility, status, error)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (slug) DO UPDATE SET
			object_sha256 = EXCLUDED.object_sha256,
			owner_subject = EXCLUDED.owner_subject,
			display_filename = EXCLUDED.display_filename,
			visibility = EXCLUDED.visibility,
			status = EXCLUDED.status,
			error = EXCLUDED.error,
			updated_at = now()`,
		alias.Slug,
		nullString(alias.ObjectSHA256),
		alias.Owner,
		alias.DisplayFilename,
		alias.Visibility,
		alias.Status,
		alias.Error,
	)
	return err
}

func getProcessingJobTx(ctx context.Context, tx *sql.Tx, id, owner string) (ProcessingJob, bool, error) {
	query := `SELECT
			j.id, j.owner_subject, j.alias_slug, COALESCE(j.source_object_sha256, ''), COALESCE(o.object_key, ''),
			j.staging_path, COALESCE(j.target_object_sha256, ''), COALESCE(t.object_key, ''),
			j.profile, j.status, j.error,
			j.created_at, j.updated_at, j.started_at, j.completed_at,
			COALESCE(NULLIF(j.source_filename, ''), a.display_filename),
			j.source_size_bytes,
			j.source_content_type
		FROM processing_jobs j
		JOIN aliases a ON a.slug = j.alias_slug
		LEFT JOIN objects o ON o.sha256 = j.source_object_sha256
		LEFT JOIN objects t ON t.sha256 = j.target_object_sha256
		WHERE j.id = $1`
	args := []any{id}
	if owner != "" {
		query += ` AND j.owner_subject = $2`
		args = append(args, owner)
	}

	var job ProcessingJob
	var started sql.NullTime
	var completed sql.NullTime
	err := tx.QueryRowContext(ctx, query, args...).Scan(
		&job.ID,
		&job.Owner,
		&job.AliasSlug,
		&job.SourceSHA256,
		&job.SourceObjectKey,
		&job.StagingPath,
		&job.TargetSHA256,
		&job.TargetObjectKey,
		&job.Profile,
		&job.Status,
		&job.Error,
		&job.CreatedAt,
		&job.UpdatedAt,
		&started,
		&completed,
		&job.DisplayFilename,
		&job.SourceSize,
		&job.SourceType,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return ProcessingJob{}, false, nil
	}
	if err != nil {
		return ProcessingJob{}, false, err
	}
	if started.Valid {
		job.StartedAt = started.Time
	}
	if completed.Valid {
		job.CompletedAt = completed.Time
	}
	return job, true, nil
}

func aliasSelect() string {
	return `SELECT
			a.slug, COALESCE(a.object_sha256, ''), COALESCE(o.object_key, ''), a.owner_subject, a.display_filename, a.visibility,
			a.status, a.error, a.created_at, a.updated_at, a.redirect_count, a.last_redirected_at,
			COALESCE(o.size_bytes, 0), COALESCE(o.content_type, '')
		FROM aliases a
		LEFT JOIN objects o ON o.sha256 = a.object_sha256`
}

type scanner interface {
	Scan(dest ...any) error
}

func scanAlias(row scanner, alias *ShareAlias) error {
	var last sql.NullTime
	if err := row.Scan(
		&alias.Slug,
		&alias.ObjectSHA256,
		&alias.ObjectKey,
		&alias.Owner,
		&alias.DisplayFilename,
		&alias.Visibility,
		&alias.Status,
		&alias.Error,
		&alias.CreatedAt,
		&alias.UpdatedAt,
		&alias.RedirectCount,
		&last,
		&alias.Size,
		&alias.ContentType,
	); err != nil {
		return err
	}
	if last.Valid {
		alias.LastRedirectedAt = last.Time
	}
	return nil
}

func nullString(value string) sql.NullString {
	if value == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: value, Valid: true}
}
