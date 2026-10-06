package main

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"
	_ "modernc.org/sqlite"
)

var (
	db         *sql.DB
	typingMu   sync.Mutex
	roomTyping = make(map[int64]string) // roomID -> "username"
)

// -------------------------------------------------------------
// DATABASE INITIALIZATION & SCHEMA
// -------------------------------------------------------------
func initDB() {
	var err error
	db, err = sql.Open("sqlite", "./social.db")
	if err != nil {
		log.Fatal("DB Open Error:", err)
	}

	schema := `
	CREATE TABLE IF NOT EXISTS users (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		username TEXT UNIQUE NOT NULL,
		password_hash TEXT NOT NULL,
		display_name TEXT DEFAULT '',
		phone_number TEXT DEFAULT '',
		bio TEXT DEFAULT 'Exploring retro J2ME 🚀',
		country_code TEXT DEFAULT 'MW',
		profile_views INTEGER DEFAULT 0,
		last_active_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		current_activity TEXT DEFAULT 'Browsing Feed',
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP
	);

	CREATE TABLE IF NOT EXISTS sessions (
		token TEXT PRIMARY KEY,
		user_id INTEGER NOT NULL,
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP
	);

	CREATE TABLE IF NOT EXISTS posts (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		user_id INTEGER NOT NULL,
		content TEXT NOT NULL,
		media_url TEXT DEFAULT '',
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP
	);

	CREATE TABLE IF NOT EXISTS likes (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		post_id INTEGER NOT NULL,
		user_id INTEGER NOT NULL,
		UNIQUE(post_id, user_id)
	);

	CREATE TABLE IF NOT EXISTS comments (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		post_id INTEGER NOT NULL,
		user_id INTEGER NOT NULL,
		content TEXT NOT NULL,
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP
	);

	CREATE TABLE IF NOT EXISTS friendships (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		user_id INTEGER NOT NULL,
		friend_id INTEGER NOT NULL,
		status TEXT NOT NULL,
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		UNIQUE(user_id, friend_id)
	);

	CREATE TABLE IF NOT EXISTS rooms (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		name TEXT NOT NULL,
		topic TEXT DEFAULT 'Welcome to the chatroom!',
		owner_id INTEGER NOT NULL,
		is_private INTEGER DEFAULT 0,
		slow_mode INTEGER DEFAULT 0,
		stage_mode INTEGER DEFAULT 0,
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP
	);

	CREATE TABLE IF NOT EXISTS room_members (
		room_id INTEGER NOT NULL,
		user_id INTEGER NOT NULL,
		role TEXT DEFAULT 'member',
		last_ping DATETIME DEFAULT CURRENT_TIMESTAMP,
		PRIMARY KEY(room_id, user_id)
	);

	CREATE TABLE IF NOT EXISTS room_messages (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		room_id INTEGER NOT NULL,
		user_id INTEGER NOT NULL,
		content TEXT NOT NULL,
		media_url TEXT DEFAULT '',
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP
	);

	CREATE TABLE IF NOT EXISTS room_mutes (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		room_id INTEGER NOT NULL,
		user_id INTEGER NOT NULL,
		is_ghost INTEGER DEFAULT 0,
		muted_until DATETIME NOT NULL,
		UNIQUE(room_id, user_id)
	);

	CREATE TABLE IF NOT EXISTS room_bans (
		room_id INTEGER NOT NULL,
		user_id INTEGER NOT NULL,
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		PRIMARY KEY(room_id, user_id)
	);

	CREATE TABLE IF NOT EXISTS room_warnings (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		room_id INTEGER NOT NULL,
		user_id INTEGER NOT NULL,
		reason TEXT NOT NULL,
		mod_id INTEGER NOT NULL,
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP
	);

	CREATE TABLE IF NOT EXISTS room_word_filters (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		room_id INTEGER NOT NULL,
		word TEXT NOT NULL
	);

	CREATE TABLE IF NOT EXISTS mod_audit_logs (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		room_id INTEGER NOT NULL,
		mod_id INTEGER NOT NULL,
		action TEXT NOT NULL,
		target_name TEXT DEFAULT '',
		details TEXT DEFAULT '',
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP
	);

	CREATE TABLE IF NOT EXISTS direct_messages (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		sender_id INTEGER NOT NULL,
		receiver_id INTEGER NOT NULL,
		content TEXT NOT NULL,
		media_url TEXT DEFAULT '',
		is_read INTEGER DEFAULT 0,
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP
	);

	CREATE TABLE IF NOT EXISTS notifications (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		user_id INTEGER NOT NULL,
		actor_id INTEGER NOT NULL,
		type TEXT NOT NULL,
		title TEXT NOT NULL,
		body TEXT NOT NULL,
		target_id INTEGER DEFAULT 0,
		is_read INTEGER DEFAULT 0,
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP
	);
	`
	_, err = db.Exec(schema)
	if err != nil {
		log.Fatal("DB Schema Error:", err)
	}

	var roomCount int
	db.QueryRow("SELECT COUNT(*) FROM rooms").Scan(&roomCount)
	if roomCount == 0 {
		db.Exec("INSERT INTO rooms(name, topic, owner_id) VALUES('Malawi Lounge', 'General chat & vibe 🇲🇼', 1)")
		db.Exec("INSERT INTO rooms(name, topic, owner_id) VALUES('Retro Tech', 'J2ME, Symbian & Vintage Gadgets 🚀', 1)")
	}

	os.MkdirAll("./uploads", 0755)
}

// -------------------------------------------------------------
// HELPERS
// -------------------------------------------------------------
func generateToken() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func formatTimeAgo(t time.Time) string {
	diff := time.Since(t)
	if diff < time.Minute {
		return "Just now"
	} else if diff < time.Hour {
		return fmt.Sprintf("%dm", int(diff.Minutes()))
	} else if diff < 24*time.Hour {
		return fmt.Sprintf("%dh", int(diff.Hours()))
	} else if diff < 7*24*time.Hour {
		return fmt.Sprintf("%dd", int(diff.Hours()/24))
	}
	return t.Format("02 Jan")
}

func calculateLevel(postCount, likes, days int) (string, string) {
	score := (postCount * 3) + (likes * 2) + days
	if score > 150 {
		return "👑 Legend", "Level 5"
	} else if score > 75 {
		return "🥇 Master", "Level 4"
	} else if score > 30 {
		return "🥈 Senior", "Level 3"
	} else if score > 10 {
		return "🥉 Junior", "Level 2"
	}
	return "🌱 Novice", "Level 1"
}

func authenticate(r *http.Request) (int64, string, error) {
	token := r.Header.Get("X-Token")
	if token == "" {
		token = r.URL.Query().Get("token")
	}
	if token == "" {
		return 0, "", fmt.Errorf("no token")
	}

	var uid int64
	var uname string
	err := db.QueryRow(`
		SELECT u.id, u.username FROM sessions s
		JOIN users u ON s.user_id = u.id
		WHERE s.token = ?
	`, token).Scan(&uid, &uname)
	if err != nil {
		return 0, "", fmt.Errorf("unauthorized")
	}

	db.Exec("UPDATE users SET last_active_at = CURRENT_TIMESTAMP WHERE id = ?", uid)
	return uid, uname, nil
}

func sendNotification(userID, actorID int64, nType, title, body string, targetID int64) {
	if userID == actorID {
		return
	}
	db.Exec(`
		INSERT INTO notifications(user_id, actor_id, type, title, body, target_id)
		VALUES(?, ?, ?, ?, ?, ?)
	`, userID, actorID, nType, title, body, targetID)
}

func logModAction(roomID, modID int64, action, targetName, details string) {
	db.Exec(`
		INSERT INTO mod_audit_logs(room_id, mod_id, action, target_name, details)
		VALUES(?, ?, ?, ?, ?)
	`, roomID, modID, action, targetName, details)
}

// -------------------------------------------------------------
// AUTHENTICATION & PROFILE
// -------------------------------------------------------------
func handleRegister(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	var req struct {
		User  string `json:"user"`
		Pass  string `json:"pass"`
		Phone string `json:"phone"`
	}
	json.NewDecoder(r.Body).Decode(&req)
	req.User = strings.TrimSpace(req.User)
	req.Pass = strings.TrimSpace(req.Pass)

	if req.User == "" || req.Pass == "" {
		http.Error(w, `{"err":"Invalid username or password"}`, 400)
		return
	}

	country := r.Header.Get("CF-IPCountry")
	if country == "" || len(country) != 2 {
		country = "MW"
	}

	hash, _ := bcrypt.GenerateFromPassword([]byte(req.Pass), bcrypt.DefaultCost)
	res, err := db.Exec(`
		INSERT INTO users(username, password_hash, display_name, phone_number, country_code)
		VALUES(?, ?, ?, ?, ?)
	`, req.User, string(hash), req.User, req.Phone, country)
	if err != nil {
		http.Error(w, `{"err":"Username already taken"}`, 400)
		return
	}

	uid, _ := res.LastInsertId()
	token := generateToken()
	db.Exec("INSERT INTO sessions(token, user_id) VALUES(?, ?)", token, uid)

	json.NewEncoder(w).Encode(map[string]interface{}{
		"token": token,
		"uid":   uid,
		"u":     req.User,
		"c":     country,
	})
}

func handleLogin(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	var req struct {
		User string `json:"user"`
		Pass string `json:"pass"`
	}
	json.NewDecoder(r.Body).Decode(&req)

	var uid int64
	var hash, country string
	err := db.QueryRow("SELECT id, password_hash, country_code FROM users WHERE username = ?", req.User).Scan(&uid, &hash, &country)
	if err != nil || bcrypt.CompareHashAndPassword([]byte(hash), []byte(req.Pass)) != nil {
		http.Error(w, `{"err":"Invalid credentials"}`, 401)
		return
	}

	token := generateToken()
	db.Exec("INSERT INTO sessions(token, user_id) VALUES(?, ?)", token, uid)

	json.NewEncoder(w).Encode(map[string]interface{}{
		"token": token,
		"uid":   uid,
		"u":     req.User,
		"c":     country,
	})
}

func handleProfile(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	uid, myName, err := authenticate(r)
	if err != nil {
		http.Error(w, `{"err":"Unauthorized"}`, 401)
		return
	}

	targetUser := r.URL.Query().Get("u")
	if targetUser == "" {
		targetUser = myName
	}

	var targetID int64
	var uname, dname, bio, country, activity string
	var views int
	var lastActive, createdAt time.Time

	err = db.QueryRow(`
		SELECT id, username, display_name, bio, country_code, profile_views, last_active_at, current_activity, created_at
		FROM users WHERE username = ?
	`, targetUser).Scan(&targetID, &uname, &dname, &bio, &country, &views, &lastActive, &activity, &createdAt)
	if err != nil {
		http.Error(w, `{"err":"User not found"}`, 404)
		return
	}

	if targetID != uid {
		db.Exec("UPDATE users SET profile_views = profile_views + 1 WHERE id = ?", targetID)
		views++
	}

	var postCount, likesCount, friendCount int
	db.QueryRow("SELECT COUNT(*) FROM posts WHERE user_id = ?", targetID).Scan(&postCount)
	db.QueryRow("SELECT COUNT(*) FROM likes l JOIN posts p ON l.post_id = p.id WHERE p.user_id = ?", targetID).Scan(&likesCount)
	db.QueryRow("SELECT COUNT(*) FROM friendships WHERE (user_id = ? OR friend_id = ?) AND status = 'accepted'", targetID, targetID).Scan(&friendCount)

	daysActive := int(time.Since(createdAt).Hours() / 24)
	rankTitle, rankLevel := calculateLevel(postCount, likesCount, daysActive)
	isOnline := time.Since(lastActive) < 60*time.Second

	json.NewEncoder(w).Encode(map[string]interface{}{
		"id":      targetID,
		"u":       uname,
		"dn":      dname,
		"bio":     bio,
		"c":       country,
		"online":  isOnline,
		"act":     activity,
		"views":   views,
		"posts":   postCount,
		"likes":   likesCount,
		"friends": friendCount,
		"rank":    rankTitle,
		"lvl":     rankLevel,
		"seen":    formatTimeAgo(lastActive),
		"is_me":   targetID == uid,
	})
}

func handleUpdateStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	uid, _, err := authenticate(r)
	if err != nil {
		http.Error(w, `{"err":"Unauthorized"}`, 401)
		return
	}

	var req struct{ Bio string `json:"bio"` }
	json.NewDecoder(r.Body).Decode(&req)
	db.Exec("UPDATE users SET bio = ? WHERE id = ?", req.Bio, uid)
	w.Write([]byte(`{"ok":true}`))
}

// -------------------------------------------------------------
// SOCIAL FEED & COMMENTS
// -------------------------------------------------------------
func handlePosts(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	uid, _, _ := authenticate(r)

	if r.Method == http.MethodGet {
		if uid > 0 {
			db.Exec("UPDATE users SET current_activity = 'Browsing Feed' WHERE id = ?", uid)
		}

		rows, err := db.Query(`
			SELECT p.id, u.username, u.country_code, u.last_active_at, p.content, p.media_url, p.created_at,
			       (SELECT COUNT(*) FROM likes WHERE post_id = p.id),
			       (SELECT COUNT(*) FROM comments WHERE post_id = p.id),
			       EXISTS(SELECT 1 FROM likes WHERE post_id = p.id AND user_id = ?)
			FROM posts p
			JOIN users u ON p.user_id = u.id
			ORDER BY p.id DESC LIMIT 15
		`, uid)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		defer rows.Close()

		type FeedPost struct {
			ID       int64  `json:"id"`
			User     string `json:"u"`
			Country  string `json:"c"`
			Online   bool   `json:"on"`
			Text     string `json:"t"`
			Media    string `json:"m,omitempty"`
			Time     string `json:"d"`
			Likes    int    `json:"l"`
			Comments int    `json:"cm"`
			Liked    bool   `json:"liked"`
		}

		posts := make([]FeedPost, 0)
		for rows.Next() {
			var p FeedPost
			var lastActive, createdAt time.Time
			var likedInt int
			rows.Scan(&p.ID, &p.User, &p.Country, &lastActive, &p.Text, &p.Media, &createdAt, &p.Likes, &p.Comments, &likedInt)
			p.Online = time.Since(lastActive) < 60*time.Second
			p.Time = formatTimeAgo(createdAt)
			p.Liked = likedInt == 1
			posts = append(posts, p)
		}

		json.NewEncoder(w).Encode(map[string]interface{}{"posts": posts})
		return
	}

	if r.Method == http.MethodPost {
		if uid == 0 {
			http.Error(w, `{"err":"Unauthorized"}`, 401)
			return
		}
		var req struct {
			Text  string `json:"t"`
			Media string `json:"m"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		if strings.TrimSpace(req.Text) == "" && req.Media == "" {
			http.Error(w, `{"err":"Empty post"}`, 400)
			return
		}

		db.Exec("INSERT INTO posts(user_id, content, media_url) VALUES(?, ?, ?)", uid, req.Text, req.Media)
		db.Exec("UPDATE users SET current_activity = 'Posted on Feed' WHERE id = ?", uid)
		w.WriteHeader(201)
		w.Write([]byte(`{"ok":true}`))
	}
}

func handleLike(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	uid, myName, err := authenticate(r)
	if err != nil {
		http.Error(w, `{"err":"Unauthorized"}`, 401)
		return
	}

	postID, _ := strconv.ParseInt(r.URL.Query().Get("id"), 10, 64)
	var postOwnerID int64
	db.QueryRow("SELECT user_id FROM posts WHERE id = ?", postID).Scan(&postOwnerID)

	var exists int
	db.QueryRow("SELECT id FROM likes WHERE post_id = ? AND user_id = ?", postID, uid).Scan(&exists)
	if exists > 0 {
		db.Exec("DELETE FROM likes WHERE post_id = ? AND user_id = ?", postID, uid)
		w.Write([]byte(`{"liked":false}`))
	} else {
		db.Exec("INSERT INTO likes(post_id, user_id) VALUES(?, ?)", postID, uid)
		sendNotification(postOwnerID, uid, "like", "★ New Like", "@"+myName+" liked your post.", postID)
		w.Write([]byte(`{"liked":true}`))
	}
}

func handleComments(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	uid, myName, err := authenticate(r)
	if err != nil {
		http.Error(w, `{"err":"Unauthorized"}`, 401)
		return
	}

	if r.Method == http.MethodGet {
		postID := r.URL.Query().Get("id")
		rows, err := db.Query(`
			SELECT c.id, u.username, u.country_code, u.last_active_at, c.content, c.created_at
			FROM comments c
			JOIN users u ON c.user_id = u.id
			WHERE c.post_id = ? ORDER BY c.id ASC
		`, postID)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		defer rows.Close()

		type CommentItem struct {
			ID      int64  `json:"id"`
			User    string `json:"u"`
			Country string `json:"c"`
			Online  bool   `json:"on"`
			Text    string `json:"t"`
			Time    string `json:"d"`
		}

		comments := make([]CommentItem, 0)
		for rows.Next() {
			var ci CommentItem
			var lastActive, createdAt time.Time
			rows.Scan(&ci.ID, &ci.User, &ci.Country, &lastActive, &ci.Text, &createdAt)
			ci.Online = time.Since(lastActive) < 60*time.Second
			ci.Time = formatTimeAgo(createdAt)
			comments = append(comments, ci)
		}
		json.NewEncoder(w).Encode(map[string]interface{}{"comments": comments})
		return
	}

	if r.Method == http.MethodPost {
		var req struct {
			PostID int64  `json:"post_id"`
			Text   string `json:"t"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		if strings.TrimSpace(req.Text) == "" {
			http.Error(w, `{"err":"Empty comment"}`, 400)
			return
		}

		db.Exec("INSERT INTO comments(post_id, user_id, content) VALUES(?, ?, ?)", req.PostID, uid, req.Text)

		var postOwnerID int64
		db.QueryRow("SELECT user_id FROM posts WHERE id = ?", req.PostID).Scan(&postOwnerID)
		sendNotification(postOwnerID, uid, "comment", "💬 New Reply", "@"+myName+" commented: "+req.Text, req.PostID)

		w.WriteHeader(201)
		w.Write([]byte(`{"ok":true}`))
	}
}

// -------------------------------------------------------------
// CHATROOMS & LIVE ENGINE
// -------------------------------------------------------------
func handleRooms(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	uid, _, err := authenticate(r)
	if err != nil {
		http.Error(w, `{"err":"Unauthorized"}`, 401)
		return
	}

	if r.Method == http.MethodGet {
		rows, err := db.Query(`
			SELECT r.id, r.name, r.topic, r.is_private, r.stage_mode,
			       (SELECT COUNT(*) FROM room_members WHERE room_id = r.id AND last_ping >= datetime('now', '-60 seconds')) as online_count
			FROM rooms r ORDER BY online_count DESC, r.id ASC
		`)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		defer rows.Close()

		type RoomItem struct {
			ID      int64  `json:"id"`
			Name    string `json:"name"`
			Topic   string `json:"topic"`
			Private bool   `json:"priv"`
			Stage   bool   `json:"stage"`
			Online  int    `json:"online"`
		}

		rooms := make([]RoomItem, 0)
		for rows.Next() {
			var ri RoomItem
			var priv, stage int
			rows.Scan(&ri.ID, &ri.Name, &ri.Topic, &priv, &stage, &ri.Online)
			ri.Private = priv == 1
			ri.Stage = stage == 1
			rooms = append(rooms, ri)
		}
		json.NewEncoder(w).Encode(map[string]interface{}{"rooms": rooms})
		return
	}

	if r.Method == http.MethodPost {
		var req struct {
			Name    string `json:"name"`
			Topic   string `json:"topic"`
			Private bool   `json:"priv"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		privInt := 0
		if req.Private {
			privInt = 1
		}

		res, _ := db.Exec("INSERT INTO rooms(name, topic, owner_id, is_private) VALUES(?, ?, ?, ?)", req.Name, req.Topic, uid, privInt)
		roomID, _ := res.LastInsertId()
		db.Exec("INSERT INTO room_members(room_id, user_id, role) VALUES(?, ?, 'owner')", roomID, uid)
		w.WriteHeader(201)
		json.NewEncoder(w).Encode(map[string]interface{}{"room_id": roomID})
	}
}

func handleRoomChat(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	uid, _, err := authenticate(r)
	if err != nil {
		http.Error(w, `{"err":"Unauthorized"}`, 401)
		return
	}

	roomID, _ := strconv.ParseInt(r.URL.Query().Get("id"), 10, 64)

	var banned int
	db.QueryRow("SELECT COUNT(*) FROM room_bans WHERE room_id = ? AND user_id = ?", roomID, uid).Scan(&banned)
	if banned > 0 {
		http.Error(w, `{"err":"You are banned from this room"}`, 403)
		return
	}

	db.Exec(`
		INSERT INTO room_members(room_id, user_id, role, last_ping)
		VALUES(?, ?, 'member', CURRENT_TIMESTAMP)
		ON CONFLICT(room_id, user_id) DO UPDATE SET last_ping = CURRENT_TIMESTAMP
	`, roomID, uid)

	var roomName string
	db.QueryRow("SELECT name FROM rooms WHERE id = ?", roomID).Scan(&roomName)
	db.Exec("UPDATE users SET current_activity = ? WHERE id = ?", "In '"+roomName+"'", uid)

	if r.Method == http.MethodGet {
		var onlineCount int
		db.QueryRow("SELECT COUNT(*) FROM room_members WHERE room_id = ? AND last_ping >= datetime('now', '-60 seconds')", roomID).Scan(&onlineCount)

		typingMu.Lock()
		typer := roomTyping[roomID]
		typingMu.Unlock()

		rows, err := db.Query(`
			SELECT m.id, u.username, u.country_code, u.last_active_at, m.content, m.media_url, m.created_at,
			       COALESCE(rm.role, 'member') as role
			FROM room_messages m
			JOIN users u ON m.user_id = u.id
			LEFT JOIN room_members rm ON rm.room_id = m.room_id AND rm.user_id = u.id
			WHERE m.room_id = ? ORDER BY m.id DESC LIMIT 20
		`, roomID)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		defer rows.Close()

		type MsgItem struct {
			ID      int64  `json:"id"`
			User    string `json:"u"`
			Country string `json:"c"`
			Role    string `json:"role"`
			Online  bool   `json:"on"`
			Text    string `json:"t"`
			Media   string `json:"m,omitempty"`
			Time    string `json:"d"`
		}

		msgs := make([]MsgItem, 0)
		for rows.Next() {
			var mi MsgItem
			var lastActive, createdAt time.Time
			rows.Scan(&mi.ID, &mi.User, &mi.Country, &lastActive, &mi.Text, &mi.Media, &createdAt, &mi.Role)
			mi.Online = time.Since(lastActive) < 60*time.Second
			mi.Time = createdAt.Format("15:04")
			msgs = append([]MsgItem{mi}, msgs...)
		}

		json.NewEncoder(w).Encode(map[string]interface{}{
			"online":   onlineCount,
			"typing":   typer,
			"messages": msgs,
		})
		return
	}

	if r.Method == http.MethodPost {
		var mutedUntil time.Time
		var isGhost int
		err := db.QueryRow("SELECT muted_until, is_ghost FROM room_mutes WHERE room_id = ? AND user_id = ?", roomID, uid).Scan(&mutedUntil, &isGhost)
		if err == nil && time.Now().Before(mutedUntil) {
			if isGhost == 0 {
				http.Error(w, `{"err":"You are muted in this room"}`, 403)
				return
			}
			w.WriteHeader(201)
			w.Write([]byte(`{"ok":true}`))
			return
		}

		var stageMode int
		var userRole string
		db.QueryRow("SELECT stage_mode FROM rooms WHERE id = ?", roomID).Scan(&stageMode)
		db.QueryRow("SELECT role FROM room_members WHERE room_id = ? AND user_id = ?", roomID, uid).Scan(&userRole)
		if stageMode == 1 && userRole != "owner" && userRole != "mod" && userRole != "speaker" {
			http.Error(w, `{"err":"Room is in Listen-Only Stage Mode"}`, 403)
			return
		}

		var req struct {
			Text  string `json:"t"`
			Media string `json:"m"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		if strings.TrimSpace(req.Text) == "" && req.Media == "" {
			http.Error(w, `{"err":"Empty message"}`, 400)
			return
		}

		db.Exec("INSERT INTO room_messages(room_id, user_id, content, media_url) VALUES(?, ?, ?, ?)", roomID, uid, req.Text, req.Media)

		typingMu.Lock()
		delete(roomTyping, roomID)
		typingMu.Unlock()

		w.WriteHeader(201)
		w.Write([]byte(`{"ok":true}`))
	}
}

func handleTyping(w http.ResponseWriter, r *http.Request) {
	_, uname, err := authenticate(r)
	if err != nil {
		return
	}
	roomID, _ := strconv.ParseInt(r.URL.Query().Get("id"), 10, 64)

	typingMu.Lock()
	roomTyping[roomID] = uname
	typingMu.Unlock()

	go func(rid int64, username string) {
		time.Sleep(5 * time.Second)
		typingMu.Lock()
		if roomTyping[rid] == username {
			delete(roomTyping, rid)
		}
		typingMu.Unlock()
	}(roomID, uname)

	w.Write([]byte(`{"ok":true}`))
}

// -------------------------------------------------------------
// MODERATION SUITE
// -------------------------------------------------------------
func handleModerate(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	uid, _, err := authenticate(r)
	if err != nil {
		http.Error(w, `{"err":"Unauthorized"}`, 401)
		return
	}

	var req struct {
		RoomID     int64  `json:"room_id"`
		Action     string `json:"action"`
		TargetUser string `json:"target_user"`
		Duration   int    `json:"duration"`
		Role       string `json:"role"`
		Reason     string `json:"reason"`
	}
	json.NewDecoder(r.Body).Decode(&req)

	var myRole string
	db.QueryRow("SELECT role FROM room_members WHERE room_id = ? AND user_id = ?", req.RoomID, uid).Scan(&myRole)
	if myRole != "owner" && myRole != "mod" {
		http.Error(w, `{"err":"Permission denied"}`, 403)
		return
	}

	var targetID int64
	if req.TargetUser != "" {
		db.QueryRow("SELECT id FROM users WHERE username = ?", req.TargetUser).Scan(&targetID)
	}

	switch req.Action {
	case "mute":
		until := time.Now().Add(time.Duration(req.Duration) * time.Minute)
		db.Exec(`
			INSERT INTO room_mutes(room_id, user_id, is_ghost, muted_until)
			VALUES(?, ?, 0, ?)
			ON CONFLICT(room_id, user_id) DO UPDATE SET is_ghost = 0, muted_until = ?
		`, req.RoomID, targetID, until, until)
		logModAction(req.RoomID, uid, "Mute", req.TargetUser, fmt.Sprintf("%dm mute", req.Duration))
		sendNotification(targetID, uid, "mod", "🔇 Muted", fmt.Sprintf("You were muted for %dm in room.", req.Duration), req.RoomID)

	case "ghost_mute":
		until := time.Now().Add(time.Duration(req.Duration) * time.Minute)
		db.Exec(`
			INSERT INTO room_mutes(room_id, user_id, is_ghost, muted_until)
			VALUES(?, ?, 1, ?)
			ON CONFLICT(room_id, user_id) DO UPDATE SET is_ghost = 1, muted_until = ?
		`, req.RoomID, targetID, until, until)
		logModAction(req.RoomID, uid, "Ghost Mute", req.TargetUser, fmt.Sprintf("%dm ghost mute", req.Duration))

	case "kick":
		db.Exec("DELETE FROM room_members WHERE room_id = ? AND user_id = ?", req.RoomID, targetID)
		logModAction(req.RoomID, uid, "Kick", req.TargetUser, "Kicked from room")

	case "ban":
		if myRole != "owner" {
			http.Error(w, `{"err":"Only Owner can ban"}`, 403)
			return
		}
		db.Exec("INSERT OR IGNORE INTO room_bans(room_id, user_id) VALUES(?, ?)", req.RoomID, targetID)
		db.Exec("DELETE FROM room_members WHERE room_id = ? AND user_id = ?", req.RoomID, targetID)
		logModAction(req.RoomID, uid, "Ban", req.TargetUser, "Permanently banned")

	case "nuke":
		db.Exec("DELETE FROM room_messages WHERE room_id = ? AND user_id = ?", req.RoomID, targetID)
		logModAction(req.RoomID, uid, "Nuke", req.TargetUser, "Deleted all user messages")

	case "wipe":
		db.Exec("DELETE FROM room_messages WHERE room_id = ?", req.RoomID)
		logModAction(req.RoomID, uid, "Wipe", "All", "Cleared room history")

	case "stage_toggle":
		db.Exec("UPDATE rooms SET stage_mode = CASE WHEN stage_mode = 1 THEN 0 ELSE 1 END WHERE id = ?", req.RoomID)
		logModAction(req.RoomID, uid, "Stage Toggle", "", "Toggled Listen-Only mode")

	case "set_role":
		if myRole != "owner" {
			http.Error(w, `{"err":"Only Owner can appoint roles"}`, 403)
			return
		}
		db.Exec("UPDATE room_members SET role = ? WHERE room_id = ? AND user_id = ?", req.Role, req.RoomID, targetID)
		logModAction(req.RoomID, uid, "Set Role", req.TargetUser, "Assigned role: "+req.Role)
		sendNotification(targetID, uid, "mod", "🛡️ Role Update", "Your role is now '"+req.Role+"' in room.", req.RoomID)

	case "warn":
		db.Exec("INSERT INTO room_warnings(room_id, user_id, reason, mod_id) VALUES(?, ?, ?, ?)", req.RoomID, targetID, req.Reason, uid)
		var strikeCount int
		db.QueryRow("SELECT COUNT(*) FROM room_warnings WHERE room_id = ? AND user_id = ?", req.RoomID, targetID).Scan(&strikeCount)
		logModAction(req.RoomID, uid, "Warning", req.TargetUser, fmt.Sprintf("Strike %d: %s", strikeCount, req.Reason))
		sendNotification(targetID, uid, "mod", "⚠️ Warning Issued", fmt.Sprintf("Strike %d/3: %s", strikeCount, req.Reason), req.RoomID)

		if strikeCount >= 3 {
			db.Exec("INSERT OR IGNORE INTO room_bans(room_id, user_id) VALUES(?, ?)", req.RoomID, targetID)
		}
	}

	w.Write([]byte(`{"ok":true}`))
}

// -------------------------------------------------------------
// DIRECT MESSAGES (UNIFIED INBOX)
// -------------------------------------------------------------
func handleDirectMessages(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	uid, myName, err := authenticate(r)
	if err != nil {
		http.Error(w, `{"err":"Unauthorized"}`, 401)
		return
	}

	if r.Method == http.MethodGet {
		targetUser := r.URL.Query().Get("u")
		if targetUser == "" {
			rows, err := db.Query(`
				SELECT u.username, u.country_code, u.last_active_at, dm.content, dm.created_at,
				       (SELECT COUNT(*) FROM direct_messages WHERE sender_id = u.id AND receiver_id = ? AND is_read = 0) as unread
				FROM direct_messages dm
				JOIN users u ON (dm.sender_id = u.id AND dm.receiver_id = ?) OR (dm.receiver_id = u.id AND dm.sender_id = ?)
				WHERE dm.id IN (
					SELECT MAX(id) FROM direct_messages WHERE sender_id = ? OR receiver_id = ? GROUP BY CASE WHEN sender_id = ? THEN receiver_id ELSE sender_id END
				)
				ORDER BY dm.id DESC
			`, uid, uid, uid, uid, uid, uid)
			if err != nil {
				http.Error(w, err.Error(), 500)
				return
			}
			defer rows.Close()

			type InboxItem struct {
				User    string `json:"u"`
				Country string `json:"c"`
				Online  bool   `json:"on"`
				LastMsg string `json:"msg"`
				Time    string `json:"d"`
				Unread  int    `json:"unread"`
			}

			items := make([]InboxItem, 0)
			for rows.Next() {
				var ii InboxItem
				var lastActive, createdAt time.Time
				rows.Scan(&ii.User, &ii.Country, &lastActive, &ii.LastMsg, &createdAt, &ii.Unread)
				ii.Online = time.Since(lastActive) < 60*time.Second
				ii.Time = formatTimeAgo(createdAt)
				items = append(items, ii)
			}
			json.NewEncoder(w).Encode(map[string]interface{}{"inbox": items})
			return
		}

		var targetID int64
		db.QueryRow("SELECT id FROM users WHERE username = ?", targetUser).Scan(&targetID)
		db.Exec("UPDATE direct_messages SET is_read = 1 WHERE sender_id = ? AND receiver_id = ?", targetID, uid)

		rows, _ := db.Query(`
			SELECT id, sender_id, content, media_url, created_at
			FROM direct_messages
			WHERE (sender_id = ? AND receiver_id = ?) OR (sender_id = ? AND receiver_id = ?)
			ORDER BY id DESC LIMIT 25
		`, uid, targetID, targetID, uid)
		defer rows.Close()

		type DMItem struct {
			ID    int64  `json:"id"`
			IsMe  bool   `json:"is_me"`
			Text  string `json:"t"`
			Media string `json:"m,omitempty"`
			Time  string `json:"d"`
		}

		msgs := make([]DMItem, 0)
		for rows.Next() {
			var dmi DMItem
			var senderID int64
			var createdAt time.Time
			rows.Scan(&dmi.ID, &senderID, &dmi.Text, &dmi.Media, &createdAt)
			dmi.IsMe = senderID == uid
			dmi.Time = createdAt.Format("15:04")
			msgs = append([]DMItem{dmi}, msgs...)
		}
		json.NewEncoder(w).Encode(map[string]interface{}{"messages": msgs})
		return
	}

	if r.Method == http.MethodPost {
		var req struct {
			ToUser string `json:"to"`
			Text   string `json:"t"`
			Media  string `json:"m"`
		}
		json.NewDecoder(r.Body).Decode(&req)

		var targetID int64
		db.QueryRow("SELECT id FROM users WHERE username = ?", req.ToUser).Scan(&targetID)
		if targetID == 0 {
			http.Error(w, `{"err":"User not found"}`, 404)
			return
		}

		db.Exec("INSERT INTO direct_messages(sender_id, receiver_id, content, media_url) VALUES(?, ?, ?, ?)", uid, targetID, req.Text, req.Media)
		sendNotification(targetID, uid, "dm", "✉️ Direct Message", "@"+myName+": "+req.Text, uid)
		w.WriteHeader(201)
		w.Write([]byte(`{"ok":true}`))
	}
}

// -------------------------------------------------------------
// FRIENDS & SMART SUGGESTIONS
// -------------------------------------------------------------
func handleFriends(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	uid, myName, err := authenticate(r)
	if err != nil {
		http.Error(w, `{"err":"Unauthorized"}`, 401)
		return
	}

	mode := r.URL.Query().Get("mode")

	switch mode {
	case "list":
		rows, _ := db.Query(`
			SELECT u.username, u.country_code, u.last_active_at, u.current_activity
			FROM friendships f
			JOIN users u ON (f.user_id = u.id AND f.friend_id = ?) OR (f.friend_id = u.id AND f.user_id = ?)
			WHERE f.status = 'accepted' AND u.id != ?
			ORDER BY u.last_active_at DESC
		`, uid, uid, uid)
		defer rows.Close()

		type FriendItem struct {
			User     string `json:"u"`
			Country  string `json:"c"`
			Online   bool   `json:"on"`
			Activity string `json:"act"`
		}

		friends := make([]FriendItem, 0)
		for rows.Next() {
			var fi FriendItem
			var lastActive time.Time
			rows.Scan(&fi.User, &fi.Country, &lastActive, &fi.Activity)
			fi.Online = time.Since(lastActive) < 60*time.Second
			friends = append(friends, fi)
		}
		json.NewEncoder(w).Encode(map[string]interface{}{"friends": friends})

	case "suggestions":
		rows, _ := db.Query(`
			SELECT u.username, u.country_code, u.last_active_at,
			       (SELECT COUNT(*) FROM room_members rm1 JOIN room_members rm2 ON rm1.room_id = rm2.room_id WHERE rm1.user_id = ? AND rm2.user_id = u.id) as shared_rooms
			FROM users u
			WHERE u.id != ?
			  AND u.id NOT IN (SELECT friend_id FROM friendships WHERE user_id = ? UNION SELECT user_id FROM friendships WHERE friend_id = ?)
			ORDER BY shared_rooms DESC, u.last_active_at DESC LIMIT 10
		`, uid, uid, uid, uid)
		defer rows.Close()

		type SugItem struct {
			User    string `json:"u"`
			Country string `json:"c"`
			Online  bool   `json:"on"`
			Reason  string `json:"reason"`
		}

		sugs := make([]SugItem, 0)
		for rows.Next() {
			var si SugItem
			var lastActive time.Time
			var sharedRooms int
			rows.Scan(&si.User, &si.Country, &lastActive, &sharedRooms)
			si.Online = time.Since(lastActive) < 60*time.Second
			if sharedRooms > 0 {
				si.Reason = fmt.Sprintf("Shares %d room(s) with you", sharedRooms)
			} else {
				si.Reason = "Active in your area"
			}
			sugs = append(sugs, si)
		}
		json.NewEncoder(w).Encode(map[string]interface{}{"suggestions": sugs})

	case "request":
		targetUser := r.URL.Query().Get("u")
		var targetID int64
		db.QueryRow("SELECT id FROM users WHERE username = ?", targetUser).Scan(&targetID)
		db.Exec("INSERT OR IGNORE INTO friendships(user_id, friend_id, status) VALUES(?, ?, 'pending')", uid, targetID)
		sendNotification(targetID, uid, "friend", "👥 Friend Request", "@"+myName+" sent you a friend request.", uid)
		w.Write([]byte(`{"ok":true}`))

	case "accept":
		targetUser := r.URL.Query().Get("u")
		var targetID int64
		db.QueryRow("SELECT id FROM users WHERE username = ?", targetUser).Scan(&targetID)
		db.Exec("UPDATE friendships SET status = 'accepted' WHERE user_id = ? AND friend_id = ?", targetID, uid)
		sendNotification(targetID, uid, "friend", "🎉 Request Accepted", "@"+myName+" accepted your friend request.", uid)
		w.Write([]byte(`{"ok":true}`))
	}
}

func handleSearch(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" {
		json.NewEncoder(w).Encode(map[string]interface{}{"users": []string{}, "rooms": []string{}})
		return
	}

	uRows, _ := db.Query("SELECT username, country_code, last_active_at FROM users WHERE username LIKE ? OR phone_number LIKE ? LIMIT 10", "%"+q+"%", "%"+q+"%")
	defer uRows.Close()

	type UserRes struct {
		User    string `json:"u"`
		Country string `json:"c"`
		Online  bool   `json:"on"`
	}
	uList := make([]UserRes, 0)
	for uRows.Next() {
		var ur UserRes
		var lastActive time.Time
		uRows.Scan(&ur.User, &ur.Country, &lastActive)
		ur.Online = time.Since(lastActive) < 60*time.Second
		uList = append(uList, ur)
	}

	rRows, _ := db.Query("SELECT id, name, topic FROM rooms WHERE name LIKE ? OR topic LIKE ? LIMIT 5", "%"+q+"%", "%"+q+"%")
	defer rRows.Close()

	type RoomRes struct {
		ID    int64  `json:"id"`
		Name  string `json:"name"`
		Topic string `json:"topic"`
	}
	rList := make([]RoomRes, 0)
	for rRows.Next() {
		var rr RoomRes
		rRows.Scan(&rr.ID, &rr.Name, &rr.Topic)
		rList = append(rList, rr)
	}

	json.NewEncoder(w).Encode(map[string]interface{}{"users": uList, "rooms": rList})
}

// -------------------------------------------------------------
// NOTIFICATIONS & MEDIA UPLOAD
// -------------------------------------------------------------
func handleNotifications(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	uid, _, err := authenticate(r)
	if err != nil {
		http.Error(w, `{"err":"Unauthorized"}`, 401)
		return
	}

	rows, _ := db.Query(`
		SELECT id, type, title, body, target_id, is_read, created_at
		FROM notifications WHERE user_id = ? ORDER BY id DESC LIMIT 20
	`, uid)
	defer rows.Close()

	type NotifItem struct {
		ID       int64  `json:"id"`
		Type     string `json:"type"`
		Title    string `json:"title"`
		Body     string `json:"body"`
		TargetID int64  `json:"tid"`
		Read     bool   `json:"read"`
		Time     string `json:"d"`
	}

	notifs := make([]NotifItem, 0)
	for rows.Next() {
		var ni NotifItem
		var isRead int
		var createdAt time.Time
		rows.Scan(&ni.ID, &ni.Type, &ni.Title, &ni.Body, &ni.TargetID, &isRead, &createdAt)
		ni.Read = isRead == 1
		ni.Time = formatTimeAgo(createdAt)
		notifs = append(notifs, ni)
	}

	db.Exec("UPDATE notifications SET is_read = 1 WHERE user_id = ?", uid)
	json.NewEncoder(w).Encode(map[string]interface{}{"notifications": notifs})
}

func handleUpload(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _, err := authenticate(r)
	if err != nil {
		http.Error(w, `{"err":"Unauthorized"}`, 401)
		return
	}

	file, header, err := r.FormFile("file")
	if err != nil {
		http.Error(w, `{"err":"No file provided"}`, 400)
		return
	}
	defer file.Close()

	ext := filepath.Ext(header.Filename)
	filename := fmt.Sprintf("%d_%s%s", time.Now().UnixNano(), generateToken()[:6], ext)
	out, err := os.Create("./uploads/" + filename)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	defer out.Close()
	io.Copy(out, file)

	mediaURL := "/uploads/" + filename
	json.NewEncoder(w).Encode(map[string]interface{}{"url": mediaURL})
}

// -------------------------------------------------------------
// SERVER MAIN ROUTER
// -------------------------------------------------------------
func main() {
	initDB()
	defer db.Close()

	http.Handle("/uploads/", http.StripPrefix("/uploads/", http.FileServer(http.Dir("./uploads"))))

	http.HandleFunc("/api/register", handleRegister)
	http.HandleFunc("/api/login", handleLogin)
	http.HandleFunc("/api/profile", handleProfile)
	http.HandleFunc("/api/profile/status", handleUpdateStatus)

	http.HandleFunc("/api/posts", handlePosts)
	http.HandleFunc("/api/like", handleLike)
	http.HandleFunc("/api/comments", handleComments)

	http.HandleFunc("/api/rooms", handleRooms)
	http.HandleFunc("/api/rooms/chat", handleRoomChat)
	http.HandleFunc("/api/rooms/typing", handleTyping)
	http.HandleFunc("/api/rooms/moderate", handleModerate)

	http.HandleFunc("/api/dm", handleDirectMessages)
	http.HandleFunc("/api/friends", handleFriends)
	http.HandleFunc("/api/search", handleSearch)
	http.HandleFunc("/api/notifications", handleNotifications)
	http.HandleFunc("/api/upload", handleUpload)

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	fmt.Printf("🚀 Retro Social Backend Live on port %s\n", port)
	if err := http.ListenAndServe(":"+port, nil); err != nil {
		log.Fatal(err)
	}
}