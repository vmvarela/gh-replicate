# gh-replicate

A GitHub CLI extension that replicates Git repositories and selected GitHub metadata from a source repository to an existing destination repository.

## What it replicates

`gh replicate SOURCE TARGET` synchronizes:

- branches and tags, overwriting matching destination refs while preserving destination-only refs;
- GitHub Releases and their uploaded assets;
- repository description and homepage;
- topics;
- labels.

The destination repository must already exist. Public and private repositories are supported when they are accessible to the account used for each side.

Issues, pull requests, discussions, Actions run history and settings, permissions, secrets, variables, rulesets, branch protection, Actions artifacts, and GitHub Packages are intentionally out of scope.

## Installation

### Requirements

Install and authenticate GitHub CLI first:

```sh
gh auth login
```

`git` must also be available in your PATH.

### Install the extension

Once the first release is published, install the latest version directly with GitHub CLI:

```sh
gh extension install vmvarela/gh-replicate
```

GitHub CLI will use the precompiled binary from the latest release when available.

To upgrade later:

```sh
gh extension upgrade replicate
```

To remove it:

```sh
gh extension remove replicate
```

### Install from source during development

Clone the repository, build the extension binary, and install the local checkout:

```sh
git clone https://github.com/vmvarela/gh-replicate.git
cd gh-replicate
go build -o gh-replicate .
gh extension install .
```

## Usage

Both repositories use `OWNER/REPO` notation:

```sh
gh replicate [--dry-run] [--timeout 30m] [--source-account LOGIN] [--target-account LOGIN] SOURCE TARGET
```

For example:

```sh
gh replicate vmvarela/project vmvarela/project-replica
```

The destination repository must already exist. Matching source branches, tags, releases, assets, metadata, topics, and labels are synchronized. Destination-only branches, tags, releases, assets, and labels are preserved.

Running the command again is safe: unchanged release assets are detected and skipped.

### Replicating between two accounts

Each side can use a different account that is already authenticated on this machine:

```sh
gh auth login --hostname github.com   # repeat for the second account
gh auth status                        # both accounts must be listed
gh replicate --source-account alice --target-account bob alice/project bob/project-replica
```

Both flags are optional and independent: without them the globally active account is used, exactly as before. The stored token of a requested account is read with `gh auth token --user`; the active account, the `gh` credential storage and the global Git configuration are never modified.

Before cloning or writing anything, the run reports the account each side authenticates as:

```text
Source identity: alice (gh auth token --user alice)
Target identity: bob (gh auth token --user bob)
```

When both sides resolve to the same account it prints a single line. An account that is not logged in fails with `no oauth token found for github.com account <login>`, and a side whose token authenticates as a different login aborts before any change. If the account exists but cannot read or write a repository, the API or Git step fails with GitHub's error and replication stops there.

## Operational behavior and limits

- **github.com only.** GHES and cross-instance replication are not supported. Set `GH_HOST=github.com` when another host is your default.
- **Source wins for matching refs.** Double-check the direction. Use `gh replicate --dry-run SOURCE TARGET` first: it reads metadata, clones locally, checks LFS and runs `git push --dry-run`, but makes no destination changes. It reports planned reconciliation, not a guarantee that server-side protections will accept a real push.
- **Atomic Git push.** All branch/tag updates succeed together or the push fails. There is no fallback to a partial push and no pruning. Git refs, GitHub metadata and releases do not form one global transaction; failures after the push may leave partial metadata replication. Rerun to reconcile.
- **No global Git configuration changes.** Authentication uses the GitHub CLI credential helper scoped to each Git subprocess.
- **One account per side.** `--source-account` and `--target-account` select already-authenticated `gh` accounts; each side's token goes to its own API client and Git subprocess, and both logins are verified with `GET /user` before the clone. Nothing is written to `gh` auth state or global Git configuration.
- **LFS is not copied.** A preflight scan of the cloned Git objects rejects LFS pointers, including historical ones, before destination changes. This scan adds work on large repositories.
- **Preflight validation.** More than 20 combined topics, empty release tags or duplicate release tags (including drafts) stop replication before writes. No topics are silently dropped.
- **Latest is preserved.** The source's explicitly selected Latest release is applied after releases/assets succeed. If the source has no Latest, destination-only releases are preserved and no Latest selection is imposed.
- **Timeout and cancellation.** `--timeout 30m` sets the limit per Git command or HTTP request, not for the entire replication. Use a larger duration for large repositories/assets. Ctrl-C cancels pending work.
- **Bounded read retries.** GET requests retry at most twice for selected transient HTTP failures and rate limits, respecting server delays up to 30 seconds. Longer rate limits fail rather than retry early. Mutating requests are never replayed automatically after uncertain failures; rerun the command to reconcile.
- **Asset replacement is not atomic.** Source downloads are checked for size and SHA-256 before a matching destination asset is deleted. Unchanged assets are detected by digest; when a digest is missing, equal-sized destination content is downloaded and hashed, so repeated runs preserve asset IDs at the cost of an extra download. Upload responses are checked for size and digest. A failed upload can still leave that asset absent until a successful rerun.
- Original release authors and publication dates are not preserved. Discussions, permissions and Actions settings are not replicated. Workflow files themselves are part of Git history and are copied with refs.

## Release binaries

Publishing a release runs `.github/workflows/release.yml`: tests, vet and vulnerability checks precede compilation/upload with `cli/gh-extension-precompile`. Release Drafter only prepares release notes. CI separately checks Linux, macOS and Windows builds for amd64 and arm64. No release is published by opening or merging a PR.

## Status

The project is under initial development. The first release will be v0.1.0.

## License

MIT

