# Security policy

## Reporting a vulnerability

Report a vulnerability privately, through GitHub's private vulnerability reporting on this repository: open the
Security tab and press "Report a vulnerability". Please do not open a public issue or a public pull request for it.

Include:

- what is affected: the package, file or document, and the version or commit;
- what an attacker needs (network position, a key, access to a host) and what they gain;
- the steps to reproduce it, or a failing test;
- any fix you have in mind.

Never include a real key, seed, password or API token in a report.

SatBooth's threat model, with what it assumes and what it does and does not protect, is in
[`docs/threat-model.md`](docs/threat-model.md).

## Release signing key

Every release tag (`v<major>.<minor>.<patch>`, or `-rc.<n>` for a release candidate) is an annotated tag signed with the
OpenPGP key whose fingerprint is

```
5BEB DE79 EDDE D015 7398 1D4F 70C6 001B 0A49 776E
```

The key is published at `https://github.com/fishcakeday.gpg`. Before you trust a release, import it, compare the full
fingerprint with the one above, and verify the tag:

```sh
curl -fsSL https://github.com/fishcakeday.gpg | gpg --import
gpg --fingerprint '5BEB DE79 EDDE D015 7398 1D4F 70C6 001B 0A49 776E'
git tag -v <tag>
```

`git tag -v` must report a good signature from that fingerprint. Then, in a clean checkout, select the tag's commit
with `git switch --detach "refs/tags/<tag>^{commit}"` (not `git checkout <tag>`, which takes a local branch of the same
name first) and check that `git rev-parse HEAD` equals `git rev-parse "refs/tags/<tag>^{commit}"`. A change of the
release signing key is announced by a commit signed with the old key, which states the new fingerprint.
