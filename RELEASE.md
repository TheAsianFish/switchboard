# Releasing Switchboard

A release is a `v*` tag. Pushing one runs `.github/workflows/release.yml`,
which tests the tagged commit, attaches four archives and `SHA256SUMS` to
a GitHub release, and pushes a linux/amd64 + linux/arm64 image to
`ghcr.io/theasianfish/switchboard:<tag>` and `:latest`. A tag with a
`-` (`v1.1.0-rc.1`) is published as a pre-release and does not move
`:latest`. The image is built only after the archives and the GitHub
release succeed, so a failed release never leaves an image behind. Nothing in it is
paid and it needs no secret beyond the repository's own `GITHUB_TOKEN`.

## One-time setup (maintainer)

1. **Make the repository public** (Settings → General → Danger Zone →
   Change visibility). `go install …@latest` fetches from GitHub through
   the public module proxy, which cannot read a private repository, and
   the README quickstart starts with `git clone`.
2. **Check that Actions may write releases and packages.** The workflow
   asks for `contents: write` (the release) and `packages: write` (the
   image) per job, which a personal repository allows whatever the default
   under Settings → Actions → General → Workflow permissions. Only an
   organisation policy that caps the token at read-only blocks it.
3. **After the first image is pushed, make the package public.** GHCR
   creates `switchboard` as a private package owned by the account. Open
   it (profile → Packages → switchboard → Package settings), set the
   visibility to public, and under "Manage Actions access" check that the
   `switchboard` repository has write access (it does when the workflow
   created the package).
4. **Protect release tags.** Any collaborator who can push a `v*` tag can
   publish. Settings → Rules → Rulesets → New tag ruleset, target
   `v*`, restrict creations, updates and deletions to the maintainer.
5. Optional, for the live evals only: add `ANTHROPIC_API_KEY` as a
   repository secret (Settings → Secrets and variables → Actions). No
   workflow on push uses it; RESULTS.md "To measure" lists what it would
   run and the estimated cost.

## Before tagging

From a clean checkout of `main` at the commit to release:

```sh
make check                 # gofmt, vet, golangci-lint, go test -race
make test-integration      # Lua and vector index against redis:8
make ui-check              # the admin page and its committed build
make results && git diff --exit-code RESULTS.md README.md   # rendered docs are current
make check-install         # go install in a clean golang container serves a request
VERSION=v1.0.0 make dist   # the four archives build (GNU tar; gtar on macOS); dist/ is gitignored
```

Then dry-run the workflow itself: Actions → release → "Run workflow" on
`main`. A manual run builds the archives and both image platforms exactly
as a tag would, uploads the archives as a workflow artifact for 7 days,
and publishes nothing. It must be green before the tag is pushed.

Also check by hand:

- `PLAN.md` has every milestone ticked and RESULTS.md's "To measure" is
  what is really left;
- the README quickstart still works from a fresh clone (`git clone`, then
  the commands under "Quickstart");
- no secret is tracked: `git ls-files | grep -Ei '\.env|secret'` prints
  nothing, and `git grep -nE 'sk-ant-api[0-9]{2}-'` finds no real
  Anthropic key.

## Tagging

```sh
git checkout main && git pull --ff-only
git tag -a v1.0.0 -m "Switchboard v1.0.0"
git push origin v1.0.0
```

Watch the run under Actions → release. When it is green:

- the release at `https://github.com/TheAsianFish/switchboard/releases/tag/v1.0.0`
  has `switchboard_1.0.0_{linux,darwin}_{amd64,arm64}.tar.gz` and
  `SHA256SUMS`, with notes generated from the merged pull requests (edit
  them to lead with the headline results if you like);
- `docker pull ghcr.io/theasianfish/switchboard:v1.0.0` works on amd64
  and arm64, and `docker run --rm ghcr.io/theasianfish/switchboard:v1.0.0
  version` prints `v1.0.0`;
- `go install github.com/TheAsianFish/switchboard/cmd/switchboard@v1.0.0`
  works from any machine with Go 1.27, and `switchboard version` prints
  `v1.0.0` (the module proxy may take a few minutes to see a new tag).

## If something goes wrong

- **The workflow failed before publishing** (tests or a build): fix on
  `main`, delete the tag locally and on GitHub (`git tag -d v1.0.0 &&
  git push origin :refs/tags/v1.0.0`) and tag the fixed commit.
- **The release was published but is broken:** do not move the tag (the
  module proxy and checksum database have already recorded it; a moved
  tag makes `go install` fail its checksum). Mark the GitHub release as a
  pre-release or delete it, delete the image tag in the package settings,
  and release `v1.0.1`. If the module version itself must not be used,
  add a `retract v1.0.0` directive to `go.mod` in `v1.0.1`.
- **The archives and the GitHub release are out but the image push was
  refused** (`denied: permission_denied`): the package's "Manage Actions
  access" lacks this repository (setup step 3), or an organisation policy
  caps the token (step 2). Fix it and re-run the failed `image` job; the
  tag stays where it is.

## Versioning

Semantic versioning on the tag. The version a binary reports comes from
the tag: archives and images are stamped with `-X main.buildVersion`,
and `go install module@vX.Y.Z` reports the module version.
