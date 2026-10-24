# gh-replicate

A GitHub CLI extension that replicates Git repositories and selected GitHub metadata from a source repository to an existing destination repository.

## What it replicates

`gh replicate SOURCE TARGET` synchronizes:

- branches and tags, overwriting matching destination refs while preserving destination-only refs;
- GitHub Releases and their uploaded assets;
- repository description and homepage;
- topics;
- labels.

The destination repository must already exist. Public and private repositories are supported when they are accessible to the current GitHub CLI authentication.

Issues, pull requests, discussions, workflows, permissions, secrets, variables, rulesets, branch protection, Actions artifacts, and GitHub Packages are intentionally out of scope.

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
gh replicate SOURCE TARGET
```

For example:

```sh
gh replicate vmvarela/project vmvarela/project-replica
```

The destination repository must already exist. Matching source branches, tags, releases, assets, metadata, topics, and labels are synchronized. Destination-only branches, tags, releases, assets, and labels are preserved.

Running the command again is safe: unchanged release assets are detected and skipped.

## Status

The project is under initial development. The first release will be v0.1.0.

## License

MIT
