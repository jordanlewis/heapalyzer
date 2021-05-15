#!/bin/bash

go build
ulimit -c unlimited
GOTRACEBACK=crash ./testprog

dlv core ./testprog ./core --headless -l localhost:7070 --accept-multiclient
