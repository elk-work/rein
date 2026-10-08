# Security

Report a vulnerability in Rein privately, through GitHub: open this
repository's **Security** tab and choose **Report a vulnerability**. The report
goes to the maintainers alone and stays private until a fix is released.

Please do not open a public issue or pull request for it.

A useful report names the version (`rein version`), the platform, and the
steps that reproduce the problem. Rein holds Elk queue tokens and, on a scoped
queue, the credentials it hands each run, so anything that lets a run or
another local user reach a credential it was not given is squarely in scope.

Fixes ship as a new release. Only the latest release is supported.
