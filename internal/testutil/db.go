package testutil

import (
	"database/sql"
	"testing"

	_ "github.com/lib/pq"
)

func NewTestDB(t *testing.T) *sql.DB {
	conn, err := sql.Open("postgres", "postgres://postgres:db_password@localhost:5432/myDB?sslmode=disable")
	if err != nil {
		t.Fatal("failed to open connection: ", err)
	}

	if err = conn.Ping(); err != nil {
		t.Fatal("failed to ping db: ", err)
	}

	_, err = conn.Exec("TRUNCATE TABLE Jobs, Idems RESTART IDENTITY CASCADE")
	if err != nil {
		t.Fatal("failed db exec: ", err)
	}

	t.Cleanup(func() { conn.Exec("TRUNCATE TABLE Jobs, Idems RESTART IDENTITY CASCADE") })

	return conn
}
