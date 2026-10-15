# Contributing to Gitaly

## Developer Certificate of Origin and License

By contributing to GitLab Inc., you accept and agree to the following terms and
conditions for your present and future contributions submitted to GitLab Inc.
Except for the license granted herein to GitLab Inc. and recipients of software
distributed by GitLab Inc., you reserve all right, title, and interest in and to
your Contributions.

All contributions are subject to the
[Developer Certificate of Origin and License](https://docs.gitlab.com/legal/developer_certificate_of_origin/).

_This notice should stay as the first item in the CONTRIBUTING.md file._

## Code of conduct

As contributors and maintainers of this project, we pledge to respect all people
who contribute through reporting issues, posting feature requests, updating
documentation, submitting pull requests or patches, and other activities.

We are committed to making participation in this project a harassment-free
experience for everyone, regardless of level of experience, gender, gender
identity and expression, sexual orientation, disability, personal appearance,
body size, race, ethnicity, age, or religion.

Examples of unacceptable behavior by participants include the use of sexual
language or imagery, derogatory comments or personal attacks, trolling, public
or private harassment, insults, or other unprofessional conduct.

Project maintainers have the right and responsibility to remove, edit, or reject
comments, commits, code, wiki edits, issues, and other contributions that are
not aligned to this Code of Conduct. Project maintainers who do not follow the
Code of Conduct may be removed from the project team.

This code of conduct applies both within project spaces and in public spaces
when an individual is representing the project or its community.

Instances of abusive, harassing, or otherwise unacceptable behavior can be
reported by emailing <contact@gitlab.com>.

This Code of Conduct is adapted from the [Contributor Covenant](https://contributor-covenant.org), version 1.1.0,
available at [https://contributor-covenant.org/version/1/1/0/](https://contributor-covenant.org/version/1/1/0/).

## Style guides

For Gitaly code style, see [`STYLE.md`](STYLE.md).

For help text style guidance for this project, see the [help text style guide](doc/help_text_style_guide.md).

## Commits

In this project we value good commit hygiene. Clean commits makes it much
easier to discover when bugs have been introduced, why changes have been made,
and what their reasoning was.

When you submit a merge request, expect the changes to be reviewed
commit-by-commit. To make it easier for the reviewer, please submit your MR
with nicely formatted commit messages and changes tied together step-by-step.

### Write small, atomic commits

Commits should be as small as possible but not smaller than required to make a
logically complete change. If you struggle to find a proper summary for your
commit message, it's a good indicator that the changes you make in this commit may
not be focused enough.

`git add -p` is useful to add only relevant changes. Often you only notice that
you require additional changes to achieve your goal when halfway through the
implementation. Use `git stash` to help you stay focused on this additional
change until you have implemented it in a separate commit.

### Split up refactors and behavioral changes

Introducing changes in behavior very often requires preliminary refactors. You
should never squash refactoring and behavioral changes into a single commit,
because that makes it very hard to spot the actual change later.

### Tell a story

When splitting up commits into small and logical changes, there will be many
interdependencies between all commits of your feature branch. If you make
changes to simply prepare another change, you should briefly mention the overall
goal that this commit is heading towards.

### Describe why you make changes, not what you change

When writing commit messages, you should typically explain why a given change is
being made. For example, if you have pondered several potential solutions, you
can explain why you settled on the specific implementation you chose. What has
changed is typically visible from the diff itself.

A good commit message answers the following questions:

- What is the current situation?
- Why does that situation need to change?
- How does your change fix that situation?
- Are there relevant resources which help further the understanding? If so,
  provide references.

Be succinct: give each question as much space as it needs and no more. A subtle
change may warrant several paragraphs, while a self-evident one is served by a single
line. What doesn't help either way is restating the diff or padding the reasoning,
which only buries what the reviewer came for.

You may want to set up a [message template](https://thoughtbot.com/blog/better-commit-messages-with-a-gitmessage-template)
to pre-populate your editor when executing `git commit`.

### Use scoped commit subjects

Many projects typically prefix their commit subjects with a scope. For example,
if you're implementing a new feature "X" for subsystem "Y", your commit message
would be "Y: Implement new feature X". This makes it easier to quickly sift
through relevant commits by simply inspecting this prefix.

### Keep the commit subject short

Because commit subjects are displayed in various command line tools by default,
keep the commit subject short. A good rule of thumb is that it shouldn't exceed
72 characters.

### Mention the original commit that introduced bugs

When implementing bugfixes, it's often useful information to see why a bug was
introduced and when it was introduced. Therefore, mentioning the original commit
that introduced a given bug is recommended. You can use `git blame` or `git
bisect` to help you identify that commit.

The format used to mention commits is typically the abbreviated object ID
followed by the commit subject and the commit date. You may create an alias for
this to have it easily available. For example:

```shell
$ git config alias.reference "show -s --pretty=reference"
$ git reference HEAD
cf7f9ffe5 (style: Document best practices for commit hygiene, 2020-11-20)
```

### Use interactive rebases to arrange your commit series

Use interactive rebases to end up with commit series that are readable and
therefore also easily reviewable one-by-one. Use interactive rebases to
rearrange commits, improve their commit messages, or squash multiple commits
into one.

### Create fixup commits

When you create multiple commits as part of feature branches, you
frequently discover bugs in one of the commits you've just written. Instead of
creating a separate commit, you can easily create a fixup commit and squash it
directly into the original source of bugs via `git commit --fixup=ORIG_COMMIT`
and `git rebase --interactive --autosquash`.

### Avoid merge commits

During development other changes might be made to the target branch. These
changes might cause a conflict with your changes. Instead of merging the target
branch into your topic branch, rebase your branch onto the target
branch. Consider setting up `git rerere` to avoid resolving the same conflict
over and over again.

### Ensure that all commits build and pass tests

To keep history bisectable using `git bisect`, you should ensure that all of
your commits build and pass tests. You can do this with interactive rebases, for
example: `git rebase -i --exec='make build format lint test'
origin/master`. This automatically builds each commit and verifies that they
pass formatting, linting, and our test suite.

### Changelog

Gitaly keeps a [changelog](CHANGELOG.md) that is generated:

- When a new release is created.
- From commit messages where a specific trailer is used.

The trailer should have the following format: `Changelog: <option>` where
`<option>` is one of:

- `added`
- `fixed`
- `changed`
- `deprecated`
- `removed`
- `security`
- `performance`
- `other`

The commit title is used to generate a changelog entry.

### Example

A great commit message could look something like:

```plaintext
package: Summarize change in 50 characters or less

The first line of the commit message is the summary. The summary should
start with a capital letter and not end with a period. Optionally
prepend the summary with the package name, feature, file, or piece of
the codebase where the change belongs to.

After an empty line the commit body provides a more detailed explanatory
text. This body is wrapped at 72 characters. The body can consist of
several paragraphs, each separated with a blank line.

The body explains the problem that this commit is solving. Focus on why
you are making this change as opposed to what (the code explains this).
Are there side effects or other counterintuitive consequences of
this change? Here's the place to explain them.

- Bullet points are okay, too

- Typically a hyphen or asterisk is used for the bullet, followed by a
  single space, with blank lines in between

- Use a hanging indent

These guidelines are pretty similar to those described in the Git Book
[1]. If you like you can use footnotes to include a lengthy hyperlink
that would otherwise clutter the text.

You can provide links to the related issue, or the issue that's fixed by
the change at the bottom using a trailer. A trailer is a token, without
spaces, directly followed with a colon and a value. Order of trailers
doesn't matter.

1. https://www.git-scm.com/book/en/v2/Distributed-Git-Contributing-to-a-Project#_commit_guidelines

Fixes: https://gitlab.com/gitlab-org/gitaly/-/issues/123
Changelog: added
Signed-off-by: Alice <alice@example.com>
```

## Gitaly Maintainers

This project is maintained by the [members](https://gitlab.com/groups/gl-gitaly/-/group_members)
of `@gl-gitaly`.

## Development Process

Gitaly follows the engineering process as described in the [handbook][eng-process],
with the exception that our [issue tracker][gitaly-issues] is on the Gitaly
project and there's no distinction between developers and maintainers. Every team
member is equally responsible for a successful master pipeline and fixing security
issues.

Merge requests are approved by [Gitaly team members](https://gitlab.com/groups/gl-gitaly/-/group_members);
[REVIEWING.md](REVIEWING.md) describes how many approvals a change needs and how
reviewers are chosen.

[eng-process]: https://handbook.gitlab.com/handbook/engineering/workflow/
[gitaly-issues]: https://gitlab.com/gitlab-org/gitaly/-/work_items

### Review Process

See [REVIEWING.md](REVIEWING.md).

## Gitaly Developer Quick-start Guide

See the [beginner's guide](doc/beginners_guide.md).

## Debugging

Set `level = "debug"` under `[logging]` in the configuration file to raise the log
level. See [Logging in Gitaly](doc/logging.md) for the other keys and for what
Gitaly logs.

Gitaly re-exports the
[`GIT_TRACE*` environment variables](https://git-scm.com/book/en/v2/Git-Internals-Environment-Variables)
to the Git processes it spawns, so setting `GIT_TRACE=true` in Gitaly's
environment reveals what Git does internally. Git writes that output to standard
error, which Gitaly logs at debug level, so raise the log level first.

For instrumenting a single Git command rather than every one of them, Gitaly has
a Trace2 integration instead. See
[Trace2 integration](doc/trace2_integration.md).

## Metrics and profiling

Gitaly serves Prometheus metrics and the Go `pprof` endpoints on the address
configured as `prometheus_listen_addr`. With Gitaly running from the GDK, that
makes a CPU profile one command away:

```shell
go tool pprof -http=: http://localhost:9236/debug/pprof/profile
```

Swap `profile` for `heap`, `goroutine`, or any other
[pprof profile](https://pkg.go.dev/net/http/pprof) as needed. For what the
metrics themselves are and where they are graphed, see
[Observability](doc/observability.md).
