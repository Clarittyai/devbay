# The demo that explains devbay in twenty seconds.
#
#   brew install vhs && vhs demo/cookie-jar.tape
#
# Produces demo/cookie-jar.gif. Needs a container runtime running, because it
# boots two real bays -- the point being that nothing here is a mock-up.

Output demo/cookie-jar.gif

Set Shell "sh"
Set FontSize 18
Set Width 1100
Set Height 620
Set Padding 22
Set Theme "Aardvark Blue"
Set TypingSpeed 40ms
# Long enough to read the two lines that matter, short enough to loop well.
Set PlaybackSpeed 1.0

Type "sh demo/cookie-jar.sh"
Sleep 400ms
Enter

# Two bays of a real stack have to boot before anything is proven.
Sleep 30s
Sleep 6s
