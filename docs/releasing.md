# Releasing

How a SatBooth release is made. Operators need only `deploy/README.md` ("Release images"), `SECURITY.md` ("Release
signing key") and the threat model's §6; this page is the maintainer's procedure. A release is three container images
(`satbooth`, `satbooth-relay` and `satbooth-tools`, each for `linux/amd64` and `linux/arm64`) published to GHCR, a
signed annotated tag and a GitHub release. The tag is `v<major>.<minor>.<patch>`, or
`v<major>.<minor>.<patch>-rc.<n>` for a release candidate; the workflow and every release script refuse any other name.

The deploy bundles pin the images by `tag@digest` (`deploy/public/compose.yml`, `deploy/hub/compose.yml` and the
`tools_image=` line of `deploy/setup.sh`), so the pins of a release must be in the tree that the tag names. That is why
the digests are computed first, in a pull request, and the tag is made afterwards on the merged commit.

## The release signing key

Release tags are signed with the OpenPGP key whose fingerprint is `5BEB DE79 EDDE D015 7398 1D4F 70C6 001B 0A49 776E`,
published at `https://github.com/fishcakeday.gpg`. Operators import it, compare the full fingerprint and run `git tag
-v <tag>` (`SECURITY.md`, "Release signing key"). A change of the key is announced by a commit signed with the old key.

## Before the first release

The release workflow names the environment `release`. A workflow that names an environment that does not exist gets
one created with no protection, so these settings must exist and be read back before the first tag is pushed. An
organisation owner does this once.

1. **The `release` environment** (the repository's Settings, Environments):
   - required reviewers: the maintainer;
   - "Prevent self-review": off, because the maintainer both tags and approves;
   - deployment branches and tags: "Selected branches and tags", with one rule of type tag, `v*`, and none of type
     branch;
   - "Allow administrators to bypass configured protection rules": off.
2. **Two tag rulesets** (Settings, Rules, Rulesets), both on the target "Tag" with the pattern `refs/tags/v*`, and both
   with enforcement "Active":
   - one that restricts deletion and restricts updates, with an empty bypass list;
   - one that restricts creation, with the repository-admin role as its only bypass actor.
3. **Read both back** and compare with the lists above:

   ```sh
   gh api repos/SatBooth/SatBooth/environments/release
   gh api repos/SatBooth/SatBooth/environments/release/deployment-branch-policies
   gh api repos/SatBooth/SatBooth/rulesets
   gh api repos/SatBooth/SatBooth/rulesets/<id>
   ```

   The environment must show the reviewer, `prevent_self_review` false, `can_admins_bypass` false and a custom policy
   whose one entry is of type `tag` and named `v*`. Each ruleset must show `enforcement` `active`, the `refs/tags/v*`
   condition, its rules and its bypass actors as above.
4. **Immutable releases.** Step 6 of the procedure relies on a published release being locked. Turn on the repository's
   immutable-releases setting (Settings, General, Releases) and read it back:

   ```sh
   gh api repos/SatBooth/SatBooth/immutable-releases
   ```

   It must show `"enabled": true`.

## Procedure

1. **Release-prep pull request.** Build the images into a directory outside the checkout (the build writes large files)
   and write their digests into the three pin sites:

   ```sh
   out=$(mktemp -d)
   scripts/release-build.sh "$out"
   scripts/release-pins.sh set <version> "$out/digests.txt"
   ```

   Run the build from a clean checkout that is up to date with `main`: CI builds the pull request's merge commit, and
   an untracked `.go` file would enter the build context and change the digests.

   `<version>` is the tag, `v` included. Commit the three changed lines. The `agree` job of `release.yml` runs on the
   pull request, because it changes the pins: it builds the images on an amd64 and an arm64 runner, requires both to
   produce the same digests, and fails the pull request when they differ from the pins. A pull request that leaves the
   pins as they are gets the comparison as a note only. `agree` is not a required check of `main`: merge the
   release-prep pull request only when `agree` is green.

2. **Dry run.** Merge the pull request, then run the `release` workflow by hand (workflow dispatch) on `main`. It does
   everything up to the comparison with the pins and pushes nothing. It must be green on the exact commit that will be
   tagged.

3. **The tag.** The maintainer makes the annotated tag on that commit, signed with the release signing key, and pushes
   it. From the commit being tagged, first check that the pins hold exactly the tag name:

   ```sh
   git switch --detach <commit>
   scripts/release-pins.sh version     # must print exactly <tag>
   git tag -s <tag> -m "SatBooth <tag>" <commit>
   git tag -v <tag>
   git push origin <tag>
   ```

   The workflow does not verify the signature: it runs from the tagged commit, so a check written there would stop no
   one who controls that tree. It checks that the tag is annotated, named as above and at the run's commit. Who can
   make the workflow publish is decided by the settings of "Before the first release" and by the operators' own
   `git tag -v`.

4. **Approve the `release` environment.** The tag push starts the run: `build` and `agree` as in the dry run, this time
   with the tag's version compared against the pins; `publish` runs only if `agree` passed, and waits for the approval.
   Before approving, read the three `match` lines in the summary of `agree`. A refused approval pushes nothing.
   `publish` rebuilds the images, checks them against the pins again, pushes them to the three packages by digest,
   points the tag `<version>` at each, and attests each image. `release-push.sh` never moves a registry tag that holds
   another digest that was there when it read the tag; it fails instead. It reads a tag and creates it a moment
   later, so a concurrent writer with package-write rights (threat model §6) could still create the tag in between;
   operators who pull by digest are unaffected. Then `smoke` pulls the published images on an amd64 and an arm64
   runner and runs `scripts/image-run-test.sh` on them; only when it passes on both architectures does `draft` create a
   draft release (a prerelease for an rc) from `scripts/release-notes.sh <tag>`, whose "Changes:" placeholder is one
   line to replace.

5. **Check that the packages are public.** GitHub's documentation says that a package published by a workflow takes the
   visibility of the repository, so the three packages should be public. The first release confirms which case applies.
   If a package is not public, an organisation owner sets it public in the package's settings. Making a package public
   cannot be undone, and package administrators can also change its visibility, so look at all three before the
   check. Then, logged out of the registry and from a clean checkout of the tag, pull and run the images as an operator
   would:

   ```sh
   docker logout ghcr.io
   git tag -v <tag>
   git switch --detach "refs/tags/<tag>^{commit}"
   git rev-parse HEAD                  # must equal the next line
   git rev-parse "refs/tags/<tag>^{commit}"
   scripts/release-smoke.sh <tag>
   ```

   It requires the registry tag to hold the pinned digest, pulls the three images by `tag@digest` and runs them. Do this
   before publishing the draft: the `smoke` job logs in, so it does not prove that an anonymous operator can pull. Once
   the packages are public, a later release needs this check only to confirm it.

6. **Publish.** Publishing is the maintainer's step and comes last: only after the `smoke` job is green on both
   architectures and after the anonymous check of step 5. Edit the draft's prose (the line "Changes: (to be written
   before publishing)"), then publish it. The release notes already hold the three pinned refs and the verification
   commands. Once the release is published, GitHub locks its tag and assets; the title and notes stay editable, so they
   are not authoritative: the signed tag's tree is.

7. **The next release.** After an rc, the next rc or the final release is another prep pull request that changes the
   version only. When no build input changed (the Go sources, `go.mod`, `vendor/`, the files `checkout` embeds,
   `deploy/image/Dockerfile`, `.dockerignore`, and the pins of the builders and the exporter settings in
   `scripts/release-lib.sh`), the digests stay the same and `release-pins.sh set` writes the same digests under the new
   version.

## When a run is refused

If `agree` fails, the pins and the build disagree: nothing is pushed. If the approval is refused, nothing is pushed;
reject the waiting deployment, so that it cannot be approved later.

A failure that is not a disagreement (a network error, a runner that stopped, a failed call to the registry or to the
attestation service) is retried on the same run with "Re-run failed jobs"; `publish`, when it is one of them, asks for
the approval again. It is safe to repeat: it pushes the same digests, leaves a registry tag that already holds the
pinned digest, and fails on one that holds another. `draft` runs only after `smoke` passed on both architectures, and
leaves an existing release as it is.

When the cause is in the tagged tree (a wrong pin, an error in a script or in the workflow, an image that fails
`smoke`), the answer is the next version: fix the cause in a pull request, then tag that version. Never move or delete
a tag: the tag rulesets forbid it, and an operator who verified the old tag must keep seeing the same commit under it.
A tag that was pushed and did not publish stays, unreleased; images already pushed for it keep their registry tag, and
no release names them.

## Between releases

The pins on `main` name the last release, while the code around them may be newer. Deploy from a release tag, not from
`main`.
