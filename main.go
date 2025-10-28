package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"log"
	"net/http"
	"net/mail"
	"os"
	"strings"

	_ "modernc.org/sqlite"
)

// waitlistRequest models the expected JSON payload.
type waitlistRequest struct {
	Email string `json:"email"`
}

type server struct {
	db *sql.DB
}

const schema = `
CREATE TABLE IF NOT EXISTS waitlist (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	email TEXT NOT NULL UNIQUE,
	created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);`

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "demo":
			if err := runDemo(); err != nil {
				log.Fatalf("demo failed: %v", err)
			}
			return
		default:
			log.Fatalf("unknown subcommand %q", os.Args[1])
		}
	}

	if err := runAPIServer(); err != nil {
		log.Fatalf("server error: %v", err)
	}
}

func runAPIServer() error {
	db, err := setupDatabase()
	if err != nil {
		return fmt.Errorf("database setup failed: %w", err)
	}
	defer db.Close()

	srv := &server{db: db}

	mux := http.NewServeMux()
	mux.HandleFunc("/", serveIndex)
	mux.HandleFunc("/api/v1/waitlist", srv.waitlistHandler)

	addr := serverAddr()
	log.Printf("waitlist API listening on %s", addr)

	if err := http.ListenAndServe(addr, mux); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("listen and serve: %w", err)
	}

	return nil
}

func runDemo() error {
	db, err := setupDemoDatabase()
	if err != nil {
		return fmt.Errorf("demo database setup failed: %w", err)
	}
	defer db.Close()

	srv := &server{db: db}

	mux := http.NewServeMux()
	mux.HandleFunc("/", serveIndex)
	mux.HandleFunc("/api/v1/waitlist", srv.waitlistHandler)

	addr := demoAddr()
	log.Printf("demo server serving index.html on %s", addr)
	log.Printf("demo submissions are stored in waitlist-demo.db")

	if err := http.ListenAndServe(addr, mux); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("listen and serve: %w", err)
	}

	return nil
}

func serverAddr() string {
	if port, ok := os.LookupEnv("PORT"); ok && port != "" {
		return ":" + port
	}
	return ":8080"
}

func (s *server) waitlistHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	contentType := r.Header.Get("Content-Type")
	if idx := strings.Index(contentType, ";"); idx != -1 {
		contentType = strings.TrimSpace(contentType[:idx])
	}

	isJSON := contentType == "application/json"

	email := ""
	switch {
	case isJSON:
		var payload waitlistRequest
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			writeMessage(w, http.StatusBadRequest, "invalid JSON body", false)
			return
		}
		email = payload.Email
	default:
		if err := r.ParseForm(); err != nil {
			writeMessage(w, http.StatusBadRequest, "invalid form data", true)
			return
		}
		email = r.FormValue("email")
	}

	email = strings.TrimSpace(email)

	if email == "" {
		writeMessage(w, http.StatusBadRequest, "email is required", !isJSON)
		return
	}

	if _, err := mail.ParseAddress(email); err != nil {
		writeMessage(w, http.StatusBadRequest, "invalid email address", !isJSON)
		return
	}

	if err := s.insertWaitlist(r.Context(), email); err != nil {
		if isUniqueConstraint(err) {
			writeMessage(w, http.StatusConflict, "email already registered", !isJSON)
			return
		}
		log.Printf("failed to insert email: %v", err)
		writeMessage(w, http.StatusInternalServerError, "internal server error", !isJSON)
		return
	}

	writeMessage(w, http.StatusCreated, "email accepted for waitlist", !isJSON)
}

func (s *server) insertWaitlist(ctx context.Context, email string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO waitlist(email) VALUES (?)`, email)
	return err
}

func isUniqueConstraint(err error) bool {
	return strings.Contains(err.Error(), "UNIQUE constraint failed")
}

func setupDatabase() (*sql.DB, error) {
	path := databasePath()

	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}

	if err := initializeDatabase(db); err != nil {
		db.Close()
		return nil, err
	}

	return db, nil
}

func setupDemoDatabase() (*sql.DB, error) {
	db, err := sql.Open("sqlite", "waitlist-demo.db")
	if err != nil {
		return nil, err
	}

	// Single connection keeps the in-memory database alive for the life of the process.
	db.SetMaxOpenConns(1)

	if err := initializeDatabase(db); err != nil {
		db.Close()
		return nil, err
	}

	return db, nil
}

func initializeDatabase(db *sql.DB) error {
	if err := db.Ping(); err != nil {
		return err
	}

	if _, err := db.Exec(schema); err != nil {
		return err
	}

	return nil
}

func databasePath() string {
	if path, ok := os.LookupEnv("DATABASE_PATH"); ok && path != "" {
		return path
	}
	return "waitlist.db"
}

func demoAddr() string {
	if port, ok := os.LookupEnv("DEMO_PORT"); ok && port != "" {
		if strings.HasPrefix(port, ":") {
			return port
		}
		return ":" + port
	}
	return serverAddr()
}

func serveIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}

	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	http.ServeFile(w, r, "index.html")
}

func writeMessage(w http.ResponseWriter, status int, message string, htmlPreferred bool) {
	if htmlPreferred {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(status)
		_, _ = fmt.Fprintf(w, "<!DOCTYPE html><html lang=\"en\"><head><meta charset=\"utf-8\"><title>Waitlist</title></head><body><main><h1>Waitlist</h1><p>%s</p><p><a href=\"/\">Back to form</a></p></main></body></html>", html.EscapeString(message))
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"message": message,
	})
}
