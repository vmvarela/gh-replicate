# gh-replicate

Copy a GitHub repository into another one that already exists: branches, tags, releases with their assets, and the main repository metadata, from a single command in GitHub CLI.

Use it to keep a mirror, a staging copy or a public fork in sync with a repository you control.

## What it copies

- Branches and tags. Matching destination refs are overwritten; destination-only refs are kept.
- Git LFS objects referenced by spec-v1 pointers (see Limits).
- Releases, their uploaded assets and the Latest selection.
- Description, homepage and topics.
- Labels.

It does not touch issues, pull requests, discussions, wikis, permissions, secrets, variables, rulesets, branch protection, Actions artifacts or Packages. Release authors and publication dates are not copied.

## Requirements

- GitHub CLI installed and authenticated with `gh auth login`.
- `git` 2.29 or newer in `PATH`.
- `git-lfs` 3.8.1 or newer in `PATH` — required only when the source contains Git LFS pointers.
- A destination repository that already exists and that you can write to. Changing its default branch and disabling its Actions needs administration write access.
- github.com only. GitHub Enterprise Server and cross-instance replication are not supported.

## Install

```sh
gh extension install vmvarela/gh-replicate
```

To upgrade or remove it later:

```sh
gh extension upgrade replicate
gh extension remove replicate
```

To run a local build instead:

```sh
git clone https://github.com/vmvarela/gh-replicate.git
cd gh-replicate
go build -o gh-replicate .
gh extension install .
```

## Use

```sh
gh replicate [--dry-run] [--components LIST] [--timeout 30m] [--source-account LOGIN] [--target-account LOGIN] SOURCE TARGET
```

Both repositories use `OWNER/REPO`. Start with a dry run, which reads the source and reports what it would change without writing to the destination:

```sh
gh replicate --dry-run vmvarela/project vmvarela/project-replica
gh replicate vmvarela/project vmvarela/project-replica
```

A real run with `refs` selected (the default) attempts to disable the destination's GitHub Actions before pushing. If that fails, it warns and continues, so the push may start workflows; already running workflows are not cancelled. With `metadata` selected it also sets the destination default branch to the source's. Without `refs`, Actions are never touched.

Running the command again is normal and safe. Matching refs are force-pushed as one atomic update, unchanged release assets are detected by digest and skipped, and content that exists only in the destination is preserved. Git refs, metadata and releases are not one global transaction, so a failure partway through can leave some metadata unsynchronised; run the command again to finish.

### Choosing components

`--components` takes a comma-separated list of `refs`, `metadata` (description, homepage, topics and default branch), `labels` and `releases`; the default, also used for an empty `--components ''`, is all four. Dry runs and real runs print which are selected and which are skipped. `releases` requires `refs` (release tags must exist in the destination) and is rejected before any write otherwise. Actions are only touched when `refs` is selected, and the default branch is changed only with `metadata`. Preflight still reads every component, so unreadable data can stop a run even if skipped. `metadata` without `refs` fails if the destination lacks the source default branch.

```sh
gh replicate --components labels,metadata owner/src owner/dst
```

### Replicating between two accounts

Each side can use a different account that is already logged in on your machine:

```sh
gh auth login --hostname github.com   # once per account
gh replicate --source-account alice --target-account bob alice/project bob/project-replica
```

Both flags are optional and independent; without them the active account is used. The tokens come from the accounts `gh` already stores, so the active account, the stored credentials and your Git configuration are not modified. Each side's login is verified before anything is written, and a mismatch stops the run.

## Limits

- **Check the direction.** Source wins for matching refs. A dry run reports what would change; it does not guarantee that server-side protections will accept the push.
- **Ref renames can be blocked.** Replacing `release` with `release/v2` fails while `release` still exists in the destination, because destination-only refs are never deleted. Delete or rename it, then rerun.
- **Git LFS support covers spec-v1 pointers smaller than 1024 bytes.** Their objects are downloaded, verified and uploaded before any ref is published, using `git-lfs` 3.8.1 or newer. The three historical version aliases (`git-lfs`, `hawser`, `git-media`), CRLF line endings, surrounding whitespace, blank lines and `ext-*` extensions are accepted. Pointers 1024 bytes or larger (padded), a zero or malformed size, an unknown key, or a newer `git-lfs.github.com` version stop the run before any destination change, including pointers only in history. The scan reads every blob reachable from branches and tags, so it can be slow on large histories (raise `--timeout`), and it is conservative: an ordinary file containing a known LFS version line may be reported too.
- **Some assets need a second step.** A changed release asset is uploaded under a temporary name, verified, then swapped in, so its name is briefly missing. An interrupted run leaves either the previous asset or the verified replacement; a rerun finishes the work, and a leftover temporary asset may need deleting from the release.
- **Invalid input stops the run before any write.** More than 20 topics, an empty release tag or a duplicated release tag, including drafts, abort the replication. Releases are matched by tag.
- **Retries are bounded.** Read requests retry twice for selected transient failures and short rate limits; write requests are never replayed automatically, so rerun instead.
- **`--timeout 30m` applies per Git command or HTTP request**, not to the whole replication. Raise it for large repositories or assets. Ctrl-C cancels pending work.

## Local cache

A per-source lock lives under your OS user cache directory (`gh-replicate/git-v1`) so a second run of the same source fails with the lock path until the first exits. Source history is no longer reused across runs: each run copies into a fresh, private, temporary repository with its own configuration, so no partial state is ever reused. Cleanup is best effort — a hard kill (SIGKILL) can leave a temporary directory behind — and a failed LFS upload can leave objects on the destination without new refs. You can delete the cache directory while no replication is running.

## License

MIT
