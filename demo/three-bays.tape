# Three bays of a four-service stack, created at the same time.
#
#   brew install vhs && vhs demo/three-bays.tape
#
# Produces demo/three-bays.gif.

Output demo/three-bays.gif

Set Shell "sh"
Set FontSize 18
Set Width 1100
Set Height 560
Set Padding 22
Set Theme "Aardvark Blue"
Set TypingSpeed 40ms

Type "sh demo/three-bays.sh"
Sleep 400ms
Enter

# The images are already pulled on a machine that has run the example once;
# allow for a build on one that has not.
Sleep 40s
Sleep 5s
