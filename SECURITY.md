# Security policy

## Supported versions

Only the latest tagged `v0` release receives security fixes while the project
is experimental.

## Reporting a vulnerability

Please use GitHub's private vulnerability reporting for this repository. Do not
open a public issue for suspected token disclosure, cache poisoning, path
handling, resource exhaustion, or dependency vulnerabilities.

Include a minimal reproduction, affected commit, expected impact, and whether
the issue requires a cache-writing trusted workflow. You should receive an
initial response within seven days.

The GitHub Actions cache-v2 runner protocol is an external, non-public
interface. Availability changes in that service are operational failures, not
security vulnerabilities, unless they expose data or authority across GitHub's
documented repository and branch scopes.
