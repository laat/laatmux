# Agent instructions for laatmux

## Never reference issues or pull requests in other repositories

Do not write `owner/repo#123`, a GitHub issue or pull request URL, or any other
form of cross-repository issue or PR reference in commit messages, pull request
titles or bodies, issue titles or bodies, or comments in this repository.

GitHub turns such references into backlinks on the referenced issue. laatmux
borrows ideas from other projects (workmux, t3code, herdr); their maintainers
must not see notifications or timeline entries because of work here.

This applies to references made through a link as well as through the short
`#123` form. Referring to a repository itself by URL, for example in a NOTICE
file or a design note, is fine. Referring to one of its issues is not.

If a design decision needs to point at a discussion elsewhere, describe the
idea in your own words and name the project. Do not link the thread.

Issues and pull requests in this repository may be referenced as usual.
