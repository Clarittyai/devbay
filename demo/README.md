# demo

Two recordings, scripted rather than performed, so they can be re-made when the
output changes instead of going quietly out of date the way a screenshot does.

```sh
brew install vhs
vhs demo/cookie-jar.tape     # -> demo/cookie-jar.gif
vhs demo/three-bays.tape     # -> demo/three-bays.gif
```

Both boot real bays, so a container runtime has to be running. Run the scripts
on their own first — `sh demo/cookie-jar.sh` — to see what will be recorded, and
to check it still says what the README claims it says.

## cookie-jar

The argument for per-bay hostnames, run for real against two bays of
[`examples/cookie-isolation`](../examples/cookie-isolation).

A cookie jar keys by host and ignores the port, which is the rule a browser
follows. So two bays reached on `127.0.0.1:40160` and `127.0.0.1:41540` share one
session, and the recording shows beta answering with alpha's:

```
bay=beta host=127.0.0.1:41540 cookie=session=alpha-session
```

Reached on their own hostnames instead, the same two bays and the same jar:

```
bay=beta host=beta.cookies.localhost cookie=(none)
```

That is the whole product in two lines of output. Nothing is simulated: the
requests are real, the jar is real, and the second pair differs from the first
only in the address.

## three-bays

Three bays of the four-service [`examples/taskboard`](../examples/taskboard)
stack, created at the same time — the claim people disbelieve is the wall clock,
so the demo is just the thing happening. On a machine with the images already
pulled, all three are serving in about four seconds.

## Keeping the bays

Both scripts tear down what they create. `DEMO_KEEP=1` leaves the bays running,
which is what you want when checking a recording against a browser.
