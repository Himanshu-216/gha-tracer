# Security policy

## Reporting a vulnerability

Please **do not open a public issue**. Report it privately through
GitHub: **Security → Report a vulnerability** on this repository. You should get
a reply within a few working days.

## What's in place

**Service**
- `POST /webhook` accepts only requests whose `X-Hub-Signature-256` HMAC matches
  `GITHUB_WEBHOOK_SECRET` (constant-time compare). The secret must be ≥ 16 bytes;
  use `openssl rand -hex 32`.
- Request bodies are capped at 25 MB, and duplicate deliveries are dropped.
- HTTP servers set read, write, idle and header timeouts and a 64 KB header limit.
  Responses carry `nosniff`, `DENY` framing, `default-src 'none'` CSP and `no-store`.
- `GITHUB_API_URL` must be `https` (plain `http` is allowed only for loopback), so
  App JWTs and installation tokens are never sent in clear text.
- `/metrics` listens only on `INTERNAL_ADDR`. Keep that port private (VPC / security group).
- The GitHub App needs only **Actions: read** and **Metadata: read**.

**Image**
- Distroless `static` base: no shell or package manager. It runs as uid 65532.
- Base images are pinned by digest. The build is static (`CGO_ENABLED=0`), trimmed and
  checks module checksums (`go mod verify`).
- Published images are signed with cosign (keyless) and carry SLSA provenance and an SBOM.

**CI** (`.github/workflows`)
- `golangci-lint` (gosec, govet, staticcheck, errcheck, …), `go test -race`.
- `govulncheck` (Go stdlib + dependencies), Grype image scan, hadolint, gitleaks
  (full history), actionlint, CodeQL (Go + Actions), OpenSSF Scorecard, and
  dependency review on PRs.
- All actions are pinned to commit SHAs, and workflows default to `contents: read`.
- Dependabot keeps Go modules, base images and actions up to date.
