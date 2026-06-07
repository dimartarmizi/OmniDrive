package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

type Store struct {
	db *sql.DB
}

func Open(ctx context.Context, dbPath string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o700); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, err
	}
	store := &Store{db: db}
	if err := store.Migrate(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return store, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) Migrate(ctx context.Context) error {
	stmts := []string{
		`PRAGMA journal_mode=WAL`,
		`CREATE TABLE IF NOT EXISTS accounts (email TEXT PRIMARY KEY, encrypted_refresh_token TEXT NOT NULL, total_space INTEGER NOT NULL DEFAULT 0, used_space INTEGER NOT NULL DEFAULT 0, priority INTEGER NOT NULL DEFAULT 0, is_active INTEGER NOT NULL DEFAULT 1, added_at TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS virtual_files (virtual_path TEXT PRIMARY KEY, display_name TEXT NOT NULL, original_name TEXT NOT NULL, is_dir INTEGER NOT NULL, size INTEGER NOT NULL, mod_time TEXT NOT NULL, account_email TEXT NOT NULL, google_file_id TEXT NOT NULL, parent_id TEXT NOT NULL, mime_type TEXT NOT NULL)`,
		`CREATE INDEX IF NOT EXISTS idx_virtual_parent ON virtual_files(parent_id)`,
		`CREATE INDEX IF NOT EXISTS idx_virtual_account ON virtual_files(account_email)`,
	}
	for _, stmt := range stmts {
		if _, err := s.db.ExecContext(ctx, stmt); err != nil {
			return err
		}
	}
	if err := s.ensureAccountsPriorityColumn(ctx); err != nil {
		return err
	}
	if err := s.normalizeAccountPriorities(ctx); err != nil {
		return err
	}
	return nil
}

func (s *Store) UpsertAccount(ctx context.Context, account CloudAccount) error {
	priority := account.Priority
	if priority <= 0 {
		existing, err := s.GetAccount(ctx, account.Email)
		if err == nil {
			priority = existing.Priority
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
	}
	if priority <= 0 {
		next, err := s.nextAccountPriority(ctx)
		if err != nil {
			return err
		}
		priority = next
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO accounts(email, encrypted_refresh_token, total_space, used_space, priority, is_active, added_at)
		VALUES(?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(email) DO UPDATE SET encrypted_refresh_token=excluded.encrypted_refresh_token,total_space=excluded.total_space,used_space=excluded.used_space,priority=excluded.priority,is_active=excluded.is_active`,
		account.Email, account.EncryptedRefreshToken, account.TotalSpace, account.UsedSpace, priority, boolInt(account.IsActive), account.AddedAt.Format(time.RFC3339Nano))
	return err
}

func (s *Store) ListAccounts(ctx context.Context) ([]CloudAccount, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT email, encrypted_refresh_token, total_space, used_space, priority, is_active, added_at FROM accounts WHERE is_active=1 ORDER BY priority, added_at, email`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var accounts []CloudAccount
	for rows.Next() {
		var a CloudAccount
		var active int
		var added string
		if err := rows.Scan(&a.Email, &a.EncryptedRefreshToken, &a.TotalSpace, &a.UsedSpace, &a.Priority, &active, &added); err != nil {
			return nil, err
		}
		a.IsActive = active == 1
		a.AddedAt, _ = time.Parse(time.RFC3339Nano, added)
		accounts = append(accounts, a)
	}
	return accounts, rows.Err()
}

func (s *Store) GetAccount(ctx context.Context, email string) (*CloudAccount, error) {
	row := s.db.QueryRowContext(ctx, `SELECT email, encrypted_refresh_token, total_space, used_space, priority, is_active, added_at FROM accounts WHERE email=?`, email)
	var a CloudAccount
	var active int
	var added string
	if err := row.Scan(&a.Email, &a.EncryptedRefreshToken, &a.TotalSpace, &a.UsedSpace, &a.Priority, &active, &added); err != nil {
		return nil, err
	}
	a.IsActive = active == 1
	a.AddedAt, _ = time.Parse(time.RFC3339Nano, added)
	return &a, nil
}

func (s *Store) TotalQuota(ctx context.Context) (total, used int64, err error) {
	row := s.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(total_space), 0), COALESCE(SUM(used_space), 0) FROM accounts WHERE is_active=1`)
	err = row.Scan(&total, &used)
	return
}

func (s *Store) BestUploadAccount(ctx context.Context) (*CloudAccount, error) {
	return s.BestUploadAccountForSize(ctx, 0)
}

func (s *Store) BestUploadAccountForSize(ctx context.Context, requiredSize int64) (*CloudAccount, error) {
	accounts, err := s.ListAccounts(ctx)
	if err != nil {
		return nil, err
	}
	if len(accounts) == 0 {
		return nil, fmt.Errorf("no active accounts")
	}
	if requiredSize <= 0 {
		return &accounts[0], nil
	}
	for i := range accounts {
		if accounts[i].TotalSpace-accounts[i].UsedSpace >= requiredSize {
			return &accounts[i], nil
		}
	}
	return nil, fmt.Errorf("no active account has enough free space for %d bytes", requiredSize)
}

func (s *Store) RemoveAccount(ctx context.Context, email string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `DELETE FROM accounts WHERE email=?`, email)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return sql.ErrNoRows
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM virtual_files WHERE account_email=?`, email); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return s.normalizeAccountPriorities(ctx)
}

func (s *Store) SetAccountPriority(ctx context.Context, email string, position int) error {
	accounts, err := s.ListAccounts(ctx)
	if err != nil {
		return err
	}
	if len(accounts) == 0 {
		return fmt.Errorf("no active accounts")
	}
	if position < 1 {
		position = 1
	}
	if position > len(accounts) {
		position = len(accounts)
	}
	index := -1
	for i := range accounts {
		if accounts[i].Email == email {
			index = i
			break
		}
	}
	if index < 0 {
		return sql.ErrNoRows
	}
	picked := accounts[index]
	accounts = append(accounts[:index], accounts[index+1:]...)
	insertAt := position - 1
	accounts = append(accounts[:insertAt], append([]CloudAccount{picked}, accounts[insertAt:]...)...)
	return s.writeAccountPriorities(ctx, accounts)
}

func (s *Store) ensureAccountsPriorityColumn(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, `PRAGMA table_info(accounts)`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name string
		var typ string
		var notnull int
		var dflt sql.NullString
		var pk int
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
			return err
		}
		if name == "priority" {
			return nil
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `ALTER TABLE accounts ADD COLUMN priority INTEGER NOT NULL DEFAULT 0`)
	return err
}

func (s *Store) nextAccountPriority(ctx context.Context) (int, error) {
	row := s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(priority), 0) FROM accounts`)
	var max int
	if err := row.Scan(&max); err != nil {
		return 0, err
	}
	return max + 1, nil
}

func (s *Store) normalizeAccountPriorities(ctx context.Context) error {
	accounts, err := s.listAccountsUnordered(ctx)
	if err != nil {
		return err
	}
	if len(accounts) == 0 {
		return nil
	}
	return s.writeAccountPriorities(ctx, accounts)
}

func (s *Store) listAccountsUnordered(ctx context.Context) ([]CloudAccount, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT email, encrypted_refresh_token, total_space, used_space, priority, is_active, added_at FROM accounts WHERE is_active=1 ORDER BY CASE WHEN priority <= 0 THEN 1 ELSE 0 END, priority, added_at, email`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var accounts []CloudAccount
	for rows.Next() {
		var a CloudAccount
		var active int
		var added string
		if err := rows.Scan(&a.Email, &a.EncryptedRefreshToken, &a.TotalSpace, &a.UsedSpace, &a.Priority, &active, &added); err != nil {
			return nil, err
		}
		a.IsActive = active == 1
		a.AddedAt, _ = time.Parse(time.RFC3339Nano, added)
		accounts = append(accounts, a)
	}
	return accounts, rows.Err()
}

func (s *Store) writeAccountPriorities(ctx context.Context, accounts []CloudAccount) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for i := range accounts {
		if _, err := tx.ExecContext(ctx, `UPDATE accounts SET priority=? WHERE email=?`, i+1, accounts[i].Email); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) ReplaceAccountFiles(ctx context.Context, email string, files []VirtualFile) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM virtual_files WHERE account_email=?`, email); err != nil {
		return err
	}
	for _, f := range files {
		if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO virtual_files(virtual_path, display_name, original_name, is_dir, size, mod_time, account_email, google_file_id, parent_id, mime_type) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			f.VirtualPath, f.DisplayName, f.OriginalName, boolInt(f.IsDir), f.Size, f.ModTime.Format(time.RFC3339Nano), f.AccountEmail, f.GoogleFileID, f.ParentID, f.MimeType); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) ReplaceAllFiles(ctx context.Context, files []VirtualFile) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM virtual_files`); err != nil {
		return err
	}
	for _, f := range files {
		if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO virtual_files(virtual_path, display_name, original_name, is_dir, size, mod_time, account_email, google_file_id, parent_id, mime_type) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			f.VirtualPath, f.DisplayName, f.OriginalName, boolInt(f.IsDir), f.Size, f.ModTime.Format(time.RFC3339Nano), f.AccountEmail, f.GoogleFileID, f.ParentID, f.MimeType); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) UpsertVirtualFile(ctx context.Context, f VirtualFile) error {
	_, err := s.db.ExecContext(ctx, `INSERT OR REPLACE INTO virtual_files(virtual_path, display_name, original_name, is_dir, size, mod_time, account_email, google_file_id, parent_id, mime_type) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		f.VirtualPath, f.DisplayName, f.OriginalName, boolInt(f.IsDir), f.Size, f.ModTime.Format(time.RFC3339Nano), f.AccountEmail, f.GoogleFileID, f.ParentID, f.MimeType)
	return err
}

func (s *Store) DeleteVirtualFile(ctx context.Context, virtualPath string) error {
	virtualPath = cleanVirtualPath(virtualPath)
	_, err := s.db.ExecContext(ctx, `DELETE FROM virtual_files WHERE virtual_path=?`, virtualPath)
	return err
}

func (s *Store) DeleteVirtualTree(ctx context.Context, virtualPath string) error {
	virtualPath = cleanVirtualPath(virtualPath)
	like := virtualPath + "/%"
	_, err := s.db.ExecContext(ctx, `DELETE FROM virtual_files WHERE virtual_path=? OR virtual_path LIKE ?`, virtualPath, like)
	return err
}

func (s *Store) HasChildren(ctx context.Context, virtualDir string) (bool, error) {
	virtualDir = cleanVirtualPath(virtualDir)
	like := virtualDir + "/%"
	row := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM virtual_files WHERE virtual_path LIKE ?)`, like)
	var exists int
	if err := row.Scan(&exists); err != nil {
		return false, err
	}
	return exists == 1, nil
}

func (s *Store) RenameVirtualPath(ctx context.Context, oldPath, newPath string) error {
	oldPath = cleanVirtualPath(oldPath)
	newPath = cleanVirtualPath(newPath)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	rows, err := tx.QueryContext(ctx, `SELECT virtual_path, display_name, original_name, is_dir, size, mod_time, account_email, google_file_id, parent_id, mime_type FROM virtual_files WHERE virtual_path=? OR virtual_path LIKE ? ORDER BY virtual_path`, oldPath, oldPath+"/%")
	if err != nil {
		return err
	}
	defer rows.Close()

	var files []VirtualFile
	for rows.Next() {
		f, err := scanVirtualFile(rows)
		if err != nil {
			return err
		}
		files = append(files, f)
	}
	if err := rows.Err(); err != nil {
		return err
	}

	if _, err := tx.ExecContext(ctx, `DELETE FROM virtual_files WHERE virtual_path=? OR virtual_path LIKE ?`, oldPath, oldPath+"/%"); err != nil {
		return err
	}

	for _, f := range files {
		suffix := strings.TrimPrefix(f.VirtualPath, oldPath)
		f.VirtualPath = cleanVirtualPath(newPath + suffix)
		if f.VirtualPath == newPath {
			f.DisplayName = filepath.Base(newPath)
		}
		if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO virtual_files(virtual_path, display_name, original_name, is_dir, size, mod_time, account_email, google_file_id, parent_id, mime_type) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			f.VirtualPath, f.DisplayName, f.OriginalName, boolInt(f.IsDir), f.Size, f.ModTime.Format(time.RFC3339Nano), f.AccountEmail, f.GoogleFileID, f.ParentID, f.MimeType); err != nil {
			return err
		}
	}

	return tx.Commit()
}

func (s *Store) ListVirtualFiles(ctx context.Context) ([]VirtualFile, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT virtual_path, display_name, original_name, is_dir, size, mod_time, account_email, google_file_id, parent_id, mime_type FROM virtual_files ORDER BY virtual_path`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var files []VirtualFile
	for rows.Next() {
		f, err := scanVirtualFile(rows)
		if err != nil {
			return nil, err
		}
		files = append(files, f)
	}
	return files, rows.Err()
}

func (s *Store) Children(ctx context.Context, virtualDir string) ([]VirtualFile, error) {
	virtualDir = cleanVirtualPath(virtualDir)
	var rows *sql.Rows
	var err error
	if virtualDir == "" {
		rows, err = s.db.QueryContext(ctx, `SELECT virtual_path, display_name, original_name, is_dir, size, mod_time, account_email, google_file_id, parent_id, mime_type FROM virtual_files WHERE instr(virtual_path, '/')=0 ORDER BY virtual_path`)
	} else {
		rows, err = s.db.QueryContext(ctx, `SELECT virtual_path, display_name, original_name, is_dir, size, mod_time, account_email, google_file_id, parent_id, mime_type FROM virtual_files WHERE virtual_path LIKE ? AND instr(substr(virtual_path, ?), '/')=0 ORDER BY virtual_path`, virtualDir+"/%", len(virtualDir)+2)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var children []VirtualFile
	for rows.Next() {
		f, err := scanVirtualFile(rows)
		if err != nil {
			return nil, err
		}
		children = append(children, f)
	}
	return children, rows.Err()
}

func (s *Store) GetVirtualFile(ctx context.Context, virtualPath string) (*VirtualFile, error) {
	virtualPath = cleanVirtualPath(virtualPath)
	row := s.db.QueryRowContext(ctx, `SELECT virtual_path, display_name, original_name, is_dir, size, mod_time, account_email, google_file_id, parent_id, mime_type FROM virtual_files WHERE virtual_path=?`, virtualPath)
	f, err := scanVirtualFile(row)
	if err != nil {
		return nil, err
	}
	return &f, nil
}

type rowScanner interface{ Scan(dest ...any) error }

func scanVirtualFile(row rowScanner) (VirtualFile, error) {
	var f VirtualFile
	var dir int
	var mod string
	err := row.Scan(&f.VirtualPath, &f.DisplayName, &f.OriginalName, &dir, &f.Size, &mod, &f.AccountEmail, &f.GoogleFileID, &f.ParentID, &f.MimeType)
	f.IsDir = dir == 1
	f.ModTime, _ = time.Parse(time.RFC3339Nano, mod)
	return f, err
}

func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}

func cleanVirtualPath(path string) string {
	path = strings.ReplaceAll(path, `\\`, `/`)
	path = strings.TrimPrefix(path, `/`)
	path = strings.TrimSuffix(path, `/`)
	return path
}

func parentOf(path string) string {
	path = cleanVirtualPath(path)
	idx := strings.LastIndex(path, `/`)
	if idx < 0 {
		return ""
	}
	return path[:idx]
}
