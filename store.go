package main

import (
	"fmt"
	"time"

	"github.com/jmoiron/sqlx"
	_ "modernc.org/sqlite"
)

// JobStatus represents the processing state of a queued job.
type JobStatus string

const (
	StatusPending    JobStatus = "pending"
	StatusProcessing JobStatus = "processing"
	StatusDone       JobStatus = "done"
	StatusError      JobStatus = "error"
)

// StoredJob is a persistent job record in SQLite.
type StoredJob struct {
	ID        int64     `db:"id"`
	Type      string    `db:"type"`    // "prompt" or "command"
	Payload   string    `db:"payload"` // the prompt text or command string
	Args      string    `db:"args"`    // JSON-encoded command args (empty for prompts)
	Status    JobStatus `db:"status"`
	Response  string    `db:"response"` // accumulated response text
	Error     string    `db:"error"`    // error message if status=error
	CreatedAt time.Time `db:"created_at"`
	UpdatedAt time.Time `db:"updated_at"`
}

// Store manages the SQLite-backed job queue.
type Store struct {
	db *sqlx.DB
}

// NewStore opens (or creates) the SQLite database at the given path.
func NewStore(path string) (*Store, error) {
	db, err := sqlx.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}

	// WAL mode for better concurrent read/write performance.
	db.MustExec("PRAGMA journal_mode=WAL")

	// Create tables.
	db.MustExec(`
		CREATE TABLE IF NOT EXISTS jobs (
			id         INTEGER PRIMARY KEY AUTOINCREMENT,
			type       TEXT NOT NULL,
			payload    TEXT NOT NULL,
			args       TEXT NOT NULL DEFAULT '',
			status     TEXT NOT NULL DEFAULT 'pending',
			response   TEXT NOT NULL DEFAULT '',
			error      TEXT NOT NULL DEFAULT '',
			created_at DATETIME NOT NULL DEFAULT (datetime('now')),
			updated_at DATETIME NOT NULL DEFAULT (datetime('now'))
		)
	`)

	// Index for picking up pending/processing jobs on restart.
	db.MustExec(`
		CREATE INDEX IF NOT EXISTS idx_jobs_status ON jobs(status)
	`)

	return &Store{db: db}, nil
}

// Enqueue adds a new job to the queue and returns its ID.
func (s *Store) Enqueue(jobType, payload, args string) (int64, error) {
	result, err := s.db.Exec(
		`INSERT INTO jobs (type, payload, args, status) VALUES (?, ?, ?, ?)`,
		jobType, payload, args, StatusPending,
	)
	if err != nil {
		return 0, fmt.Errorf("enqueue: %w", err)
	}
	return result.LastInsertId()
}

// NextPending returns the oldest pending job and marks it as processing.
// Returns nil if no pending jobs exist.
func (s *Store) NextPending() (*StoredJob, error) {
	tx, err := s.db.Beginx()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	var j StoredJob
	err = tx.Get(&j,
		`SELECT * FROM jobs WHERE status = ? ORDER BY id ASC LIMIT 1`,
		StatusPending,
	)
	if err != nil {
		return nil, nil // no pending jobs
	}

	_, err = tx.Exec(
		`UPDATE jobs SET status = ?, updated_at = datetime('now') WHERE id = ?`,
		StatusProcessing, j.ID,
	)
	if err != nil {
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}

	j.Status = StatusProcessing
	return &j, nil
}

// AppendResponse appends text to a job's response field.
func (s *Store) AppendResponse(id int64, text string) error {
	_, err := s.db.Exec(
		`UPDATE jobs SET response = response || ?, updated_at = datetime('now') WHERE id = ?`,
		text, id,
	)
	return err
}

// Complete marks a job as done.
func (s *Store) Complete(id int64) error {
	_, err := s.db.Exec(
		`UPDATE jobs SET status = ?, updated_at = datetime('now') WHERE id = ?`,
		StatusDone, id,
	)
	return err
}

// Fail marks a job as errored with a message.
func (s *Store) Fail(id int64, errMsg string) error {
	_, err := s.db.Exec(
		`UPDATE jobs SET status = ?, error = ?, updated_at = datetime('now') WHERE id = ?`,
		StatusError, errMsg, id,
	)
	return err
}

// GetJob retrieves a job by ID.
func (s *Store) GetJob(id int64) (*StoredJob, error) {
	var j StoredJob
	err := s.db.Get(&j, `SELECT * FROM jobs WHERE id = ?`, id)
	if err != nil {
		return nil, err
	}
	return &j, nil
}

// PendingCount returns the number of pending jobs.
func (s *Store) PendingCount() (int, error) {
	var count int
	err := s.db.Get(&count, `SELECT COUNT(*) FROM jobs WHERE status = ?`, StatusPending)
	return count, err
}

// RecoverProcessing resets any jobs stuck in "processing" back to "pending".
// Called on startup to recover from crashes.
func (s *Store) RecoverProcessing() (int64, error) {
	result, err := s.db.Exec(
		`UPDATE jobs SET status = ?, updated_at = datetime('now') WHERE status = ?`,
		StatusPending, StatusProcessing,
	)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

// Close closes the database connection.
func (s *Store) Close() error {
	return s.db.Close()
}
