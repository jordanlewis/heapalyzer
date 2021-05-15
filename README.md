This is a work in progress heap analyzer on top of delve.

It really does not work yet!

To play with it, run headless delve on port 7070 on a core. Then, run ./heapalyzer.

The program attempts to aggregate all objects of each type to get a sum of their counts and size.
