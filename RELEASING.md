# Releasing ieee-2030_5-client-go

Sections 2 to 5, 7 to 9 and 11 to 15 are shared doctrine: they read
identically in the `RELEASING.md` of all four repositories in this family
(`ieee-2030_5-core-go`, `ieee-2030_5-server-go`, `ieee-2030_5-client-go`, and
`gridappsd-ieee-2030_5-go`, the bridge), so a releaser moving between
repositories is not re-learning the process. Sections 1, 6, 10 and 16 hold
this repository's own facts. Section numbers mean the same thing in all four.

## 1. This repository and its release shape

Module path: `github.com/GRIDAPPSD/ieee-2030_5-client-go`

The IEEE 2030.5 DER client and inverter simulator. The binary is
`inverterclient`, built from `cmd/inverterclient`. It CONSUMES
`ieee-2030_5-core-go` and is consumed by no other repository in this family
(section 10).

Release shape: a Go module with a command binary and no tag-driven release
workflow. A release is a tag plus `gh release create`, cut by hand following
section 11.

**Read section 16 before cutting anything here.** This repository has an
unresolved licensing-posture question that is a gate on releasing it at all,
not a documentation cleanup task. It is the one thing about client-go that
does not resemble its siblings.
## 2. Versioning

Every module in this family is 0.x. Semver's 0.x carve-out applies: MINOR
carries any change that is not a strict patch, including a breaking one, and
MAJOR is not in play until a 1.0 commitment is made.

The version is not chosen by feel. It is read off the classification built in
section 3, by this rule:

- Any entry categorised `feature` or `breaking` forces at least MINOR.
- A range whose entries are all `bug fix`, `documentation`, `test` or `chore`
  may be PATCH.

State the version WITH that classification as its justification, in the
release notes, so a reader can check the arithmetic instead of trusting it.
"MINOR because the range adds two routes and removes one" is a justification.
"MINOR" on its own is not.

Worked examples from this family, all correctly MINOR:

- an additive release that introduced a new store contract and required no
  consumer change;
- a release that changed an exported field's type from a pointer to a
  complexType, which is breaking, and was still MINOR because the family has
  not committed to 1.0 semantics;
- a release that moved a served address and added a new write surface.

## 3. Classify the FULL commit range since the previous tag

Before choosing a version, enumerate every merge since the previous tag, not
only the change that prompted the release.

```
git fetch origin --tags
git log --first-parent --oneline vPREV..origin/main
```

Classify every merge in that range into the categories in section 5. The
version follows from the whole range's classification, per section 2. Do this
before writing the notes, because the table in section 5.1 IS the version
justification.

This step exists because its absence already cost a correction in public. A
release was cut as a PATCH on the strength of the one change its releaser had
in mind, while the range since the previous tag also carried new routes, a new
exported type and behaviour changes. It was published before anyone re-read
the range, and the notes had to be corrected after the fact. A correction
appended to published notes reaches nobody who already read them.

If the range is empty there is nothing to release. If it contains merges you
did not author, read them: "I only changed one thing" is a statement about
your own work, not about the range.

## 4. First release of a module

A module with no meaningful prior version has no range to diff against, so
section 3's `vPREV..origin/main` does not apply. Use this instead.

1. Confirm there is genuinely no prior release:
   ```
   gh release list --limit 20
   git tag --list
   ```
   A repository can carry tags that are not releases: import markers, backup
   points, phase snapshots. Those are not a prior version and do not seed the
   numbering. A repository with tags but no published release is still a first
   release.
2. Choose `v0.1.0` unless there is a specific reason not to. Do not start at
   `v1.0.0`: that is a stability commitment this family has not made, and it
   cannot be walked back once a consumer has resolved it.
3. Build the section 5 table over the module's history, but do NOT list every
   merge since the repository was created. List the merges a consumer needs to
   know about in order to adopt the module at all, and say so in a line above
   the table: "First release; the table covers the surface a consumer adopts,
   not the full commit history."
4. The Difference column is still mandatory. It reads differently for a first
   release: the difference is measured against having no dependency on this
   module at all, so each cell states what an adopter gains and what it must
   now supply (configuration, certificates, environment variables, a build
   step).
5. Everything else in this document applies unchanged: the same gates, the
   same licensed-material check, the same post-release verification.

## 5. Release notes: the required structure

Release notes are the human record of a release. They are required and they
have a fixed shape. Read a published example before writing your own: the
`v0.13.0` notes on `ieee-2030_5-core-go`, at
https://github.com/GRIDAPPSD/ieee-2030_5-core-go/releases/tag/v0.13.0

Every release's notes contain the following, in this order.

### 5.1 The commits table

One row per merged pull request. Per-merge, not per-commit: a pull request
routinely carries a version bump, a fix and a test as three separate commits,
and listing those separately is noise that buries the one line a reader needs.
The card link carries the detail.

```
### Commits in this release

| Merge | Card | Category | Change | Difference for consumers |
|---|---|---|---|---|
| [<sha>](<commit url>) ([#<pr>](<pr url>)) | <CARD-ID> | <categories> | <what changed> | <what a consumer observes now that they did not before> |
```

Immediately below the table, restate the category set and the version rule, so
the table explains itself to a reader who has never seen this document:

```
Category set: `feature`, `bug fix`, `breaking`, `refactor`, `documentation`,
`test`, `chore`. `breaking` stacks on a primary category rather than replacing
it. Any `feature` or `breaking` entry forces at least MINOR under the 0.x
carve-out, which is how the version below was determined.
```

### 5.2 Categories: a closed set

`feature`, `bug fix`, `breaking`, `refactor`, `documentation`, `test`,
`chore`.

The set is closed, and aligned to the conventional-commit prefixes these
repositories already use, so a merge's own commit message usually names its
category. If a change appears not to fit, that is a signal to split the row,
not to invent a category.

### 5.3 Three rules that make the table load-bearing rather than decorative

**Rule 1: categories drive the version.** Any `feature` or `breaking` entry
forces at least MINOR under the 0.x carve-out. A range whose entries are all
`bug fix`, `documentation`, `test` or `chore` may be PATCH. Enumerating the
range and determining the version are ONE procedure, not two: the table is the
version justification, which is why section 3 runs before a version is chosen.
A releaser who fills in the table honestly cannot then pick a version that
contradicts it.

**Rule 2: `breaking` stacks on a primary category rather than replacing it.**
Write `feature, **breaking**`, not one or the other. A worked case from this
family: one change mounted the WADL-declared LogEvent addresses, which is a
`feature`, and removed the previous `/log` addresses, which is `breaking`.
Forcing a single category loses half the story, and there it was the removal
that broke two consumers, so the half that would have been lost is the half
that mattered. A row may equally read `bug fix, **breaking**` or
`refactor, **breaking**`.

**Rule 3: the Difference column is mandatory and must be non-empty.** It is
what a consumer reads before deciding whether to bump, and it is the field
most likely to be skipped, because the difference is obvious to the person who
just made the change and to nobody else. The contrast, from the same worked
case:

- CHANGE: "mounts `/edev/{id}/lel` and derives `LogEventListLink` on every
  EndDevice read path".
- DIFFERENCE: "any ACL rule, test or client that names `log` now matches
  nothing and fails closed, so LogEvent posting stops working with no error
  that points at the cause".

Only the second tells a consumer to act. Write the Difference cell as what
somebody else observes, not as what you did.

If the honest answer is that nothing observable changes, write that: "no
observable change; internal refactor with identical wire output" is a valid
cell. An empty cell is never valid, because an empty cell cannot be told apart
from a cell nobody thought about.

### 5.4 The rest of the notes

After the table:

- The version and its justification, referencing the table (section 2).
- Breaking changes, each under its own heading, with downstream impact named
  per section 9.
- Added or changed behaviour that needs more than a table cell to explain.
- A **Verified** section stating what was actually built and run, and against
  what, including which env-gated suites were armed and which tests in them
  ran (section 7). A claim that "no consumer needs to change" is stated as
  verified only where something was actually executed; otherwise say what was
  reasoned and what was not.
- Findings that were reported and deliberately NOT fixed in this release, with
  a tracking reference where one exists.
- A **Deployment** section: the tag, the commit SHA it resolves to, and either
  "no Docker image or deploy step; consumers pin the module version in their
  own `go.mod`" for a library release, or the concrete artifact and deployment
  path for a repository that ships one.
## 6. Verification gates before tagging (client-go)

Run all of these on the exact commit you intend to tag:

```
go build ./...
go vet ./...
gofmt -l .
go test -count=1 ./...
go test -race -count=1 ./...
```

Client-go has no Makefile-driven gates, no coverage floor and no conformance
harness of its own. It is the plainest of the four to verify, which makes it
the easiest one to under-verify: there is no `make lint` failing loudly to
remind you that something was not run.

**Take a gate inventory rather than assuming there are none.** Client-go
exercises the core schema gate wherever its tests validate messages, and its
own suites may skip on conditions of their own:

```
grep -rn 'SEP2_SCHEMA_PATH\|SEP2_WADL_PATH\|t\.Skip\|testing\.Short' \
  --include='*.go' .
```

That command is this repository's gate inventory. Run it against the commit
being released, not from memory, because a suite added since the last release
is exactly the one nobody knows to arm. Every skip condition it reports that
could be reached under the release's run conditions is a section 7 case: arm
it, confirm zero SKIP, and name the tests in the notes.

Client-go connects to a server over mutual TLS, so some of its useful
verification is not a unit test at all: running `inverterclient` against a
live server exercises registration, DERControl polling and reporting in a way
`go test` does not. Where that was done, name the server and version it ran
against. Where it was not done, say so rather than implying the binary was
exercised.
## 7. Env-gated test suites: confirm they RAN, and name the tests

Several suites in this family are gated behind an environment variable and
SKIP silently when it is unset. A skipped gate and a passing gate report
success indistinguishably: `go test ./...` is green either way, and a releaser
reading only the exit code learns nothing.

This is not a hypothetical. A wire-format regression reached a published
release because the gate that would have caught it skipped, and the release
notes recorded that run as green.

Before tagging, for every env-gated suite this repository has (section 6 lists
them):

1. Run it with the gate ARMED, not merely with the variable set. Where a
   `*_REQUIRED` style variable exists, set it too, so a missing or
   misconfigured input fails the run instead of skipping it.
2. Read the output and confirm zero SKIP results among the gated tests.
   Contrast it against a run with the variable unset, which should show the
   full skip. If the two runs look the same, the gate is not armed and neither
   run proved anything.
3. Name the tests that ran, in the release notes' Verified section. "Schema
   gate passed" is not evidence. "All `TestSchemaGate*` in `pkg/sep2` passed,
   including the new cases this release adds, with zero SKIP" is evidence,
   because a reader can check it.

A releaser who does not have the gated input available should say exactly that
in the notes rather than reporting a green run that proves nothing. An honest
"not run, input unavailable" is useful; a green tick over a skip is worse than
no line at all.

## 8. Licensed material must never ship, and the check must prove it looked

The IEEE 2030.5 normative XML Schema (`sep.xsd`) and normative WADL
(`sep_wadl.xml`) are licensed material. Neither may appear in a commit, in a
tag's tree, in a release artifact, or in any binary built from these
repositories. Both are supplied at test time from outside the checkout, via
environment variable. Where this repository carries a `NOTICE` file, it holds
the full rationale.

Check the tagged tree, not the working tree you tagged from:

```
git fetch origin --tags
git ls-tree -r vX.Y.Z --name-only > tagged-files.txt
echo "files scanned: $(wc -l < tagged-files.txt)"
echo "matches:       $(grep -icE 'sep\.xsd|sep_wadl\.xml' tagged-files.txt || true)"
```

**Report both numbers, and report them together.** A zero-match result from a
command that scanned zero files is not a check, it is a false green, and that
exact failure happened during a release in this family. It was caught only
because the file count was printed beside the match count.

- Zero matches against a plausible file count is a pass.
- Zero matches against a file count of zero means the command was pointed at
  nothing, most often a tag that was never fetched or a typo in the ref. It is
  a FAIL. Fix the ref and run it again.
- A non-zero match count stops the release.

Do not resolve a non-zero match by moving the tag (section 13). Resolve it by
not publishing, fixing the tree and tagging a new version.

A security classifier flagging a release for licensed material is the system
working as intended, not a false alarm to route around. Running this check
before publishing is what makes answering such a flag cheap.

## 9. Downstream impact

Release notes name the downstream impact of every breaking change, per known
consumer, by name. "Breaking" on its own leaves each consumer to work out
whether it is affected, which in practice means most of them will not.

For each known consumer, state one of:

- it does not reference the changed surface, so no change is needed, AND how
  that was determined (a grep, a build, a test run);
- it does reference it, at a named file and line, with a tracking reference;
- it was built and tested against the new version, and passed.

**When a module has no established external consumers**, this requirement does
not evaporate, and silence does not satisfy it. Do this instead:

1. State it plainly in the notes: "No known external consumers as of this
   release." A reader must be able to tell "we checked and there are none"
   apart from "nobody checked".
2. Say how you know. The check is cheap: search the sibling repositories'
   `go.mod` files for this module path, and record the result rather than the
   impression.
3. Describe the impact on a hypothetical adopter anyway, in the same terms:
   what a consumer that DID depend on the changed surface would have to do.
   That is what the Difference column already asks for, and it is what keeps
   the notes useful on the day somebody does adopt the module, which is
   precisely the day nobody will re-derive it.
4. In-repo consumers count. A change that breaks this repository's own binary,
   its own test harness, its own example code or its own deployment
   configuration is a downstream impact and is named the same way.
## 10. Who consumes this module

No repository in this family imports `ieee-2030_5-client-go`. It is a leaf on
the import graph.

Section 9's no-established-consumers path therefore applies to every client-go
release. State plainly in the notes that there are no known external
consumers, say how that was checked, and describe the impact on a hypothetical
adopter anyway.

Verify rather than assume, each release:

```
gh api "repos/GRIDAPPSD/<repo>/contents/go.mod" --jq .content \
  | base64 -d | grep ieee-2030_5-client-go
```

**The real consumers of this repository are people running the binary**, not
modules importing it. That changes what belongs in the Difference column: the
observable surface is command-line flags, the `--role` selection, certificate
and trust-store expectations, configuration keys, the scenario timeline the
simulator drives, and what the client puts on the wire. A change that alters
the meaning of a flag or a configuration key is breaking for an operator even
though no exported Go identifier moved, and it is written up as breaking.

**In-repo consumers count** as well: `cmd/inverterclient` and anything under
`scripts/` are downstream of a change to an internal package.

**Client-go consumes core**, pinned by exact version. Read the current pin
rather than any number written down; pins are deliberately not recorded here
because a recorded pin goes stale within days:

```
grep ieee-2030_5-core-go go.mod
```

Expect this pin to lag core, potentially by several releases. State in every
client-go release which core version it was built and tested against, and if
the release includes a core bump, say what moved and what that changed on the
wire.
## 11. Cutting the release

1. Confirm authorization (section 14).
2. Confirm the section 6 gates are green on the exact commit you intend to
   tag, and that the section 7 env-gated suites actually ran.
3. Classify the full range and determine the version (section 3, or section 4
   for a first release).
4. Write the notes (section 5) BEFORE tagging. Writing them first is what
   surfaces a misclassified range while it is still free to fix; writing them
   after means discovering the mistake at a point where the only honest remedy
   is another version.
5. Tag:
   ```
   git tag -a vX.Y.Z -m "vX.Y.Z"
   git push origin vX.Y.Z
   ```
   A lightweight tag (`git tag vX.Y.Z`) is also acceptable; the release in the
   next step is what makes it a full release either way. Tag history in this
   family mixes both kinds, which is why section 12's dereference step is
   written the way it is.
6. Run the section 8 licensed-material check against the pushed tag.
7. Publish the release:
   ```
   gh release create vX.Y.Z --notes-file <path>
   ```
8. Run the section 12 post-release verification before telling anyone the
   release is done.

**A tag is not a release.** Both are required and they serve different
readers. The tag is the machine-facing half: it is what `go get` resolves, and
without it a consumer cannot depend on the version at all. The GitHub release
is the human record: it is where the section 5 notes live, and without it a
consumer can fetch the code but has no way to learn what changed or whether to
act. Stopping after `git push origin vX.Y.Z` leaves a version consumers can
silently pick up with no notes attached to it, which is the worst of the two
halves rather than half the job.

## 12. Post-release verification (mandatory, in this order)

1. **The tag dereferences to the intended commit.**
   ```
   git fetch origin --tags
   git rev-parse "vX.Y.Z^{commit}"
   ```
   Use `^{commit}`. A bare `git rev-parse vX.Y.Z` on an ANNOTATED tag returns
   the tag object's own SHA, which is not the commit SHA and will not match
   the HEAD you meant to tag. On a LIGHTWEIGHT tag the two happen to be the
   same value, which is exactly what makes the annotated case easy to miss: a
   check that was only ever tried against a lightweight tag looks correct and
   is not. `^{commit}` dereferences both kinds uniformly and is the only form
   that answers "what commit does this tag actually point at".

   Compare the result against the commit you intended to tag AND against the
   SHA written into the notes' Deployment section. All three must agree.
2. **The release is not a draft.**
   ```
   gh release view vX.Y.Z --json isDraft
   ```
   `isDraft: true` is a fail. A draft is invisible to anyone not already
   looking for it, so a drafted release is a tag with no human record: the
   same failure section 11 describes, arriving by a different route.
3. **The asset list is what you expect.**
   ```
   gh release view vX.Y.Z --json assets
   ```
   For a library release with nothing to attach, an empty list is correct, and
   a non-empty one is worth reading closely: a stray licensed file is exactly
   the kind of thing that must never be an asset (section 8). For a repository
   that ships an artifact, confirm the expected artifact is present and that
   nothing else is.
4. **The licensed-material check passes against the pushed tag**, with both
   numbers reported (section 8).

## 13. A published tag is immutable

Never move, re-cut, force-push or delete a tag that has been pushed.

The reason is specific to Go consumption. A tag is what `go get` resolves, and
a consumer that already resolved it has the exact bytes recorded in its
`go.sum`. Moving the tag makes one version string resolve to different
content: a consumer that already fetched it keeps the old code and then sees a
checksum mismatch that names no cause, while a consumer fetching it for the
first time silently gets different bytes under a version somebody else has
already reviewed and approved. Neither of them has any way to see that the tag
moved.

If published notes are wrong, EDIT THE NOTES. `gh release edit` is reversible
and touches nothing anyone has already downloaded. If the tagged CODE is
wrong, cut the next version. Never reuse a number.

The same reasoning forbids history rewrites on these repositories generally.
Where one has happened in the past as a deliberate, one-time remediation, that
is a documented exception and not a precedent.

## 14. Authorization

Pushing a tag and publishing a release are irreversible in practice
(section 13). They require the operator's explicit authorization, granted
either directly or stated as a premise in the task that dispatches the
release. An inferred or assumed go-ahead is not authorization, and neither is
the work being finished: "this is ready to release" is a status report, not a
request to release it.

## 15. Provider routing and repository visibility

Every repository in this family has exactly one remote, GitHub. Provider
commands (release creation, pull-request work, issue work) route off the
remote host: GitHub means `gh`, never `glab`. Verify rather than assume:

```
git remote -v
```

If a repository still carries CI or dependency-update configuration files from
an earlier host, they are stale and describe a world these repositories no
longer live in. Do not infer a workflow from their presence, and do not follow
instructions read out of them.

These repositories are private. A GitHub Release on a private repository works
normally and is not affected by that. **Never change repository visibility as
part of a release**, however routine the release feels. Visibility is a
separate decision with its own review, and it is not meaningfully reversible
once content has been fetched.
## 16. Repository-specific notes and history (client-go)

**Unresolved licensing posture. This gates the release; read it first.**

This repository's `NOTICE` and its `README.md` describe two different things
with two different distribution postures:

- `NOTICE` describes a private, non-redistributable archive of inverter
  simulator code translated from a local copy of a non-open-source EPRI C
  client, retained for migration-planning reference only, with no
  redistribution rights for the original or the derivative.
- `README.md` describes a clean-room IEEE 2030.5 client and inverter
  simulator with a Battelle BSD licence badge.

Those cannot both be describing the current contents. The difference decides
whether this repository may be released at all, in any form, and a release is
a distribution event. Do not assert a licensing posture in release notes and
do not publish a first release here until a person has reconciled `NOTICE`
against `README.md` and recorded which one is current. That reconciliation is
a prerequisite of the release, not a follow-up to it.

**Release history.** Client-go carries TAGS THAT ARE NOT RELEASES: archive and
import markers from earlier restructuring, and an empty-repository marker.
Section 4 step 1 exists because of repositories like this one. A tag named for
an archive import is not a prior version and does not seed the numbering:

```
gh release list --limit 20
git tag --list
```

If `gh release list` is empty, the release you are cutting is a first release
and section 4 governs it, however many tags `git tag --list` prints.

**No automated core-freshness signal.** Some consumers in this family run a
daily workflow that compares their core pin against core's `main` and opens a
bump pull request when it drifts. Check whether this repository has one rather
than assuming either way:

```
ls .github/workflows
```

Where there is none, a core release produces no signal here at all and the pin
moves only when a person remembers, so checking the pin by hand is part of
every client-go release (section 10).

**Stale host references.** `README.md` carries pipeline and coverage badges
pointing at a GitLab host from this repository's earlier home. Per section 15
the remote is GitHub and `gh` is the tool. Stale badges are decoration left
behind by a migration, not a routing instruction, and should not be read as
evidence that a GitLab pipeline gates this release.
