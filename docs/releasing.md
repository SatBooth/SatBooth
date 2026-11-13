# Releasing

How a SatBooth release is made. Operators need only `deploy/README.md` ("Release images") and the threat model's §6;
this page is the maintainer's procedure. A release is three container images (`satbooth`, `satbooth-relay` and
`satbooth-tools`, each for `linux/amd64` and `linux/arm64`) published to GHCR, a signed annotated tag and a GitHub
release. The tag is `v<major>.<minor>.<patch>`, or `v<major>.<minor>.<patch>-rc.<n>` for a release candidate; the
workflow and every release script refuse any other name.

The deploy bundles pin the images by `tag@digest` (`deploy/public/compose.yml`, `deploy/hub/compose.yml` and the
`tools_image=` line of `deploy/setup.sh`), so the pins of a release must be in the tree that the tag names. That is why
the digests are computed first, in a pull request, and the tag is made afterwards on the merged commit.

## Procedure

1. **Release-prep pull request.** Build the images and write their digests into the three pin sites:

   ```sh
   scripts/release-build.sh out
   scripts/release-pins.sh set <version> out/digests.txt
   ```

   `<version>` is the tag, `v` included. Commit the three changed lines. The `agree` job of `release.yml` runs on the
   pull request, because it changes the pins: it builds the images on an amd64 and an arm64 runner, requires both to
   produce the same digests, and fails the pull request when they differ from the pins. A pull request that leaves the
   pins as they are gets the comparison as a note only.

2. **Dry run.** Merge the pull request, then run the `release` workflow by hand (workflow dispatch) on `main`. It does
   everything up to the comparison with the pins and pushes nothing. It must be green on the exact commit that will be
   tagged.

3. **The tag.** The maintainer makes the annotated tag on that commit, signed with the release key, and pushes it:

   ```sh
   git tag -s <tag> -m "SatBooth <tag>" <commit>
   git tag -v <tag>
   git push origin <tag>
   ```

   The workflow does not verify the signature: it runs from the tagged commit, so a check written there would stop no
   one who controls that tree. It checks that the tag is annotated, named as above and at the run's commit. Who can
   make the workflow publish is decided by three controls outside the tree: the `release` environment (the maintainer
   must approve it, deployments are limited to `v*` tags, administrators cannot bypass it), a tag ruleset on
   `v*` (no update, no deletion, creation only by the repository-admin role), and the operators' own `git tag -v`, with
   the maintainer's public key from a source other than this repository's hosting.

4. **Approve the `release` environment.** The tag push starts the run: `build` and `agree` as in the dry run, this time
   with the tag's version compared against the pins; `publish` runs only if `agree` passed, and waits for the approval.
   Before approving, read the three `match` lines in the summary of `agree`. `publish` rebuilds the images, checks them against the pins again,
   pushes them to the three packages by digest, points the tag `<version>` at each, and attests each image. Then
   `draft` creates a draft release (a prerelease for an rc) from `scripts/release-notes.sh <tag>`, and `smoke` pulls the
   published images on an amd64 and an arm64 runner and runs `scripts/image-run-test.sh` on them.

5. **Check that the packages are public.** GitHub's documentation says that a package published by a workflow takes the
   visibility of the repository, so the three packages should be public. The first release confirms which case applies.
   If a package is not public, an organisation owner sets it public in the package's settings. Then, logged out of the
   registry, pull and run the images as an operator would:

   ```sh
   docker logout ghcr.io
   scripts/release-smoke.sh <tag>
   ```

   It requires the registry tag to hold the pinned digest, pulls the three images by `tag@digest` and runs them. Do this
   before publishing the draft: the `smoke` job logs in, so it does not prove that an anonymous operator can pull. Once
   the packages are public, a later release needs this check only to confirm it.

6. **Publish.** Publishing is the maintainer's step and comes last: only after the `smoke` job is green on both
   architectures and after the anonymous check of step 5. Edit the draft's prose (the placeholder under "Changes"), then
   publish it. The release notes already hold the three pinned refs and the verification commands. A release is
   immutable once published, so what the draft says must be right before it is.

7. **The next release.** After an rc, the next rc or the final release is another prep pull request that changes the
   version only. When no build input changed (the Go sources, `go.mod`, `vendor/`, the files `checkout` embeds,
   `deploy/image/Dockerfile`, and the pins of the builders), the digests stay the same and `release-pins.sh set` writes
   the same digests under the new version.

## When a run is refused

If `agree` fails, the pins and the build disagree: nothing is pushed. If `publish` fails before the push, nothing is
pushed. If the run is refused or fails after some images were pushed, the digests in the registry are the pinned ones
and `release-push.sh` leaves a tag that holds another digest as a failure. In every case the answer is the next rc: fix
the cause in a pull request and tag the next version. Never move or delete a tag: the ruleset forbids it, and an
operator who verified the old tag must keep seeing the same commit under it. A tag that was pushed and did not publish
stays, unreleased.

## Between releases

The pins on `main` name the last release, while the code around them may be newer. Deploy from a release tag, not from
`main`.
