# J2ME Social App Backend

Ultra-lightweight Go backend with SQLite designed for J2ME (MIDP 2.0) mobile clients.

## Endpoints
- `POST /api/register` - Create account (`{"user":"...","pass":"..."}`)
- `POST /api/login` - Authenticate (`{"user":"...","pass":"..."}`)
- `GET /api/posts` - Fetch latest 10 posts
- `POST /api/posts` - Create post (Requires `X-Token` header)
