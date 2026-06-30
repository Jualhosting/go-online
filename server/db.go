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
			token TEXT UNIQUE,
			is_anonymous INTEGER DEFAULT 1,
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
		`CREATE TABLE IF NOT EXISTS webhook_logs (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			subdomain TEXT NOT NULL,
			method TEXT NOT NULL,
			url TEXT NOT NULL,
			headers TEXT NOT NULL,
			body TEXT NOT NULL,
			received_at DATETIME DEFAULT CURRENT_TIMESTAMP
		);`,
		`CREATE TABLE IF NOT EXISTS traffic_stats (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			subdomain TEXT NOT NULL,
			bytes_sent INTEGER NOT NULL,
			logged_at DATETIME DEFAULT CURRENT_TIMESTAMP
		);`,
		`CREATE TABLE IF NOT EXISTS audit_events (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			user_id TEXT NOT NULL,
			event_type TEXT NOT NULL,
			details TEXT NOT NULL,
			logged_at DATETIME DEFAULT CURRENT_TIMESTAMP
		);`,
		`CREATE TABLE IF NOT EXISTS billing_transactions (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			user_id TEXT NOT NULL,
			reference_id TEXT UNIQUE NOT NULL,
			trx_id TEXT,
			amount REAL NOT NULL,
			channel_code TEXT NOT NULL,
			status TEXT NOT NULL DEFAULT 'pending',
			payment_info TEXT,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			FOREIGN KEY(user_id) REFERENCES users(id)
		);`,
	}

	for _, query := range queries {
		if _, err := m.db.Exec(query); err != nil {
			return err
		}
	}

	// Dynamic column migrations (ignore errors if columns exist)
	_, _ = m.db.Exec("ALTER TABLE users ADD COLUMN token TEXT")
	_, _ = m.db.Exec("CREATE UNIQUE INDEX IF NOT EXISTS idx_users_token ON users(token)")
	_, _ = m.db.Exec("ALTER TABLE users ADD COLUMN is_anonymous INTEGER DEFAULT 1")

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

func (m *DBManager) LogWebhookRequest(subdomain, method, url, headers, body string) error {
	_, err := m.db.Exec(
		"INSERT INTO webhook_logs (subdomain, method, url, headers, body) VALUES (?, ?, ?, ?, ?)",
		subdomain, method, url, headers, body,
	)
	return err
}

func (m *DBManager) LogTraffic(subdomain string, bytesSent int64) error {
	_, err := m.db.Exec(
		"INSERT INTO traffic_stats (subdomain, bytes_sent) VALUES (?, ?)",
		subdomain, bytesSent,
	)
	return err
}

func (m *DBManager) LogAuditEvent(userID, eventType, details string) error {
	_, err := m.db.Exec(
		"INSERT INTO audit_events (user_id, event_type, details) VALUES (?, ?, ?)",
		userID, eventType, details,
	)
	return err
}

func (m *DBManager) ValidateUserToken(token string) (string, error) {
	var userID string
	err := m.db.QueryRow("SELECT id FROM users WHERE token = ?", token).Scan(&userID)
	return userID, err
}

func (m *DBManager) GetSubdomainOwnerToken(subdomain string) (string, error) {
	var token string
	err := m.db.QueryRow("SELECT u.token FROM subdomains s JOIN users u ON s.user_id = u.id WHERE s.subdomain = ?", subdomain).Scan(&token)
	return token, err
}

func (m *DBManager) AssociateTokenWithUser(userID, token string) error {
	_, err := m.db.Exec("UPDATE users SET token = ?, is_anonymous = 0 WHERE id = ?", token, userID)
	return err
}

func (m *DBManager) CreateBillingTransaction(userID, referenceID, trxID string, amount float64, channelCode string, status string, paymentInfo string) error {
	_, err := m.db.Exec(
		`INSERT INTO billing_transactions (user_id, reference_id, trx_id, amount, channel_code, status, payment_info)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		userID, referenceID, trxID, amount, channelCode, status, paymentInfo,
	)
	return err
}

func (m *DBManager) UpdateBillingTransactionStatus(referenceID, status string) (string, error) {
	var userID string
	err := m.db.QueryRow("SELECT user_id FROM billing_transactions WHERE reference_id = ?", referenceID).Scan(&userID)
	if err != nil {
		return "", err
	}
	_, err = m.db.Exec("UPDATE billing_transactions SET status = ?, updated_at = CURRENT_TIMESTAMP WHERE reference_id = ?", status, referenceID)
	if err != nil {
		return "", err
	}
	return userID, nil
}

func (m *DBManager) UpdateUserPlan(userID, planType string) error {
	_, err := m.db.Exec("UPDATE users SET plan_type = ? WHERE id = ?", planType, userID)
	return err
}
