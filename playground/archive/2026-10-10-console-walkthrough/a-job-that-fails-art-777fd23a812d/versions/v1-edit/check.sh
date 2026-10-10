#!/bin/sh
echo "checking the report inputs"
echo "report.txt would be written here" > report.txt
echo "error: the upstream file is missing" >&2
exit 3
