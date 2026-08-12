# cookie-isolation

The smallest application that can demonstrate why a bay gets its own hostname
instead of its own port: it sets a session cookie, and it reports the `Cookie`
header the browser sent it.

```sh
devbay new alpha
devbay new beta
```

Then, in a browser, visit each bay twice — once on the host ports that
`devbay url` prints, and once on the bay hostnames.

Logging in to alpha on `127.0.0.1:<port-a>` and then opening beta on
`127.0.0.1:<port-b>` shows beta reporting **alpha's** session: browsers key
cookies by host and ignore the port, so the two bays share one jar. Doing the
same through `alpha.cookies.localhost` and `beta.cookies.localhost` shows beta
reporting no cookie at all.

That is the whole argument for per-bay origins, and it is the one claim in
[docs/ACCEPTANCE.md](../../docs/ACCEPTANCE.md#the-browser-gate) that has to be
checked by a human, because it is a claim about a browser.
