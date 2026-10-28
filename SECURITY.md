# Security Policy

## Supported Versions

Only the latest released version receives security fixes.

## Reporting a Vulnerability

Please do not open a public issue for security problems. Report them privately through
GitHub's Private Vulnerability Reporting:

1. Open the repository on GitHub.
2. Go to the **Security** tab.
3. Click **Report a vulnerability** and describe the issue.

Include the affected version, steps to reproduce, and the expected and actual behavior. You will
get an acknowledgement, and the report stays private until a fix is released.

## Scope

In scope:

- Bypasses of the origin check in `HTTPPager`, for example a next link or a redirect that sends
  a request to another origin and so receives the credentials of the injected client.
- Bypasses of the guards in `Paginate` (cursor loops, page limit, empty-page loop, time limits)
  that let an iteration run without bound.
- Error messages that contain full URLs or query values.
- Hangs or goroutine leaks that a server can trigger, for example through `Retry-After`.

Out of scope:

- Servers or APIs that return wrong data. The library stops on guard violations, but it cannot
  check the content of the items.
- `fetch` functions that ignore their context. The library returns on time limits, but the
  function itself keeps running.
