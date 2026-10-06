package main

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"

	"golang.org/x/crypto/bcrypt"
	_ "modernc.org/sqlite"
)

var db *sql.DB

type UserAuthRequest struct {
	Username string `json:"user"`
	Password string `json:"pass"`
}

type AuthResponse struct {
	Token  string `json:"token,omitempty"`
	UserID int64  `json:"uid,omitempty"`
	Error  string `json:"err,omitempty"`
}

type Post struct {
	ID       int64  `json:"id"`
	Username string `json:"u"`
	Text     string `json:"t"`
	Date     string `json:"d"`
}

type FeedResponse struct {
	Posts []Post `json:"posts"`
}

type CreatePostRequest struct {
	Text string `json:"t"`
}

func initDB() {
	var err error
	db, err = sql.Open("sqlite", "./social.db")
	if err != nil {
		log.Fatal(err)
	}

	schema := `
	CREATE TABLE IF NOT EXISTS users (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		username TEXT UNIQUE NOT NULL,
		password_hash TEXT NOT NULL,
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP
	);

	CREATE TABLE IF NOT EXISTS sessions (
		token TEXT PRIMARY KEY,
		user_id INTEGER NOT NULL,
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		FOREIGN KEY(user_id) REFERENCES users(id)
	);

	CREATE TABLE IF NOT EXISTS posts (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		user_id INTEGER NOT NULL,
		content TEXT NOT NULL,
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		FOREIGN KEY(user_id) REFERENCES users(id)
	);
	`
	_, err = db.Exec(schema)
	if err != nil {
		log.Fatal("Failed to initialize database schema:", err)
	}
}

func generateToken() string {
	bytes := make([]byte, 16)
	rand.Read(bytes)
	return hex.EncodeToString(bytes)
}

func handleRegister(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req UserAuthRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Username == "" || req.Password == "" {
		http.Error(w, "Invalid input", http.StatusBadRequest)
		return
	}

	hashedPassword, _ := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
	res, err := db.Exec("INSERT INTO users(username, password_hash) VALUES(?, ?)", req.Username, string(hashedPassword))
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(AuthResponse{Error: "Username taken"})
		return
	}

	userID, _ := res.LastInsertId()
	token := generateToken()
	db.Exec("INSERT INTO sessions(token, user_id) VALUES(?, ?)", token, userID)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(AuthResponse{Token: token, UserID: userID})
}

func handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req UserAuthRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid input", http.StatusBadRequest)
		return
	}

	var userID int64
	var hash string
	err := db.QueryRow("SELECT id, password_hash FROM users WHERE username = ?", req.Username).Scan(&userID, &hash)
	if err != nil || bcrypt.CompareHashAndPassword([]byte(hash), []byte(req.Password)) != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(AuthResponse{Error: "Invalid credentials"})
		return
	}

	token := generateToken()
	db.Exec("INSERT INTO sessions(token, user_id) VALUES(?, ?)", token, userID)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(AuthResponse{Token: token, UserID: userID})
}

func authenticate(r *http.Request) (int64, error) {
	token := r.Header.Get("X-Token")
	if token == "" {
		token = r.URL.Query().Get("token")
	}
	if token == "" {
		return 0, fmt.Errorf("no token provided")
	}

	var userID int64
	err := db.QueryRow("SELECT user_id FROM sessions WHERE token = ?", token).Scan(&userID)
	if err != nil {
		return 0, fmt.Errorf("invalid token")
	}
	return userID, nil
}

func handlePosts(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		rows, err := db.Query(`
			SELECT p.id, u.username, p.content, strftime('%H:%M', p.created_at)
			FROM posts p
			JOIN users u ON p.user_id = u.id
			ORDER BY p.id DESC
			LIMIT 10
		`)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer rows.Close()

		posts := make([]Post, 0)
		for rows.Next() {
			var p Post
			if err := rows.Scan(&p.ID, &p.Username, &p.Text, &p.Date); err == nil {
				posts = append(posts, p)
			}
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(FeedResponse{Posts: posts})

	case http.MethodPost:
		userID, err := authenticate(r)
		if err != nil {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}

		var req CreatePostRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Text) == "" {
			http.Error(w, "Invalid post text", http.StatusBadRequest)
			return
		}

		text := req.Text
		if len(text) > 280 {
			text = text[:280]
		}

		_, err = db.Exec("INSERT INTO posts(user_id, content) VALUES(?, ?)", userID, text)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{"ok":true}`))

	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

func main() {
	initDB()
	defer db.Close()

	http.HandleFunc("/api/register", handleRegister)
	http.HandleFunc("/api/login", handleLogin)
	http.HandleFunc("/api/posts", handlePosts)

	// Render dynamically sets the PORT environment variable
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	fmt.Printf("Server running on port %s\n", port)
	if err := http.ListenAndServe(":"+port, nil); err != nil {
		log.Fatal(err)
	}
}
