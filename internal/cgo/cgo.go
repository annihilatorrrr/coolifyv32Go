// Package cgo writes into the coolifygo Postgres database. Mirrors the small
// slice of coolifygo's internal/db statements we actually need — kept as a
// copy (rather than depending on coolifygo/internal/...) because Go's internal
// rule blocks cross-module access.
package cgo

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const encPrefix = "enc:v1:"

// Client wraps a pgxpool to coolifygo's database, plus the data-encryption key.
type Client struct {
	Pool *pgxpool.Pool
	key  []byte
}

// Open connects to coolifygo's Postgres and validates the encryption key shape.
// rawKey is the base64-encoded 32-byte key from coolifygo's
// DATA_ENCRYPTION_KEY env var.
func Open(ctx context.Context, dsn, rawKey string) (*Client, error) {
	keyBytes, err := base64.StdEncoding.DecodeString(rawKey)
	if err != nil {
		return nil, fmt.Errorf("DATA_ENCRYPTION_KEY: invalid base64: %w", err)
	}
	if len(keyBytes) != 32 {
		return nil, fmt.Errorf("DATA_ENCRYPTION_KEY: want 32 raw bytes, got %d", len(keyBytes))
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse dsn: %w", err)
	}
	cfg.MaxConns = 4
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("pgxpool: %w", err)
	}
	if err = pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping coolifygo postgres: %w", err)
	}
	return &Client{Pool: pool, key: keyBytes}, nil
}

// Close releases the connection pool.
func (c *Client) Close() { c.Pool.Close() }

// Encrypt mirrors internal/crypto.Encrypt: AES-256-GCM, "enc:v1:" prefix,
// random nonce. Empty plaintext returns empty output so optional fields stay
// unset rather than carrying a sealed empty string.
func (c *Client) Encrypt(plaintext string) (string, error) {
	if plaintext == "" {
		return "", nil
	}
	if strings.HasPrefix(plaintext, encPrefix) {
		return plaintext, nil
	}
	block, err := aes.NewCipher(c.key)
	if err != nil {
		return "", fmt.Errorf("aes: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("gcm: %w", err)
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err = rand.Read(nonce); err != nil {
		return "", fmt.Errorf("rand: %w", err)
	}
	sealed := gcm.Seal(nonce, nonce, []byte(plaintext), nil)
	return encPrefix + base64.StdEncoding.EncodeToString(sealed), nil
}

// FindLocalServer returns the singleton `type=local` server row. Errors if
// coolifygo hasn't booted on this host yet (no local row exists), since
// EnsureLocalServer is the canonical creator and we must not bypass it.
func (c *Client) FindLocalServer(ctx context.Context) (uuid.UUID, error) {
	var id uuid.UUID
	err := c.Pool.QueryRow(ctx,
		`SELECT id FROM servers WHERE type = 'local' LIMIT 1`).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, errors.New(
			"no type=local server in coolifygo DB — start coolifygo at least once before running migrater")
	}
	if err != nil {
		return uuid.Nil, fmt.Errorf("query local server: %w", err)
	}
	return id, nil
}

// UsedPorts returns every host port already claimed by apps and public
// databases on the given server. Mirrors coolifygo's ports.Allocator.collectUsed
// so the migrater can detect conflicts before inserting.
func (c *Client) UsedPorts(ctx context.Context, serverID uuid.UUID) (map[int]string, error) {
	used := make(map[int]string)

	rows, err := c.Pool.Query(ctx,
		`SELECT name, port FROM applications WHERE server_id = $1 AND port > 0`, serverID)
	if err != nil {
		return nil, fmt.Errorf("query app ports: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		var port int
		if err = rows.Scan(&name, &port); err != nil {
			return nil, err
		}
		used[port] = fmt.Sprintf("existing app %q", name)
	}
	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate app ports: %w", err)
	}

	drows, err := c.Pool.Query(ctx,
		`SELECT name, public_port FROM databases WHERE server_id = $1 AND is_public = true AND public_port > 0`, serverID)
	if err != nil {
		return nil, fmt.Errorf("query db ports: %w", err)
	}
	defer drows.Close()
	for drows.Next() {
		var name string
		var port int
		if err = drows.Scan(&name, &port); err != nil {
			return nil, err
		}
		used[port] = fmt.Sprintf("existing database %q", name)
	}
	if err = drows.Err(); err != nil {
		return nil, fmt.Errorf("iterate db ports: %w", err)
	}

	return used, nil
}

// AppRow is the minimal column set we insert into applications. Mirrors
// coolifygo's CreateApplication call sig, keeping defaults for fields the
// migrater doesn't carry over from v3.
type AppRow struct {
	EnvVars            map[string]string
	Name               string
	GitRepo            string
	Branch             string
	BuildPack          string
	GitSourceID        string // uuid string of coolifygo git_sources row, "" if none
	BaseDirectory      string
	DockerfileLocation string
	ContainerID        string
	ImageName          string
	Status             string
	WebhookSecret      string
	Port               int
	ServerID           uuid.UUID
	IsBot              bool
	AutoDeploy         bool
}

// InsertApplication inserts a single application row, returning its new UUID.
// Wraps the encrypted env-vars as JSONB. git_source_id is a real FK column on
// applications; nil when the v3 app had no GitHub App attached.
func (c *Client) InsertApplication(ctx context.Context, tx pgx.Tx, a AppRow) (uuid.UUID, error) {
	envJSON, err := json.Marshal(a.EnvVars)
	if err != nil {
		return uuid.Nil, fmt.Errorf("env_vars: %w", err)
	}
	if a.EnvVars == nil {
		envJSON = []byte("{}")
	}

	var gitSourceArg any
	if a.GitSourceID != "" {
		gsID, perr := uuid.Parse(a.GitSourceID)
		if perr != nil {
			return uuid.Nil, fmt.Errorf("git_source_id %q: %w", a.GitSourceID, perr)
		}
		gitSourceArg = gsID
	}

	var id uuid.UUID
	err = tx.QueryRow(ctx, `
		INSERT INTO applications (
			server_id, name, git_repo, branch, build_pack, port,
			env_vars, is_bot, auto_deploy,
			base_directory, dockerfile_location,
			status, container_id, image_name, webhook_secret,
			source_type, git_source_id
		)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,'git',$16)
		RETURNING id`,
		a.ServerID, a.Name, a.GitRepo, a.Branch, a.BuildPack, a.Port,
		envJSON, a.IsBot, a.AutoDeploy,
		a.BaseDirectory, a.DockerfileLocation,
		a.Status, a.ContainerID, a.ImageName, a.WebhookSecret,
		gitSourceArg,
	).Scan(&id)
	if err != nil {
		return uuid.Nil, fmt.Errorf("insert application: %w", err)
	}
	return id, nil
}

// DBRow is the minimal column set we insert into databases.
type DBRow struct {
	Name            string
	Slug            string
	Type            string
	Version         string
	DBUser          string
	Password        string // plaintext; encrypted by InsertDatabase
	RootUser        string
	RootPassword    string // plaintext; encrypted by InsertDatabase
	DefaultDatabase string
	ContainerID     string
	Status          string
	Port            int
	InternalPort    int
	PublicPort      int
	ServerID        uuid.UUID
	IsPublic        bool
	AppendOnly      bool // Redis AOF; ignored for non-Redis types
}

// InsertDatabase inserts a single database row. Encrypts password + root_password.
func (c *Client) InsertDatabase(ctx context.Context, tx pgx.Tx, d DBRow) (uuid.UUID, error) {
	encPw, err := c.Encrypt(d.Password)
	if err != nil {
		return uuid.Nil, fmt.Errorf("encrypt password: %w", err)
	}
	encRoot, err := c.Encrypt(d.RootPassword)
	if err != nil {
		return uuid.Nil, fmt.Errorf("encrypt root_password: %w", err)
	}
	var id uuid.UUID
	err = tx.QueryRow(ctx, `
		INSERT INTO databases (
			server_id, name, slug, type, version, db_user, password,
			port, internal_port, root_user, root_password, default_database,
			is_public, public_port, append_only, container_id, status
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17)
		RETURNING id`,
		d.ServerID, d.Name, d.Slug, d.Type, d.Version, d.DBUser, encPw,
		d.Port, d.InternalPort, d.RootUser, encRoot, d.DefaultDatabase,
		d.IsPublic, d.PublicPort, d.AppendOnly, d.ContainerID, d.Status,
	).Scan(&id)
	if err != nil {
		return uuid.Nil, fmt.Errorf("insert database: %w", err)
	}
	return id, nil
}

// UpdateAppContainer records the real container id + status of an application
// after takeover. The insert phase seeds container_id with v3's (now-removed)
// container; coolifygo's Stop/Logs/Stats handlers read applications.container_id
// directly, so we must overwrite it with the freshly-created coolifygo container
// or those actions target a dead id until the reconciler heals it on restart.
func (c *Client) UpdateAppContainer(ctx context.Context, id uuid.UUID, containerID, status string) error {
	_, err := c.Pool.Exec(ctx,
		`UPDATE applications SET status = $2, container_id = $3, updated_at = NOW() WHERE id = $1`,
		id, status, containerID)
	if err != nil {
		return fmt.Errorf("update application container %s: %w", id, err)
	}
	return nil
}

// UpdateDBContainer records the real container id + status of a database after
// takeover, for the same reason as UpdateAppContainer: coolifygo's database
// Stop/Logs/Stats handlers key off databases.container_id.
func (c *Client) UpdateDBContainer(ctx context.Context, id uuid.UUID, containerID, status string) error {
	_, err := c.Pool.Exec(ctx,
		`UPDATE databases SET status = $2, container_id = $3, updated_at = NOW() WHERE id = $1`,
		id, status, containerID)
	if err != nil {
		return fmt.Errorf("update database container %s: %w", id, err)
	}
	return nil
}

// AppInfo is the minimal read-back shape --oldfix needs to reconcile an
// already-inserted application row against a live container. No secrets, so no
// decryption path is involved.
type AppInfo struct {
	Name        string
	ImageName   string
	ContainerID string
	Status      string
	ID          uuid.UUID
	IsBot       bool
}

// ListApplications returns every application row on the given server. Used by
// --oldfix to find rows whose expected container isn't running. COALESCE keeps
// the scan total-order clean for the nullable text columns.
func (c *Client) ListApplications(ctx context.Context, serverID uuid.UUID) ([]AppInfo, error) {
	rows, err := c.Pool.Query(ctx, `
		SELECT id, name, COALESCE(image_name,''), COALESCE(container_id,''),
		       COALESCE(status,''), is_bot
		  FROM applications
		 WHERE server_id = $1`, serverID)
	if err != nil {
		return nil, fmt.Errorf("list applications: %w", err)
	}
	defer rows.Close()
	var out []AppInfo
	for rows.Next() {
		var a AppInfo
		if err = rows.Scan(&a.ID, &a.Name, &a.ImageName, &a.ContainerID, &a.Status, &a.IsBot); err != nil {
			return nil, fmt.Errorf("scan application: %w", err)
		}
		out = append(out, a)
	}
	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate applications: %w", err)
	}
	return out, nil
}

// AdoptApplication marks an application running against a live (renamed)
// container. Unlike UpdateAppContainer it also refreshes image_name (so the
// reconciler can heal the app later — it recreates only when status='running'
// AND image_name != ”) and the host port (the interrupted takeover left it at
// 0 because no workload was matched at insert time). A detected host port also
// forces is_bot=false: an app exposed on the host can't be a network-only
// bot/worker, and coolifygo binds the host port only when port>0 && !is_bot, so
// a stale is_bot=true would make the reconciler/restart drop the binding even
// though the adopted (renamed) container still has it. The COALESCE/NULLIF and
// CASE guards leave a good image_name / already-set port / non-bot flag intact
// when the caller passes "" / 0, so a refresh never clobbers correct data.
func (c *Client) AdoptApplication(ctx context.Context, id uuid.UUID, containerID, imageName string, hostPort int) error {
	_, err := c.Pool.Exec(ctx, `
		UPDATE applications
		   SET status = 'running',
		       container_id = $2,
		       image_name = COALESCE(NULLIF($3,''), image_name),
		       port = CASE WHEN $4::int > 0 THEN $4 ELSE port END,
		       is_bot = CASE WHEN $4::int > 0 THEN false ELSE is_bot END,
		       updated_at = NOW()
		 WHERE id = $1`, id, containerID, imageName, hostPort)
	if err != nil {
		return fmt.Errorf("adopt application %s: %w", id, err)
	}
	return nil
}

// GitSourceRow is the minimal column set for an inserted GitHub App.
type GitSourceRow struct {
	Name          string
	AppSlug       string
	ClientID      string
	ClientSecret  string // plaintext
	WebhookSecret string // plaintext
	PEM           string // plaintext PEM
	AppID         int64
}

// InsertGitSource creates a github-app row, returning its UUID.
func (c *Client) InsertGitSource(ctx context.Context, tx pgx.Tx, g GitSourceRow) (uuid.UUID, error) {
	encCS, err := c.Encrypt(g.ClientSecret)
	if err != nil {
		return uuid.Nil, fmt.Errorf("encrypt client_secret: %w", err)
	}
	encWH, err := c.Encrypt(g.WebhookSecret)
	if err != nil {
		return uuid.Nil, fmt.Errorf("encrypt webhook_secret: %w", err)
	}
	encPEM, err := c.Encrypt(g.PEM)
	if err != nil {
		return uuid.Nil, fmt.Errorf("encrypt pem: %w", err)
	}
	var id uuid.UUID
	err = tx.QueryRow(ctx, `
		INSERT INTO git_sources (
			name, provider, auth_type, app_id, app_slug, client_id,
			client_secret, webhook_secret, pem
		) VALUES ($1,'github','github-app',$2,$3,$4,$5,$6,$7)
		RETURNING id`,
		g.Name, g.AppID, g.AppSlug, g.ClientID, encCS, encWH, encPEM,
	).Scan(&id)
	if err != nil {
		return uuid.Nil, fmt.Errorf("insert git source: %w", err)
	}
	return id, nil
}
