# Code Signing Policy

Free code signing is provided by [SignPath.io](https://signpath.io/), certificate
by [SignPath Foundation](https://signpath.org/).

## Scope

All Canticle release binaries published to GitHub Releases are signed
using the SignPath free open-source program. Signing applies to the final
binaries produced by the GoReleaser release pipeline.

## Release approval

Every release is manually reviewed and approved before signing. No automated
process can approve a signing request on its own. A human approver verifies
that the release tag corresponds to a reviewed, merged commit on `main` before
authorizing the signing step.

## Team member roles

**Authors** hold commit access to the repository and may push branches and open
pull requests without requiring an additional gating review:

- `sydlexius`

**Reviewers** inspect changes and provide feedback before merge. Automated
review bots (CodeRabbit, Codoki) assist human review by surfacing findings on
each pull request. All automated review output is assessed by a human before
merge.

**Approvers** authorize each signing request. A signing request is approved
only after the release has been reviewed and the approver confirms the binary
corresponds to the intended commit:

- `sydlexius`

## Privacy

Lookup data handling is described in the [Privacy Policy](privacy-policy.md).
No personal data is transmitted to SignPath beyond what is required for the
signing workflow. For details on what SignPath processes during signing, see the
[SignPath Foundation Privacy Policy](https://signpath.org/privacy).

## Cross-references

- [SignPath.io](https://signpath.io/) - code signing provider
- [SignPath Foundation](https://signpath.org/) - certificate authority for the free open-source program
- [Privacy Policy](privacy-policy.md) - data handling policy for Canticle
