package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"ds-task-queue/internal/models"
	"ds-task-queue/internal/testutil"
	"github.com/google/uuid"
)

func TestMain(m *testing.M) {
	resultCode := m.Run()

	os.Exit(resultCode)
}

func TestEnqueue(t *testing.T) {
	conn := testutil.NewTestDB(t)
	srv := NewServer(conn)

	ts := httptest.NewServer(http.HandlerFunc(srv.handleEnqueue))
	defer ts.Close()

	reqBody := models.EnqueueRequest{Payload: "test_job"}
	body, err := json.Marshal(reqBody)
	if err != nil {
		t.Fatalf("failed marshal req: %v", err)
	}

	resp, err := http.Post(ts.URL, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal("Post failed: ", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200 but got %d", resp.StatusCode)
	}

	var state string
	err = conn.QueryRow("SELECT state FROM Jobs LIMIT 1").Scan(&state)
	if err != nil {
		t.Fatalf("query failed: %v", err)
	}

	if state != "QUEUED" {
		t.Errorf("expected QUEUED, got %s", state)
	}

}

func TestPoll(t *testing.T) {
	conn := testutil.NewTestDB(t)
	srv := NewServer(conn)

	ts := httptest.NewServer(http.HandlerFunc(srv.handlePoll))
	defer ts.Close()

	var id string = uuid.NewString()

	_, err := conn.Exec("INSERT INTO Jobs (id, payload, state) VALUES ($1, $2, $3)", id, "test_job", models.StateQueued)
	if err != nil {
		t.Fatal("Failed to insert job: ", err)
	}

	reqBody := models.PollRequest{WorkerID: "test-worker"}
	body, err := json.Marshal(reqBody)
	if err != nil {
		t.Fatalf("failed marshal req: %v", err)
	}

	resp, err := http.Post(ts.URL, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal("Post failed: ", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200 but got %d", resp.StatusCode)
	}

	var state string
	err = conn.QueryRow("SELECT state FROM Jobs LIMIT 1").Scan(&state)
	if err != nil {
		t.Fatalf("query failed: %v", err)
	}

	if state != "LEASED" {
		t.Errorf("expected LEASED, got %s", state)
	}
}

func TestAck(t *testing.T) {
	conn := testutil.NewTestDB(t)
	srv := NewServer(conn)

	ts := httptest.NewServer(http.HandlerFunc(srv.handleAck))
	defer ts.Close()

	id := uuid.NewString()
	_, err := conn.Exec(`
		INSERT INTO Jobs (id, payload, state, lease_owner, lease_id, lease_expires_at)
		VALUES ($1, $2, $3, $4, $5, $6)
	`, id, "test_job", models.StateLeased, "test-worker", 1, time.Now().Unix()+30)
	if err != nil {
		t.Fatal("failed to insert job: ", err)
	}

	reqBody := models.AckRequest{JobID: id, WorkerID: "test-worker", LeaseID: 1}
	body, err := json.Marshal(reqBody)
	if err != nil {
		t.Fatalf("failed to marshal req: %v", err)
	}

	resp, err := http.Post(ts.URL, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal("Post failed: ", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200 but got %d", resp.StatusCode)
	}

	var state string
	err = conn.QueryRow("SELECT state FROM Jobs WHERE id = $1", id).Scan(&state)
	if err != nil {
		t.Fatalf("query failed: %v", err)
	}
	if state != string(models.StateDone) {
		t.Errorf("expected DONE, got %s", state)
	}
}

func TestHealth(t *testing.T) {
	conn := testutil.NewTestDB(t)
	srv := NewServer(conn)

	ts := httptest.NewServer(http.HandlerFunc(srv.handleHealth))
	defer ts.Close()

	resp, err := http.Get(ts.URL)
	if err != nil {
		t.Fatal("GET failed: ", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200 but got %d", resp.StatusCode)
	}
}

func TestJobs(t *testing.T) {
	conn := testutil.NewTestDB(t)
	srv := NewServer(conn)

	ts := httptest.NewServer(http.HandlerFunc(srv.handleJobs))
	defer ts.Close()

	for i := 0; i < 3; i++ {
		_, err := conn.Exec("INSERT INTO Jobs (id, payload, state) VALUES ($1, $2, $3)",
			uuid.NewString(), "test_job", models.StateQueued)
		if err != nil {
			t.Fatal("failed to insert job: ", err)
		}
	}

	resp, err := http.Get(ts.URL)
	if err != nil {
		t.Fatal("GET failed: ", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200 but got %d", resp.StatusCode)
	}

	var jobs map[string]models.Job
	if err := json.NewDecoder(resp.Body).Decode(&jobs); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if len(jobs) != 3 {
		t.Errorf("expected 3 jobs, got %d", len(jobs))
	}
}

func TestDead(t *testing.T) {
	conn := testutil.NewTestDB(t)
	srv := NewServer(conn)

	ts := httptest.NewServer(http.HandlerFunc(srv.handleDead))
	defer ts.Close()

	_, err := conn.Exec("INSERT INTO Jobs (id, payload, state) VALUES ($1, $2, $3)",
		uuid.NewString(), "done_job", models.StateDone)
	if err != nil {
		t.Fatal("failed to insert done job: ", err)
	}
	deadID := uuid.NewString()
	_, err = conn.Exec("INSERT INTO Jobs (id, payload, state) VALUES ($1, $2, $3)",
		deadID, "dead_job", models.StateDead)
	if err != nil {
		t.Fatal("failed to insert dead job: ", err)
	}

	resp, err := http.Get(ts.URL)
	if err != nil {
		t.Fatal("GET failed: ", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200 but got %d", resp.StatusCode)
	}

	var dead map[string]models.Job
	if err := json.NewDecoder(resp.Body).Decode(&dead); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if len(dead) != 1 {
		t.Errorf("expected 1 dead job, got %d", len(dead))
	}
	if _, ok := dead[deadID]; !ok {
		t.Errorf("expected dead job %s in response", deadID)
	}
}

func TestStaleLeaseIDOnAck(t *testing.T) {
	conn := testutil.NewTestDB(t)
	srv := NewServer(conn)

	ts := httptest.NewServer(http.HandlerFunc(srv.handleAck))
	defer ts.Close()

	id := uuid.NewString()
	_, err := conn.Exec(`
		INSERT INTO Jobs (id, payload, state, lease_owner, lease_id, lease_expires_at)
		VALUES ($1, $2, $3, $4, $5, $6)
	`, id, "test_job", models.StateLeased, "test-worker", 1, time.Now().Unix()+30)
	if err != nil {
		t.Fatal("failed to insert job: ", err)
	}

	reqBody := models.AckRequest{JobID: id, WorkerID: "test-worker", LeaseID: 2}
	body, err := json.Marshal(reqBody)
	if err != nil {
		t.Fatalf("failed to marshal req: %v", err)
	}

	resp, err := http.Post(ts.URL, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal("Post failed: ", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusConflict {
		t.Errorf("expected 409 but got %d", resp.StatusCode)
	}

	var state string
	err = conn.QueryRow("SELECT state FROM Jobs WHERE id = $1", id).Scan(&state)
	if err != nil {
		t.Fatalf("query failed: %v", err)
	}
	if state != string(models.StateLeased) {
		t.Errorf("expected LEASED, got %s", state)
	}
}

func TestStaleLeaseIDOnFail(t *testing.T) {
	conn := testutil.NewTestDB(t)
	srv := NewServer(conn)

	ts := httptest.NewServer(http.HandlerFunc(srv.handleFail))
	defer ts.Close()

	id := uuid.NewString()
	_, err := conn.Exec(`
		INSERT INTO Jobs (id, payload, state, lease_owner, lease_id, lease_expires_at)
		VALUES ($1, $2, $3, $4, $5, $6)
	`, id, "test_job", models.StateLeased, "test-worker", 1, time.Now().Unix()+30)
	if err != nil {
		t.Fatal("failed to insert job: ", err)
	}

	reqBody := models.FailRequest{JobID: id, WorkerID: "test-worker", LeaseID: 2}
	body, err := json.Marshal(reqBody)
	if err != nil {
		t.Fatalf("failed to marshal req: %v", err)
	}

	resp, err := http.Post(ts.URL, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal("Post failed: ", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusConflict {
		t.Errorf("expected 409 but got %d", resp.StatusCode)
	}

	var state string
	err = conn.QueryRow("SELECT state FROM Jobs WHERE id = $1", id).Scan(&state)
	if err != nil {
		t.Fatalf("query failed: %v", err)
	}
	if state != string(models.StateLeased) {
		t.Errorf("expected LEASED, got %s", state)
	}
}

func TestExpiredOnFail(t *testing.T) {
	conn := testutil.NewTestDB(t)
	srv := NewServer(conn)

	ts := httptest.NewServer(http.HandlerFunc(srv.handleFail))
	defer ts.Close()

	id := uuid.NewString()
	_, err := conn.Exec(`
		INSERT INTO Jobs (id, payload, state, lease_owner, lease_id, lease_expires_at)
		VALUES ($1, $2, $3, $4, $5, $6)
	`, id, "test_job", models.StateLeased, "test-worker", 1, time.Now().Unix()-1)
	if err != nil {
		t.Fatal("failed to insert job: ", err)
	}

	reqBody := models.FailRequest{JobID: id, WorkerID: "test-worker", LeaseID: 1}
	body, err := json.Marshal(reqBody)
	if err != nil {
		t.Fatalf("failed to marshal req: %v", err)
	}

	resp, err := http.Post(ts.URL, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal("Post failed: ", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusConflict {
		t.Errorf("expected 409 but got %d", resp.StatusCode)
	}

	var state string
	err = conn.QueryRow("SELECT state FROM Jobs WHERE id = $1", id).Scan(&state)
	if err != nil {
		t.Fatalf("query failed: %v", err)
	}
	if state != string(models.StateLeased) {
		t.Errorf("expected LEASED, got %s", state)
	}
}

func TestExpiredOnAck(t *testing.T) {
	conn := testutil.NewTestDB(t)
	srv := NewServer(conn)

	ts := httptest.NewServer(http.HandlerFunc(srv.handleAck))
	defer ts.Close()

	id := uuid.NewString()
	_, err := conn.Exec(`
		INSERT INTO Jobs (id, payload, state, lease_owner, lease_id, lease_expires_at)
		VALUES ($1, $2, $3, $4, $5, $6)
	`, id, "test_job", models.StateLeased, "test-worker", 1, time.Now().Unix()-1)
	if err != nil {
		t.Fatal("failed to insert job: ", err)
	}

	reqBody := models.AckRequest{JobID: id, WorkerID: "test-worker", LeaseID: 1}
	body, err := json.Marshal(reqBody)
	if err != nil {
		t.Fatalf("failed to marshal req: %v", err)
	}

	resp, err := http.Post(ts.URL, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal("Post failed: ", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusConflict {
		t.Errorf("expected 409 but got %d", resp.StatusCode)
	}

	var state string
	err = conn.QueryRow("SELECT state FROM Jobs WHERE id = $1", id).Scan(&state)
	if err != nil {
		t.Fatalf("query failed: %v", err)
	}
	if state != string(models.StateLeased) {
		t.Errorf("expected LEASED, got %s", state)
	}

}

func TestFail(t *testing.T) {
	conn := testutil.NewTestDB(t)
	srv := NewServer(conn)

	ts := httptest.NewServer(http.HandlerFunc(srv.handleFail))
	defer ts.Close()

	id := uuid.NewString()
	_, err := conn.Exec(`
		INSERT INTO Jobs (id, payload, state, lease_owner, lease_id, lease_expires_at)
		VALUES ($1, $2, $3, $4, $5, $6)
	`, id, "test_job", models.StateLeased, "test-worker", 1, time.Now().Unix()+30)
	if err != nil {
		t.Fatal("failed to insert job: ", err)
	}

	reqBody := models.FailRequest{JobID: id, WorkerID: "test-worker", LeaseID: 1}
	body, err := json.Marshal(reqBody)
	if err != nil {
		t.Fatalf("failed to marshal req: %v", err)
	}

	resp, err := http.Post(ts.URL, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal("Post failed: ", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200 but got %d", resp.StatusCode)
	}

	var state string
	var attempts int
	var nextAvailableAt int64
	err = conn.QueryRow("SELECT state, attempts, next_available_at FROM Jobs WHERE id = $1", id).Scan(&state, &attempts, &nextAvailableAt)
	if err != nil {
		t.Fatalf("query failed: %v", err)
	}
	if state != string(models.StateQueued) {
		t.Errorf("expected QUEUED, got %s", state)
	}
	if attempts != 1 {
		t.Errorf("expected 1 attempts, got %v", attempts)
	}
	if nextAvailableAt < time.Now().Unix() {
		t.Errorf("expected next available at to be greater than or equal to now: %v but, got %v", time.Now().Unix(), nextAvailableAt)
	}
}

func TestDLQ(t *testing.T) {
	conn := testutil.NewTestDB(t)
	srv := NewServer(conn)

	ts := httptest.NewServer(http.HandlerFunc(srv.handleFail))
	defer ts.Close()

	id := uuid.NewString()
	_, err := conn.Exec(`
		INSERT INTO Jobs (id, payload, state, lease_owner, lease_id, lease_expires_at, attempts, max_tries)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
	`, id, "test_job", models.StateLeased, "test-worker", 1, time.Now().Unix()+30, 2, 3)
	if err != nil {
		t.Fatal("failed to insert job: ", err)
	}

	reqBody := models.FailRequest{JobID: id, WorkerID: "test-worker", LeaseID: 1}
	body, err := json.Marshal(reqBody)
	if err != nil {
		t.Fatalf("failed to marshal req: %v", err)
	}

	resp, err := http.Post(ts.URL, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal("Post failed: ", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200 but got %d", resp.StatusCode)
	}

	var state string
	err = conn.QueryRow("SELECT state FROM Jobs WHERE id = $1", id).Scan(&state)
	if err != nil {
		t.Fatalf("query failed: %v", err)
	}
	if state != string(models.StateDead) {
		t.Errorf("expected DEAD, got %s", state)
	}
}

func TestSweep(t *testing.T) {
	conn := testutil.NewTestDB(t)
	srv := NewServer(conn)

	id := uuid.NewString()
	_, err := conn.Exec(`
		INSERT INTO Jobs (id, payload, state, lease_owner, lease_id, lease_expires_at)
		VALUES ($1, $2, $3, $4, $5, $6)
	`, id, "test_job", models.StateLeased, "test-worker", 1, time.Now().Unix()-1)
	if err != nil {
		t.Fatal("failed to insert job: ", err)
	}

	srv.Sweep()

	var state string
	err = conn.QueryRow("SELECT state FROM Jobs WHERE id = $1", id).Scan(&state)
	if err != nil {
		t.Fatalf("query failed: %v", err)
	}
	if state != string(models.StateQueued) {
		t.Errorf("expected QUEUED, got %s", state)
	}
}
