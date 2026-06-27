package server

import (
	"database/sql"
	"fmt"
	"log"

	_ "modernc.org/sqlite"
)

type SubdomainRecord struct {
	ID           int64
	UserID       string
	Subdomain    string
	RoutingType  string
	CustomDomain sql.NullString
	IsActive     bool
}

type DBManager struct {
	db *sql.DB
}

func NewDBManager(dbPath string) (*DBManager, error) {
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("failed to open database: %w", err)
	}

	mgr := &DBManager{db: db}
	if err := mgr.migrate(); err != nil {
		db.Close()
		return nil, fmt.Errorf("failed to migrate database: %w", err)
	}

	if err := mgr.seed(); err != nil {
		db.Close()
		return nil, fmt.Errorf("failed to seed database: %w", err)
	}

	return mgr, nil
}

func (m *DBManager) Close() error {
	return m.db.Close()
}

func (m *DBManager) migrate() error {
	queries := []string{
		`CREATE TABLE IF NOT EXISTS users (
			id TEXT PRIMARY KEY,
			email TEXT UNIQUE NOT NULL,
			plan_type TEXT DEFAULT 'free',
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP
		);`,
		`CREATE TABLE IF NOT EXISTS subdomains (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			user_id TEXT NOT NULL,
			subdomain TEXT UNIQUE NOT NULL,
			routing_type TEXT NOT NULL,
			custom_domain TEXT UNIQUE,
			is_active INTEGER DEFAULT 1,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			FOREIGN KEY(user_id) REFERENCES users(id)
		);`,
		`CREATE TABLE IF NOT EXISTS static_deploys (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			subdomain_id INTEGER NOT NULL,
			r2_bucket_folder TEXT NOT NULL,
			version_output TEXT NOT NULL,
			deployed_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			FOREIGN KEY(subdomain_id) REFERENCES subdomains(id)
		);`,
	}

	for _, query := range queries {
		if _, err := m.db.Exec(query); err != nil {
			return err
		}
	}
	return nil
}

func (m *DBManager) seed() error {
	// Check if users empty
	var count int
	err := m.db.QueryRow("SELECT COUNT(*) FROM users").Scan(&count)
	if err != nil {
		return err
	}

	if count > 0 {
		return nil // Already seeded
	}

	log.Println("[DB] Seeding default testing data...")

	// Start transaction
	tx, err := m.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// Insert default user
	_, err = tx.Exec("INSERT INTO users (id, email, plan_type) VALUES (?, ?, ?)", "user_syafri", "syafri@goinstant.my.id", "free")
	if err != nil {
		return fmt.Errorf("seed user: %w", err)
	}

	// Insert default subdomains
	_, err = tx.Exec("INSERT INTO subdomains (user_id, subdomain, routing_type, is_active) VALUES (?, ?, ?, ?)", "user_syafri", "toko-syafri", "tunnel", 1)
	if err != nil {
		return fmt.Errorf("seed subdomain toko-syafri: %w", err)
	}

	_, err = tx.Exec("INSERT INTO subdomains (user_id, subdomain, routing_type, is_active) VALUES (?, ?, ?, ?)", "user_syafri", "portofolio", "static", 1)
	if err != nil {
		return fmt.Errorf("seed subdomain portofolio: %w", err)
	}

	_, err = tx.Exec("INSERT INTO subdomains (user_id, subdomain, routing_type, is_active) VALUES (?, ?, ?, ?)", "user_syafri", "testapp", "tunnel", 1)
	if err != nil {
		return fmt.Errorf("seed subdomain testapp: %w", err)
	}

	_, err = tx.Exec("INSERT INTO subdomains (user_id, subdomain, routing_type, is_active) VALUES (?, ?, ?, ?)", "user_syafri", "staticapp", "static", 1)
	if err != nil {
		return fmt.Errorf("seed subdomain staticapp: %w", err)
	}

	return tx.Commit()
}

func (m *DBManager) LoadAllSubdomains() ([]SubdomainRecord, error) {
	rows, err := m.db.Query("SELECT id, user_id, subdomain, routing_type, custom_domain, is_active FROM subdomains")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var records []SubdomainRecord
	for rows.Next() {
		var rec SubdomainRecord
		var customDomain sql.NullString
		var isActive int
		err := rows.Scan(&rec.ID, &rec.UserID, &rec.Subdomain, &rec.RoutingType, &customDomain, &isActive)
		if err != nil {
			return nil, err
		}
		rec.CustomDomain = customDomain
		rec.IsActive = isActive == 1
		records = append(records, rec)
	}
	return records, nil
}

func (m *DBManager) RegisterSubdomain(userID, subdomain, routingType, customDomain string) (int64, error) {
	var cd interface{}
	if customDomain != "" {
		cd = customDomain
	}

	// Ensure user exists (auto create if not exists for convenience)
	var exists bool
	_ = m.db.QueryRow("SELECT EXISTS(SELECT 1 FROM users WHERE id = ?)", userID).Scan(&exists)
	if !exists {
		_, err := m.db.Exec("INSERT INTO users (id, email) VALUES (?, ?)", userID, userID+"@goinstant.my.id")
		if err != nil {
			return 0, fmt.Errorf("auto-create user failed: %w", err)
		}
	}

	res, err := m.db.Exec(
		"INSERT INTO subdomains (user_id, subdomain, routing_type, custom_domain, is_active) VALUES (?, ?, ?, ?, 1) ON CONFLICT(subdomain) DO UPDATE SET routing_type = EXCLUDED.routing_type, custom_domain = EXCLUDED.custom_domain, is_active = 1",
		userID, subdomain, routingType, cd,
	)
	if err != nil {
		return 0, err
	}

	return res.LastInsertId()
}

func (m *DBManager) GetSubdomainID(subdomain string) (int64, error) {
	var id int64
	err := m.db.QueryRow("SELECT id FROM subdomains WHERE subdomain = ?", subdomain).Scan(&id)
	return id, err
}

func (m *DBManager) AddStaticDeployment(subdomainID int64, r2BucketFolder, version string) error {
	_, err := m.db.Exec(
		"INSERT INTO static_deploys (subdomain_id, r2_bucket_folder, version_output) VALUES (?, ?, ?)",
		subdomainID, r2BucketFolder, version,
	)
	return err
}

func (m *DBManager) GetLatestDeploymentFolder(subdomainID int64) (string, error) {
	var folder string
	err := m.db.QueryRow("SELECT r2_bucket_folder FROM static_deploys WHERE subdomain_id = ? ORDER BY id DESC LIMIT 1", subdomainID).Scan(&folder)
	return folder, err
}
