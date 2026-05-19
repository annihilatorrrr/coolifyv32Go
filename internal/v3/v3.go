// Package v3 reads a Coolify v3 SQLite database and decrypts its secrets.
//
// v3 stores its data in /app/db/prod.db inside the `coolify` container (volume
// `coolify-db`). Secrets are sealed with AES-256-CTR keyed by the
// COOLIFY_SECRET_KEY env var, serialised as JSON {iv, content} in hex.
package v3

import (
	"crypto/aes"
	"crypto/cipher"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	_ "modernc.org/sqlite"
)

// Application mirrors the v3 `Application` Prisma row plus its joined
// ApplicationSettings, Secrets, and gitSourceId resolution. Only the fields we
// migrate are populated; the rest of v3's schema is intentionally ignored.
type Application struct {
	Secrets            map[string]string // plaintext after decryption
	ID                 string
	Name               string
	Repository         string
	Branch             string
	BuildPack          string
	BaseDirectory      string
	DockerFileLocation string
	GitSourceID        string
	DestinationID      string
	Port               int
	AutoDeploy         bool
	IsBot              bool
}

// Database mirrors v3's `Database` Prisma row plus the joined `DatabaseSettings.appendOnly`
// for Redis instances. v3 defaults appendOnly to true; we preserve whatever the
// user actually had set so a migrated Redis keeps its persistence guarantees.
type Database struct {
	ID               string
	Name             string
	Type             string // v3 stores: postgresql, mysql, mongodb, redis, ...
	Version          string
	DefaultDatabase  string
	DBUser           string
	DBUserPassword   string // plaintext after decryption
	RootUser         string
	RootUserPassword string // plaintext after decryption
	DestinationID    string
	PublicPort       int
	AppendOnly       bool // Redis AOF; carried from v3 DatabaseSettings, meaningless for non-Redis
}

// GitHubApp mirrors v3's `GithubApp` row.
type GitHubApp struct {
	ID             string
	Name           string
	ClientID       string
	ClientSecret   string // plaintext after decryption
	WebhookSecret  string // plaintext after decryption
	PrivateKey     string // plaintext after decryption (PEM)
	AppID          int64
	InstallationID int64
}

// GitSource mirrors v3's `GitSource` row (only github-app rows handled).
type GitSource struct {
	ID           string
	Name         string
	Type         string
	APIURL       string
	HTMLURL      string
	Organization string
	GitHubAppID  string
}

// Destination mirrors v3's `DestinationDocker` row. We only migrate the local
// one — remote destinations are out of scope per the user's brief.
type Destination struct {
	ID           string
	Name         string
	Network      string
	Engine       string
	RemoteEngine bool
}

// Client reads from an opened v3 SQLite file. Cheap to create; close when done.
type Client struct {
	db        *sql.DB
	secretKey []byte
}

// Open returns a Client backed by the given SQLite file. secretKey is the raw
// 32-byte COOLIFY_SECRET_KEY string from v3's env — Node's crypto.createCipheriv
// uses it verbatim as the AES key.
func Open(dbPath, secretKey string) (*Client, error) {
	if len(secretKey) != 32 {
		return nil, fmt.Errorf("v3 secret key must be 32 bytes, got %d", len(secretKey))
	}
	db, err := sql.Open("sqlite", "file:"+dbPath+"?mode=ro&immutable=1")
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	if err = db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("sqlite ping: %w", err)
	}
	return &Client{db: db, secretKey: []byte(secretKey)}, nil
}

// Close releases the SQLite handle.
func (c *Client) Close() error { return c.db.Close() }

// Decrypt unseals a v3 ciphertext. v3 stores the empty string for unset secrets
// — we mirror that behaviour and return "" for empty input.
func (c *Client) Decrypt(stored string) (string, error) {
	if stored == "" {
		return "", nil
	}
	var hash struct {
		IV      string `json:"iv"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal([]byte(stored), &hash); err != nil {
		return "", fmt.Errorf("v3 ciphertext is not JSON: %w", err)
	}
	iv, err := hex.DecodeString(hash.IV)
	if err != nil {
		return "", fmt.Errorf("iv hex: %w", err)
	}
	if len(iv) != aes.BlockSize {
		return "", errors.New("iv length != 16")
	}
	ct, err := hex.DecodeString(hash.Content)
	if err != nil {
		return "", fmt.Errorf("content hex: %w", err)
	}
	block, err := aes.NewCipher(c.secretKey)
	if err != nil {
		return "", fmt.Errorf("aes: %w", err)
	}
	stream := cipher.NewCTR(block, iv)
	plain := make([]byte, len(ct))
	stream.XORKeyStream(plain, ct)
	return string(plain), nil
}

// Applications returns all v3 applications joined with settings + decrypted
// secrets. Apps with no name are skipped (v3's "unconfigured" placeholder).
func (c *Client) Applications() ([]Application, error) {
	rows, err := c.db.Query(`
		SELECT a.id, a.name,
		       COALESCE(a.repository, ''), COALESCE(a.branch, ''),
		       COALESCE(a.buildPack, ''), COALESCE(a.port, 0),
		       COALESCE(a.baseDirectory, ''),
		       COALESCE(a.dockerFileLocation, ''),
		       COALESCE(a.gitSourceId, ''),
		       COALESCE(a.destinationDockerId, ''),
		       COALESCE(s.autodeploy, 1),
		       COALESCE(s.isBot, 0)
		FROM Application a
		LEFT JOIN ApplicationSettings s ON s.applicationId = a.id
		WHERE a.name IS NOT NULL AND a.name <> ''`)
	if err != nil {
		return nil, fmt.Errorf("select applications: %w", err)
	}
	defer rows.Close()
	var out []Application
	for rows.Next() {
		var a Application
		if err = rows.Scan(&a.ID, &a.Name, &a.Repository, &a.Branch,
			&a.BuildPack, &a.Port, &a.BaseDirectory, &a.DockerFileLocation,
			&a.GitSourceID, &a.DestinationID, &a.AutoDeploy, &a.IsBot); err != nil {
			return nil, fmt.Errorf("scan application: %w", err)
		}
		secrets, serr := c.appSecrets(a.ID)
		if serr != nil {
			return nil, fmt.Errorf("load secrets for app %s: %w", a.ID, serr)
		}
		a.Secrets = secrets
		out = append(out, a)
	}
	return out, rows.Err()
}

func (c *Client) appSecrets(appID string) (map[string]string, error) {
	rows, err := c.db.Query(
		`SELECT name, value FROM Secret WHERE applicationId = ? AND isPRMRSecret = 0`,
		appID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]string)
	for rows.Next() {
		var name, ct string
		if err = rows.Scan(&name, &ct); err != nil {
			return nil, err
		}
		plain, derr := c.Decrypt(ct)
		if derr != nil {
			return nil, fmt.Errorf("decrypt secret %s: %w", name, derr)
		}
		out[name] = plain
	}
	return out, rows.Err()
}

// Databases returns all v3 databases with their passwords decrypted.
func (c *Client) Databases() ([]Database, error) {
	// LEFT JOIN DatabaseSettings: the relation is optional in v3's schema, so
	// a DB whose settings row was never written joins to NULL. v3's prisma
	// default for appendOnly is true — match it, otherwise a missing-row
	// edge case would silently turn AOF off on the new container.
	rows, err := c.db.Query(`
		SELECT d.id, d.name,
		       COALESCE(d.type, ''), COALESCE(d.version, ''),
		       COALESCE(d.defaultDatabase, ''),
		       COALESCE(d.dbUser, ''), COALESCE(d.dbUserPassword, ''),
		       COALESCE(d.rootUser, ''), COALESCE(d.rootUserPassword, ''),
		       COALESCE(d.publicPort, 0),
		       COALESCE(d.destinationDockerId, ''),
		       COALESCE(s.appendOnly, 1)
		FROM Database d
		LEFT JOIN DatabaseSettings s ON s.databaseId = d.id
		WHERE d.name IS NOT NULL AND d.name <> ''`)
	if err != nil {
		return nil, fmt.Errorf("select databases: %w", err)
	}
	defer rows.Close()
	var out []Database
	for rows.Next() {
		var d Database
		var dbPw, rootPw string
		var aofInt int // sqlite stores BOOLEAN as 0/1
		if err = rows.Scan(&d.ID, &d.Name, &d.Type, &d.Version,
			&d.DefaultDatabase, &d.DBUser, &dbPw, &d.RootUser, &rootPw,
			&d.PublicPort, &d.DestinationID, &aofInt); err != nil {
			return nil, fmt.Errorf("scan database: %w", err)
		}
		d.AppendOnly = aofInt != 0
		if d.DBUserPassword, err = c.Decrypt(dbPw); err != nil {
			return nil, fmt.Errorf("decrypt db user pw (%s): %w", d.ID, err)
		}
		if d.RootUserPassword, err = c.Decrypt(rootPw); err != nil {
			return nil, fmt.Errorf("decrypt root pw (%s): %w", d.ID, err)
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// GitHubApps returns all rows from v3's `GithubApp` table with decrypted PEM,
// clientSecret, webhookSecret. Rows with no appId are filtered out (manifest
// flow incomplete).
func (c *Client) GitHubApps() ([]GitHubApp, error) {
	rows, err := c.db.Query(`
		SELECT id, COALESCE(name, ''),
		       COALESCE(appId, 0), COALESCE(installationId, 0),
		       COALESCE(clientId, ''), COALESCE(clientSecret, ''),
		       COALESCE(webhookSecret, ''), COALESCE(privateKey, '')
		FROM GithubApp
		WHERE appId IS NOT NULL AND appId > 0`)
	if err != nil {
		return nil, fmt.Errorf("select github apps: %w", err)
	}
	defer rows.Close()
	var out []GitHubApp
	for rows.Next() {
		var g GitHubApp
		var cs, ws, pk string
		if err = rows.Scan(&g.ID, &g.Name, &g.AppID, &g.InstallationID,
			&g.ClientID, &cs, &ws, &pk); err != nil {
			return nil, fmt.Errorf("scan github app: %w", err)
		}
		if g.ClientSecret, err = c.Decrypt(cs); err != nil {
			return nil, fmt.Errorf("decrypt client_secret (%s): %w", g.ID, err)
		}
		if g.WebhookSecret, err = c.Decrypt(ws); err != nil {
			return nil, fmt.Errorf("decrypt webhook_secret (%s): %w", g.ID, err)
		}
		if g.PrivateKey, err = c.Decrypt(pk); err != nil {
			return nil, fmt.Errorf("decrypt private_key (%s): %w", g.ID, err)
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// GitSources returns all v3 git sources (we only consume github-app rows).
func (c *Client) GitSources() ([]GitSource, error) {
	rows, err := c.db.Query(`
		SELECT id, name,
		       COALESCE(type, ''), COALESCE(apiUrl, ''),
		       COALESCE(htmlUrl, ''), COALESCE(organization, ''),
		       COALESCE(githubAppId, '')
		FROM GitSource
		WHERE name IS NOT NULL AND name <> ''`)
	if err != nil {
		return nil, fmt.Errorf("select git sources: %w", err)
	}
	defer rows.Close()
	var out []GitSource
	for rows.Next() {
		var g GitSource
		if err = rows.Scan(&g.ID, &g.Name, &g.Type, &g.APIURL,
			&g.HTMLURL, &g.Organization, &g.GitHubAppID); err != nil {
			return nil, fmt.Errorf("scan git source: %w", err)
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// Destinations returns all v3 destinations. Only local ones get migrated.
func (c *Client) Destinations() ([]Destination, error) {
	rows, err := c.db.Query(`
		SELECT id, name,
		       COALESCE(network, ''),
		       COALESCE(engine, ''),
		       COALESCE(remoteEngine, 0)
		FROM DestinationDocker`)
	if err != nil {
		return nil, fmt.Errorf("select destinations: %w", err)
	}
	defer rows.Close()
	var out []Destination
	for rows.Next() {
		var d Destination
		if err = rows.Scan(&d.ID, &d.Name, &d.Network, &d.Engine, &d.RemoteEngine); err != nil {
			return nil, fmt.Errorf("scan destination: %w", err)
		}
		out = append(out, d)
	}
	return out, rows.Err()
}
