# Git Workflow

## Branching Model

| Branch | Role | Merged from |
|--------|------|-------------|
| `main` | Production / release. Only released code. | `dev` -> `main` (release PR) or `hotfix/*` -> `main` |
| `dev` | Integration / development branch. Default base branch. | `feat/*`, `fix/*`, ... -> `dev` PR |
| `<type>/<slug>` | A single feature or fix. Short-lived. | Deleted after merge |

Rules:

- **Every feature lives on its own branch.** Never commit directly to `dev` or `main`.
- Feature branches are **cut from `dev`** and return to **`dev`** through a PR:
  ```bash
  git checkout dev && git pull
  git checkout -b feat/<slug>
  # ... commits ...
  git push -u origin feat/<slug>
  gh pr create --base dev
  ```
- Branch names reuse the commit types: `feat/`, `fix/`, `refactor/`, `docs/`, `test/`,
  `chore/`, `perf/`, `ci/`. Example: `feat/pairing-endpoint`, `fix/tls-handshake-timeout`.
- **Release:** `dev` -> `main` PR. A merge into `main` means "releasable" and is tagged
  `vX.Y.Z`.
- **Hotfix:** only when production is broken — cut `hotfix/<slug>` from `main`, merge into
  `main`, then merge `main` back into `dev` so the fix is not lost.
- Delete merged feature branches locally and on the remote.
- Treat `main` and `dev` as protected: no force push, no merge without a PR.

## Commit Message Format
```
<type>: <description>

<optional body>
```

Types: feat, fix, refactor, docs, test, chore, perf, ci

Note: To disable co-author attribution on commits, set `"includeCoAuthoredBy": false` in `~/.claude/settings.json` (Claude Code appends `Co-Authored-By` by default; ECC does not ship this setting).

## Pull Request Workflow

When creating PRs:
1. Analyze full commit history (not just latest commit)
2. Use `git diff [base-branch]...HEAD` to see all changes
3. Draft comprehensive PR summary
4. Include test plan with TODOs
5. Push with `-u` flag if new branch

> For the full development process (planning, TDD, code review) before git operations,
> see [development-workflow.md](./development-workflow.md).
