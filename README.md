# Waitlist

Small Go binary that serves a waitlist form and SQLite-backed API.

## Commands

- `waitlist serve [-f path]` creates/opens the DB (`DATABASE_PATH` or `waitlist.db`) and listens on `:PORT` (`:8080` default) while serving `index.html`.
- `waitlist list [-f path] [--honeypot]` tab-prints rows from `waitlist` or `waitlist_honeypot`.
- `waitlist demo [-dir path]` seeds a temp SQLite file in `dir` then runs `serve` against it.

## API

- `POST /api/v1/waitlist` accepts JSON or form `email`; duplicate emails return 409, invalid addresses 400.
- Supplying `nickname` (hidden honeypot field) writes to `waitlist_honeypot` and still returns 201.
- Non-POST requests receive 405 with `Allow: POST`.

## Honeypot

Add a hidden `nickname` input to capture bots separately without blocking real users.
