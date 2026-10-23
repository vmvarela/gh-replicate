# gh-replicate

A GitHub CLI extension that replicates Git repositories and selected GitHub metadata from a source repository to an existing destination repository.

## Scope

`gh replicate SOURCE TARGET` synchronizes:

- source branches and tags into matching destination refs without deleting destination-only refs;
- GitHub Releases and uploaded release assets;
- repository description, homepage, topics, and labels;
- public and private repositories accessible to the current GitHub CLI authentication.

The destination repository must already exist. Issues, pull requests, discussions, workflows, permissions, secrets, variables, rulesets, and branch protection are intentionally out of scope.

## Status

The project is under initial development. The first release will be v0.1.0.

## License

MIT
