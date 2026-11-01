# Dependency sweep (2026-09-26)

This research note became the current dependency list:
[docs/reference/dependencies.md](../reference/dependencies.md). The sweep's full text
is in that file's git history.

## Addendum: litellm incidents in 2026

Recorded from a survey of October 2026, with no re-check against the sources. Treat dates,
versions and CVE numbers as leads to confirm. The records change as vendors revise notices.

| When | What | How |
|---|---|---|
| March | Two PyPI releases (1.82.7 and 1.82.8) carried a credential stealer for a few hours. Other packages pulled them in. Version 1.82.6 and earlier were clean. | A compromised scanner in CI leaked the PyPI token. The GitHub source was not changed. |
| April | CVE-2026-42208: SQL injection before authentication, in the proxy's API-key check. Exploited. | Code flaw |
| June | CVE-2026-42271: command injection, listed in CISA's Known Exploited Vulnerabilities catalog. It chains with a Starlette Host-header bypass, CVE-2026-48710. | Code flaw |
| 21 October | Release 1.107.0 on PyPI, and four Docker image tags, carried a credential stealer and a remote code loader. 1.106.2 was clean. The notice was revised on 23 October and the status was still open. No link to March was shown. | Stolen GitHub credentials ran a release workflow aimed at a look-alike repository. The source tag was clean again. |

The release pipeline was hit twice in one year, once after a rebuilt CI. Two code flaws
were exploited in the same year. That is a pattern. A client library for model providers
holds every API key, so one compromise costs all of them. This is why `llm_client.py` is
our own code over the provider interfaces, and why the policy in the
[security model](../security-model.md) pins hashes and waits before adopting releases.

If exposure is suspected: check lockfiles and CI and deploy logs for the bad versions and
their time windows. Then rotate every credential the host could read. Follow the vendor's
own remediation steps and not a summary.
