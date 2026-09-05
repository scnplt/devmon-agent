---
name: git-commit
description: Stage and commit changes in this repo following conventional-commit format and the project's gates from CLAUDE.md plus a secret scan. Use when the user asks to commit, "commit this", "save my work to git", or after finishing a unit of work that should land as a commit.
trigger: /git-commit
---

# /git-commit

Turn the current working-tree changes into one or more well-scoped conventional commits,
after the project's quality gates pass.

Message format and PR conventions come from `.claude/rules/ecc/common/git-workflow.md`;
this skill is the executable procedure around them.

## Usage

```
/git-commit                      # commit everything that belongs together
/git-commit <description>        # plain-English targeting: "commit the config loader changes"
/git-commit --amend              # only when the user explicitly asks; never amend pushed commits
```

## Workflow

### 1. Inspect before staging

Run these in parallel — never commit blind:

```bash
git status
git diff                 # unstaged
git diff --staged        # already staged
git log --oneline -10    # match the repo's message style
git branch --show-current
```

### 2. Branch check

If the current branch is `main` **or `dev`**, **stop and create a feature branch
first**, cut from `dev`:

```bash
git checkout dev && git pull
git checkout -b <type>/<short-kebab-description>
```

Never commit directly to `main` or `dev`. Both are protected: `main` is
production/release and `dev` is the integration branch, and every change reaches
either one through a PR. See `CLAUDE.md` and
`.claude/rules/ecc/common/git-workflow.md` for the full branching model.

### 3. Quality gates

Run every gate listed in `CLAUDE.md` under **Gates (MANDATORY)** — that section is the only
gate list; this skill does not keep its own copy. All must pass before committing. If any
fails, fix it — do not commit around it and do not use `--no-verify`. Skip a gate only if
the tool is not installed, and say so explicitly in the final report.

### 4. Secret scan

Reject the commit if the diff contains any of:

- Hardcoded API keys, passwords, tokens, or connection strings
- PEM blocks, private keys, certificates, or pairing codes
- `.env` files or anything matching `*.key`, `*.pem`, `*.p12`
- Debug leftovers: stray `fmt.Println`, `log.Printf("DEBUG…")`, commented-out code blocks

This repo's rule is absolute: **never log key material, pairing codes, or PEM bytes at
any level.** A diff that adds such logging is a blocking finding, not a nit.

### 5. Stage deliberately

Stage only the files that belong to this logical change:

```bash
git add <specific paths>
```

Avoid `git add -A` unless the whole tree is genuinely one change. If the working tree
holds two unrelated changes, make two commits.

Never stage: `coverage.out`, `bin/`, editor files, or anything `.gitignore` should cover
(fix `.gitignore` instead).

### 6. Message format

```
<type>: <imperative description under 72 chars>

<body: why the change was made, not what the diff already shows>
<wrap at 72 columns; omit entirely for trivial changes>
```

Types: `feat`, `fix`, `refactor`, `docs`, `test`, `chore`, `perf`, `ci`

Rules:
- Imperative mood — "add mTLS handshake", not "added" or "adds"
- No trailing period on the subject
- Body explains **why**; the diff already says what
- Reference issues as `Refs #123` / `Closes #123` on their own line
- Append the `Co-Authored-By:` trailer unless `~/.claude/settings.json` sets
  `"includeCoAuthoredBy": false`. The harness fills in the model name; never hardcode one.

Use a heredoc so multi-line messages survive the shell:

```bash
git commit -m "$(cat <<'EOF'
feat: add mTLS client-certificate verification to the control API

The Docker socket must never be reachable without a pinned client cert.
Verification happens in the TLS handshake so unauthorized peers never
reach the mux.

Co-Authored-By: <model name>
EOF
)"
```

In PowerShell, use a single-quoted here-string (`@'` … `'@`, closing delimiter at column 0)
instead of a heredoc.

### 7. Verify

```bash
git status        # confirm the intended files landed
git log -1 --stat
```

## Guardrails

- **Do not push** unless the user asked. Committing ≠ pushing.
- **Do not amend or rebase** commits that already exist on a remote.
- **Do not use** `--no-verify`, `--no-gpg-sign`, or interactive flags (`-i` does not work here).
- **Prefer a new commit** over amending, unless the user explicitly asks to amend.
- If gates fail, report the actual failing output verbatim — never claim a clean run
  that did not happen.

## Examples

| Change | Message |
|--------|---------|
| New env var parsing | `feat: read DEVMON_LISTEN_ADDR at startup` |
| Nil deref on empty container list | `fix: guard against empty container list in inspect handler` |
| Extract TLS setup into its own file | `refactor: move TLS config into internal/transport` |
| Table-driven tests for the config loader | `test: cover config loader defaults and validation` |
| Bump moby client | `chore: bump github.com/moby/moby to v29.1.0` |
