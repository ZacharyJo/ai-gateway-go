# Security Policy

## Reporting a Vulnerability

If you discover a security vulnerability, please report it **privately** to the
maintainers instead of opening a public issue, so the fix can be prepared and
released before the details are disclosed.

To report a vulnerability, please open a private advisory via the GitHub
repository's **Security → Report a vulnerability** page, or reach out to the
maintainers directly.

Please include in your report:

- The affected version(s) / commit(s)
- A description of the vulnerability and its impact
- A minimal reproducer (config + request/response) if possible

## Response

- We will acknowledge receipt of the report.
- We will work on a fix and keep you informed of progress.
- Once fixed, we will coordinate disclosure (typically a security release +
  advisory).

## Scope

The proxy is a local service that forwards traffic to third-party upstreams.
Ensure you only run it on machines you trust and only point it at upstreams you
trust, since it relays credentials and content between clients and upstreams.
