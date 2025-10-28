package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"html"
	"io"
	"log"
	"net/http"
	"net/mail"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"

	_ "modernc.org/sqlite"
)

// waitlistRequest models the expected JSON payload.
type waitlistRequest struct {
	Email string `json:"email"`
	Trap  string `json:"nickname,omitempty"`
}

type server struct {
	db *sql.DB
}

const schema = `
CREATE TABLE IF NOT EXISTS waitlist (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    email TEXT NOT NULL UNIQUE,
    created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS waitlist_honeypot (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    email TEXT NOT NULL,
    trap_value TEXT NOT NULL,
    created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);`

func main() {
	log.SetFlags(0)

	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, usage(filepath.Base(os.Args[0])))
		os.Exit(1)
	}

	switch os.Args[1] {
	case "serve":
		serveCmd := flag.NewFlagSet("serve", flag.ExitOnError)
		dbPath := serveCmd.String("f", "", "path to SQLite database file (defaults to waitlist.db or $DATABASE_PATH)")
		serveCmd.Parse(os.Args[2:])

		if err := runAPIServer(*dbPath); err != nil {
			log.Fatalf("server error: %v", err)
		}
	case "list":
		listCmd := flag.NewFlagSet("list", flag.ExitOnError)
		dbPath := listCmd.String("f", "", "path to SQLite database file (defaults to waitlist.db or $DATABASE_PATH)")
		honeypotOnly := listCmd.Bool("honeypot", false, "list only honeypot trap submissions")
		listCmd.Parse(os.Args[2:])

		if err := listWaitlistEntries(*dbPath, os.Stdout, *honeypotOnly); err != nil {
			log.Fatalf("list failed: %v", err)
		}
	case "demo":
		demoCmd := flag.NewFlagSet("demo", flag.ExitOnError)
		dir := demoCmd.String("dir", ".", "directory where the demo SQLite database will be created")
		demoCmd.Parse(os.Args[2:])

		dbPath, err := createDemoDatabase(*dir)
		if err != nil {
			log.Fatalf("demo setup failed: %v", err)
		}
		log.Printf("demo database created at %s", dbPath)

		if err := runAPIServer(dbPath); err != nil {
			log.Fatalf("demo server error: %v", err)
		}
	case "-h", "--help":
		fmt.Println(usage(filepath.Base(os.Args[0])))
	default:
		fmt.Fprintln(os.Stderr, usage(filepath.Base(os.Args[0])))
		log.Fatalf("unknown subcommand %q", os.Args[1])
	}
}

func usage(cmd string) string {
	return fmt.Sprintf(`Usage:
  %s serve [-f path]
  %s list [-f path] [--honeypot]
  %s demo [-dir path]

Commands:
  serve   Start the waitlist HTTP API server.
  list    Print waitlist entries (use --honeypot for trap submissions).
  demo    Launch the demo server with a fresh SQLite database.`, cmd, cmd, cmd)
}

func runAPIServer(dbPathOverride string) error {
	dbPath := resolveDatabasePath(dbPathOverride)

	db, err := setupDatabase(dbPath)
	if err != nil {
		return fmt.Errorf("database setup failed: %w", err)
	}
	defer db.Close()

	srv := &server{db: db}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/waitlist", srv.waitlistHandler)

	addr := serverAddr()
	log.Printf("waitlist API listening on %s (database %s)", addr, dbPath)

	if err := http.ListenAndServe(addr, mux); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("listen and serve: %w", err)
	}

	return nil
}

func listWaitlistEntries(dbPathOverride string, out io.Writer, honeypotOnly bool) error {
	dbPath := resolveDatabasePath(dbPathOverride)

	if dbPath != ":memory:" && !strings.HasPrefix(dbPath, "file:") {
		if _, err := os.Stat(dbPath); errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("database file %q not found", dbPath)
		} else if err != nil {
			return fmt.Errorf("stat database: %w", err)
		}
	}

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer db.Close()

	if err := initializeDatabase(db); err != nil {
		return fmt.Errorf("initialize database: %w", err)
	}

	var rows *sql.Rows
	queryLabel := "waitlist"
	if honeypotOnly {
		rows, err = db.Query(`SELECT id, email, trap_value, created_at FROM waitlist_honeypot ORDER BY created_at ASC, id ASC`)
		queryLabel = "honeypot"
	} else {
		rows, err = db.Query(`SELECT id, email, created_at FROM waitlist ORDER BY created_at ASC, id ASC`)
	}
	if err != nil {
		return fmt.Errorf("query %s: %w", queryLabel, err)
	}
	defer rows.Close()

	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	if honeypotOnly {
		fmt.Fprintln(tw, "ID\tEmail\tTrap Value\tCreated At")
	} else {
		fmt.Fprintln(tw, "ID\tEmail\tCreated At")
	}

	count := 0
	for rows.Next() {
		if honeypotOnly {
			var (
				id        int64
				email     string
				trapValue string
				created   string
			)
			if err := rows.Scan(&id, &email, &trapValue, &created); err != nil {
				return fmt.Errorf("scan row: %w", err)
			}
			fmt.Fprintf(tw, "%d\t%s\t%s\t%s\n", id, email, trapValue, created)
		} else {
			var (
				id      int64
				email   string
				created string
			)
			if err := rows.Scan(&id, &email, &created); err != nil {
				return fmt.Errorf("scan row: %w", err)
			}
			fmt.Fprintf(tw, "%d\t%s\t%s\n", id, email, created)
		}
		count++
	}

	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate rows: %w", err)
	}

	if count == 0 {
		if honeypotOnly {
			fmt.Fprintln(tw, "(no honeypot entries)\t\t\t")
		} else {
			fmt.Fprintln(tw, "(no entries)\t\t")
		}
	}

	if err := tw.Flush(); err != nil {
		return fmt.Errorf("flush output: %w", err)
	}

	return nil
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
	trapValue := ""
	switch {
	case isJSON:
		var payload waitlistRequest
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			writeMessage(w, http.StatusBadRequest, "invalid JSON body", false)
			return
		}
		email = payload.Email
		trapValue = payload.Trap
	default:
		if err := r.ParseForm(); err != nil {
			writeMessage(w, http.StatusBadRequest, "invalid form data", true)
			return
		}
		email = r.FormValue("email")
		trapValue = r.FormValue("nickname")
	}

	email = strings.TrimSpace(email)
	trapValue = strings.TrimSpace(trapValue)

	if trapValue != "" {
		if err := s.insertHoneypot(r.Context(), email, trapValue); err != nil {
			log.Printf("failed to insert honeypot entry: %v", err)
			writeMessage(w, http.StatusInternalServerError, "internal server error", !isJSON)
			return
		}

		writeMessage(w, http.StatusCreated, "email accepted for waitlist", !isJSON)
		return
	}

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

func (s *server) insertHoneypot(ctx context.Context, email, trapValue string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO waitlist_honeypot(email, trap_value) VALUES (?, ?)`, email, trapValue)
	return err
}

func isUniqueConstraint(err error) bool {
	return strings.Contains(err.Error(), "UNIQUE constraint failed")
}

func setupDatabase(path string) (*sql.DB, error) {
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

func initializeDatabase(db *sql.DB) error {
	if err := db.Ping(); err != nil {
		return err
	}

	if _, err := db.Exec(schema); err != nil {
		return err
	}

	return nil
}

func resolveDatabasePath(override string) string {
	if override != "" {
		return override
	}
	return databasePath()
}

func databasePath() string {
	if path, ok := os.LookupEnv("DATABASE_PATH"); ok && path != "" {
		return path
	}
	return "waitlist.db"
}

func createDemoDatabase(dir string) (string, error) {
	if dir == "" {
		dir = "."
	}

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("ensure demo directory: %w", err)
	}

	f, err := os.CreateTemp(dir, "waitlist-demo-*.db")
	if err != nil {
		return "", fmt.Errorf("create demo database file: %w", err)
	}

	name := f.Name()
	if err := f.Close(); err != nil {
		return "", fmt.Errorf("close demo database file: %w", err)
	}

	return name, nil
}

func serverAddr() string {
	if port, ok := os.LookupEnv("PORT"); ok && port != "" {
		return ":" + port
	}
	return ":8080"
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
