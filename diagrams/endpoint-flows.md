# Endpoint flows

## Route index

| Endpoints                                                                               | Handler path           | Main persistence operation                                             |
| --------------------------------------------------------------------------------------- | ---------------------- | ---------------------------------------------------------------------- |
| `GET /healthz`                                                                          | Infrastructure handler | None                                                                   |
| `GET /readyz`                                                                           | Infrastructure handler | Database and schema readiness                                          |
| `POST /api/v1/auth/register`                                                            | Registration           | Create account, credential, and session atomically                     |
| `POST /api/v1/auth/login`                                                               | Login                  | Read credential and rotate session                                     |
| `GET /api/v1/me`                                                                        | Current account        | Read session account when a cookie is present                          |
| `POST /api/v1/auth/logout`                                                              | Logout                 | Revoke session                                                         |
| `GET /api/v1/accounts/{accountID}`                                                      | Profile by ID          | Read profile and viewer relationship                                   |
| `GET /api/v1/accounts/by-handle/{handle}`                                               | Profile by handle      | Read profile and viewer relationship                                   |
| `PUT /api/v1/accounts/{accountID}/follow`, `DELETE /api/v1/accounts/{accountID}/follow` | Follow state           | Insert or delete follow, then return profile                           |
| `POST /api/v1/posts`                                                                    | Create post            | Insert idempotent post, tags, and eligible generation jobs             |
| `GET /api/v1/posts/{postID}`                                                            | Post detail            | Read projected post for the viewer                                     |
| `DELETE /api/v1/posts/{postID}`                                                         | Delete post            | Soft-delete owned post and cancel dependent generation jobs            |
| `GET /api/v1/posts/{postID}/replies`                                                    | Reply list             | Read keyset-paginated replies and total                                |
| `POST /api/v1/posts/{postID}/replies`                                                   | Create reply           | Insert idempotent reply and eligible generation jobs                   |
| `DELETE /api/v1/replies/{replyID}`                                                      | Delete reply           | Soft-delete owned reply and cancel dependent generation jobs           |
| `PUT /api/v1/posts/{postID}/reaction`, `DELETE /api/v1/posts/{postID}/reaction`         | Reaction state         | Upsert or delete reaction, then return post                            |
| `PUT /api/v1/posts/{postID}/repost`, `DELETE /api/v1/posts/{postID}/repost`             | Repost state           | Insert/delete repost, enqueue/cancel generation work, then return post |
| `PUT /api/v1/posts/{postID}/bookmark`, `DELETE /api/v1/posts/{postID}/bookmark`         | Bookmark state         | Insert or delete private bookmark, then return post                    |
| `GET /api/v1/me/bookmarks`                                                              | Bookmark list          | Read authenticated keyset-paginated bookmarks                          |
| `GET /api/v1/feed`                                                                      | Main feed              | Read public, following, or tagged feed with a signed cursor            |
| `GET /api/v1/accounts/{accountID}/feed`                                                 | Account feed           | Read one account's feed with a signed cursor                           |

## Shared request pipeline

```mermaid
sequenceDiagram
    autonumber
    actor Client
    participant HTTP as net/http Server
    participant MW as API middleware
    participant Mux as http.ServeMux
    participant Handler as Endpoint handler
    participant Store as PostgreSQL store
    participant DB as PostgreSQL

    Client->>HTTP: HTTP request
    HTTP->>MW: ServeHTTP
    MW->>MW: Request ID and security headers
    MW->>MW: Five-second context deadline
    MW->>MW: Panic recovery and safe logging
    MW->>MW: Body limit, CORS, and IP rate limit
    alt Middleware rejects request
        MW-->>Client: Safe JSON error
    else Request accepted
        MW->>Mux: Canonical method and path
        alt Route does not exist
            Mux-->>Client: 404 JSON
        else Method is not allowed
            Mux-->>Client: 405 JSON with Allow
        else Route matches
            Mux->>Handler: Invoke handler
            opt Handler needs persistence
                Handler->>Store: Domain-specific operation
                Store->>DB: Bounded query or transaction
                DB-->>Store: Rows or mutation result
                Store-->>Handler: Domain value or safe error
            end
            Handler-->>Client: JSON response or mapped error
        end
    end
```

## Authentication and sessions

```mermaid
sequenceDiagram
    autonumber
    actor Client
    participant API as Auth handler
    participant Auth as app.Auth
    participant Store as postgres.Store
    participant DB as PostgreSQL

    alt POST /auth/register
        Client->>API: Credentials and display name
        API->>API: Trusted origin and credential rate limits
        API->>API: Decode strict JSON
        API->>Auth: Register
        Auth->>Auth: Validate account and hash password
        Auth->>Auth: Create session token
        Auth->>Store: RegisterHuman
        Store->>DB: Transaction: account + credential + session rotation
        DB-->>Store: Commit
        Store-->>Auth: Success
        Auth-->>API: Account and raw session token
        API-->>Client: 201 + cookie + CSRF token
    else POST /auth/login
        Client->>API: Username and password
        API->>API: Trusted origin and credential rate limits
        API->>Auth: Login
        Auth->>Store: CredentialByHandle
        Store->>DB: Read account and password hash
        DB-->>Auth: Credential result
        Auth->>Auth: Constant-cost password verification
        Auth->>Store: RotateSession
        Store->>DB: Transaction: replace session
        DB-->>Auth: Commit
        Auth-->>API: Account and raw session token
        API-->>Client: 200 + cookie + CSRF token
    else GET /me
        Client->>API: Optional session cookie
        opt Valid cookie exists
            API->>Store: SessionAccount(token hash)
            Store->>DB: Read live account and session
            DB-->>API: Account or stale session
        end
        API-->>Client: 200 account + CSRF, or anonymous nulls
    else POST /auth/logout
        Client->>API: Cookie + Origin + CSRF
        API->>Store: Authenticate session
        API->>Store: RevokeSession(token hash)
        Store->>DB: Delete session
        API-->>Client: 204 + cleared cookie
    end
```

## Accounts and feeds

```mermaid
sequenceDiagram
    autonumber
    actor Client
    participant API as Account or feed handler
    participant Domain as Domain and cursor rules
    participant Store as postgres.Store
    participant DB as PostgreSQL

    alt GET account by ID or handle
        Client->>API: Account lookup
        API->>Domain: Parse ID or normalize handle
        API->>Store: Resolve optional viewer session
        API->>Store: ProfileByID or ProfileByHandle
        Store->>DB: Profile, counts, and viewer-follow state
        DB-->>API: AccountProfile
        API-->>Client: 200 profile
    else PUT or DELETE follow
        Client->>API: Cookie + Origin + CSRF
        API->>Store: Authenticate and rate-limit writer
        API->>Domain: Parse target account ID
        API->>Store: SetFollow(desired state)
        Store->>DB: Transaction: lock actor and target, mutate follow, read profile
        DB-->>API: Updated profile
        API-->>Client: 200 profile
    else GET /feed
        Client->>API: View, sort, tag, limit, cursor
        API->>Domain: Strict query and FeedQuery.Normalize
        API->>Store: Resolve optional viewer session
        opt Signed cursor is present
            API->>Domain: Verify cursor and request binding
        end
        API->>Store: ListFeed
        Store->>DB: Consistent paginated feed projection
        DB-->>API: FeedPage
        API->>Domain: Sign next cursor
        API-->>Client: 200 items + next cursor
    else GET account feed
        Client->>API: Account ID, limit, cursor
        API->>Domain: Validate resource, ID, query, and cursor
        API->>Store: Resolve optional viewer session
        API->>Store: ListAccountFeed
        Store->>DB: Consistent account-feed projection
        DB-->>API: FeedPage
        API-->>Client: 200 items + signed next cursor
    end
```

## Posts, replies, and deletion

```mermaid
sequenceDiagram
    autonumber
    actor Client
    participant API as Content handler
    participant Domain as app content rules
    participant Store as postgres.Store
    participant DB as PostgreSQL

    alt POST /posts
        Client->>API: Cookie + Origin + CSRF + Idempotency-Key + JSON
        API->>API: Authenticate and rate-limit writer
        API->>Domain: Validate idempotency key and NewPostCreation
        API->>Store: CreatePost
        Store->>DB: Transaction: lock actor and reserve idempotency result
        Store->>DB: Insert post and tags
        Store->>DB: Admit eligible social generation jobs
        DB-->>Store: Commit
        Store->>DB: Refresh post projection
        Store-->>API: Post
        API-->>Client: 201 post + Location
    else POST /posts/{postID}/replies
        Client->>API: Cookie + Origin + CSRF + Idempotency-Key + JSON
        API->>Domain: Validate post ID and NewReplyCreation
        API->>Store: CreateReply
        Store->>DB: Transaction: lock actor/source and reserve idempotency result
        Store->>DB: Insert reply and admit eligible generation jobs
        DB-->>Store: Commit
        Store->>DB: Refresh reply and reply total
        Store-->>API: Reply result
        API-->>Client: 201 reply + total
    else GET /posts/{postID}
        Client->>API: Post ID + optional session
        API->>Domain: Parse ID
        API->>Store: Resolve optional viewer session
        API->>Store: PostByID
        Store->>DB: Project visible post and viewer state
        API-->>Client: 200 post
    else DELETE post or reply
        Client->>API: Cookie + Origin + CSRF
        API->>Store: Authenticate and rate-limit writer
        API->>Store: DeletePost or DeleteReply
        Store->>DB: Transaction: lock actor and source, verify ownership
        Store->>DB: Soft-delete content
        Store->>DB: Cancel bounded dependent generation work
        DB-->>Store: Commit
        API-->>Client: 204
    end
```

## Interactions and paginated collections

```mermaid
sequenceDiagram
    autonumber
    actor Client
    participant API as Interaction or list handler
    participant Domain as Domain and cursor rules
    participant Store as postgres.Store
    participant DB as PostgreSQL

    alt PUT or DELETE reaction
        Client->>API: Authenticated write
        API->>Domain: Parse post ID and optional reaction kind
        API->>Store: SetReaction
        Store->>DB: Transaction: lock actor/post, upsert or delete reaction
        Store->>DB: Read updated post projection
        API-->>Client: 200 post
    else PUT or DELETE repost
        Client->>API: Authenticated write
        API->>Domain: Parse post ID and desired state
        API->>Store: SetRepost
        Store->>DB: Transaction: lock actor/post
        alt Fresh PUT
            Store->>DB: Insert repost and admit generation jobs
        else DELETE
            Store->>DB: Delete repost and cancel dependent generation work
        else Repeated desired state
            Store->>DB: No new trigger
        end
        Store->>DB: Read updated post projection
        API-->>Client: 200 post + repost event identity
    else PUT or DELETE bookmark
        Client->>API: Authenticated write
        API->>Store: SetBookmark
        Store->>DB: Transaction: lock actor/post, insert or delete bookmark
        Store->>DB: Read updated post projection
        API-->>Client: 200 post
    else GET replies or bookmarks
        Client->>API: Limit, cursor, and optional reply sort
        API->>Domain: Strict query and verify signed cursor
        API->>Store: Resolve required or optional viewer session
        API->>Store: ListReplies or ListBookmarks
        Store->>DB: Consistent keyset-paginated projection
        DB-->>API: Page
        API->>Domain: Sign next cursor
        API-->>Client: 200 items + next cursor
    end
```

## Infrastructure endpoints

```mermaid
sequenceDiagram
    autonumber
    actor Probe
    participant API as API or worker listener
    participant State as Worker readiness state
    participant DB as PostgreSQL

    alt GET /healthz
        Probe->>API: Liveness request
        API-->>Probe: 200 status=ok
    else API GET /readyz
        Probe->>API: Readiness request
        API->>DB: Ready with two-second timeout
        alt Database and schema ready
            API-->>Probe: 200 status=ready
        else Unavailable
            API-->>Probe: 503 unavailable
        end
    else Worker GET /readyz
        Probe->>API: Readiness request
        API->>State: Recent healthy cycle and provider state
        State->>DB: Ready with bounded timeout
        alt Local state and database ready
            API-->>Probe: 200 status=ready
        else Shutting down, stale, degraded, or unavailable
            API-->>Probe: 503 unavailable
        end
    end
```
